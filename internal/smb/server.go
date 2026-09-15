package smb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/ntlmssp"
	"github.com/sonroyaalmerol/go-smb-server/smb/server"
	smbvfs "github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// User is an account allowed to connect to the share.
type User struct {
	Name     string
	Password string
}

// ServerOptions configures an SMB server.
type ServerOptions struct {
	// Address is the TCP address to listen on, such as ":445".
	Address string
	// ShareName is the share clients connect to.
	ShareName string
	// Domain is the NTLM domain and server name the server advertises.
	Domain string
	// Users are the accounts allowed to connect. Usernames match
	// case-insensitively, and the domain a client sends is accepted as is.
	Users []User
	// AllowedClients restricts which client addresses may connect (CIDRs or
	// single IPs). Empty allows all clients.
	AllowedClients []string
	// EncryptionRequired rejects sessions that do not encrypt traffic.
	EncryptionRequired bool
	// Logger receives server logs; nil discards them.
	Logger *zap.Logger
}

// Server serves one share of a filesystem view over SMB.
type Server struct {
	srv      *server.Server
	listener *connListener
	logger   *zap.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	started  atomic.Bool
	done     chan struct{}
	stopOnce sync.Once
}

// NewServer creates an SMB server for fs and binds its listener. Give it a
// view with Windows naming, labelled for SMB metrics.
func NewServer(fs *vfs.Filesystem, opts ServerOptions) (*Server, error) {
	if len(opts.Users) == 0 {
		return nil, errors.New("smb: at least one user is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.With(zap.String("component", "smb"))

	var allowed []*net.IPNet
	if len(opts.AllowedClients) > 0 {
		networks, err := config.ParseClientRules(opts.AllowedClients)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed_clients: %w", err)
		}
		allowed = networks
	}

	serverOpts := []server.Option{
		server.WithShares(smbvfs.NewDiskShare(opts.ShareName, NewBackend(fs))),
		server.WithAuth(ntlmssp.NewServer(newCredentials(opts.Users), opts.Domain)),
		server.WithLogger(slog.New(zapHandler{logger: logger})),
	}
	if opts.EncryptionRequired {
		serverOpts = append(serverOpts, server.WithEncryptionRequired())
	}
	srv, err := server.New(serverOpts...)
	if err != nil {
		return nil, fmt.Errorf("smb: %w", err)
	}

	inner, err := net.Listen("tcp", opts.Address)
	if err != nil {
		return nil, fmt.Errorf("failed to create listener: %w", err)
	}
	if allowed != nil {
		logger.Info("SMB client allowlist enabled", zap.Strings("allowed_clients", opts.AllowedClients))
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		srv:      srv,
		listener: newConnListener(inner, allowed, logger),
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}, nil
}

// Start begins serving SMB clients.
func (s *Server) Start() error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("smb: server already started")
	}
	s.logger.Info("Starting SMB server", zap.String("address", s.Address()))
	go func() {
		defer close(s.done)
		if err := s.srv.Serve(s.ctx, s.listener); err != nil {
			s.logger.Error("SMB server error", zap.Error(err))
		}
	}()
	return nil
}

// Stop stops accepting clients, closes open connections, and waits for
// their requests to finish. It is safe to call more than once.
func (s *Server) Stop() error {
	s.stopOnce.Do(func() {
		s.logger.Info("Stopping SMB server")
		s.cancel()
		_ = s.listener.Close()
		// Closing the context does not interrupt a connection waiting for
		// its next request, so close the connections too.
		s.listener.closeConns()
		if s.started.Load() {
			<-s.done
		}
		s.logger.Info("SMB server stopped")
	})
	return nil
}

// Address returns the server's listening address.
func (s *Server) Address() string {
	return s.listener.Addr().String()
}

// credentials looks up NTLM keys for configured users. The NTLMv2 key mixes
// in the username and domain the client sent, so it is derived per login:
// usernames match case-insensitively and any client domain is accepted, as
// Windows clients send their own workgroup or domain name.
type credentials struct {
	passwords map[string]string
}

func newCredentials(users []User) credentials {
	passwords := make(map[string]string, len(users))
	for _, user := range users {
		passwords[strings.ToLower(user.Name)] = user.Password
	}
	return credentials{passwords: passwords}
}

// LookupNTOWFv2 implements ntlmssp.CredentialLookup.
func (c credentials) LookupNTOWFv2(_ context.Context, domain, user string) ([]byte, error) {
	password, ok := c.passwords[strings.ToLower(user)]
	if !ok {
		return nil, ntlmssp.ErrUnknownUser
	}
	return ntlmssp.NTOWFv2(password, user, domain), nil
}

// connListener drops connections from outside the allowlist at accept, the
// same way the NFS listener does, and tracks accepted connections so Stop
// can close them.
type connListener struct {
	net.Listener
	allowed []*net.IPNet
	logger  *zap.Logger

	mu     sync.Mutex
	closed bool
	conns  map[*trackedConn]struct{}
}

func newConnListener(inner net.Listener, allowed []*net.IPNet, logger *zap.Logger) *connListener {
	return &connListener{Listener: inner, allowed: allowed, logger: logger, conns: make(map[*trackedConn]struct{})}
}

func (l *connListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !l.allowedAddr(conn.RemoteAddr()) {
			l.logger.Error("Rejected SMB connection from address outside allowed_clients",
				zap.String("remote_addr", conn.RemoteAddr().String()))
			_ = conn.Close()
			continue
		}
		tracked := &trackedConn{Conn: conn, owner: l}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = conn.Close()
			return nil, net.ErrClosed
		}
		l.conns[tracked] = struct{}{}
		l.mu.Unlock()
		return tracked, nil
	}
}

func (l *connListener) allowedAddr(addr net.Addr) bool {
	if len(l.allowed) == 0 {
		return true
	}
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok || tcpAddr.IP == nil {
		return false
	}
	for _, network := range l.allowed {
		if network.Contains(tcpAddr.IP) {
			return true
		}
	}
	return false
}

// closeConns closes every open connection and refuses new ones.
func (l *connListener) closeConns() {
	l.mu.Lock()
	l.closed = true
	conns := make([]*trackedConn, 0, len(l.conns))
	for conn := range l.conns {
		conns = append(conns, conn)
	}
	l.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

type trackedConn struct {
	net.Conn
	owner *connListener
	once  sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.owner.mu.Lock()
		delete(c.owner.conns, c)
		c.owner.mu.Unlock()
	})
	return c.Conn.Close()
}

// zapHandler sends the SMB library's slog output to the gateway's logger.
type zapHandler struct {
	logger *zap.Logger
	fields []zap.Field
	group  string
}

func (h zapHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.logger.Core().Enabled(zapLevel(level))
}

func (h zapHandler) Handle(_ context.Context, record slog.Record) error {
	entry := h.logger.Check(zapLevel(record.Level), record.Message)
	if entry == nil {
		return nil
	}
	fields := slices.Clone(h.fields)
	record.Attrs(func(attr slog.Attr) bool {
		fields = append(fields, h.field(attr))
		return true
	})
	entry.Write(fields...)
	return nil
}

func (h zapHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := h
	next.fields = slices.Clone(h.fields)
	for _, attr := range attrs {
		next.fields = append(next.fields, h.field(attr))
	}
	return next
}

func (h zapHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := h
	if h.group != "" {
		name = h.group + "." + name
	}
	next.group = name
	return next
}

func (h zapHandler) field(attr slog.Attr) zap.Field {
	key := attr.Key
	if h.group != "" {
		key = h.group + "." + key
	}
	return zap.Any(key, attr.Value.Resolve().Any())
}

func zapLevel(level slog.Level) zapcore.Level {
	switch {
	case level >= slog.LevelError:
		return zapcore.ErrorLevel
	case level >= slog.LevelWarn:
		return zapcore.WarnLevel
	case level >= slog.LevelInfo:
		return zapcore.InfoLevel
	default:
		return zapcore.DebugLevel
	}
}
