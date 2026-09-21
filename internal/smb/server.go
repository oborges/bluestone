package smb

import (
	"context"
	"encoding/asn1"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/lock"
	"github.com/oborges/bluestone/internal/metrics"
	"github.com/oborges/bluestone/internal/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/auth"
	"github.com/sonroyaalmerol/go-smb-server/smb/kerberos"
	"github.com/sonroyaalmerol/go-smb-server/smb/ntlmssp"
	"github.com/sonroyaalmerol/go-smb-server/smb/server"
	"github.com/sonroyaalmerol/go-smb-server/smb/spnego"
	smbvfs "github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// User is an account allowed to connect to the share. Give it either an NT
// hash or a password; the hash keeps the password itself out of the
// gateway's configuration.
type User struct {
	Name string
	// Password is the account's password. Prefer NTLMHash.
	Password string
	// NTLMHash is the account's NT hash (16 bytes), as
	// bluestone -smb-hash prints it. When set, Password is ignored.
	NTLMHash []byte
	// UID and GID are the owner recorded for files the account creates; 0
	// leaves the default owner.
	UID, GID int
	// Groups name the account's groups, for "@group" in share access lists.
	Groups []string
}

// ServerOptions configures an SMB server.
type ServerOptions struct {
	// Address is the TCP address to listen on, such as ":445".
	Address string
	// ShareName is the share clients connect to when Shares is empty: the
	// whole bucket, for every user.
	ShareName string
	// Shares are the shares served, each a directory of the bucket with its
	// own access rules.
	Shares []ShareOptions
	// KerberosKeytab is the keytab of the gateway's service principal.
	// Setting it lets domain users sign in with Kerberos.
	KerberosKeytab string
	// KerberosMaxClockSkew is how far a client's clock may be off; 0
	// selects five minutes.
	KerberosMaxClockSkew time.Duration
	// IDMap maps users to the owners recorded for files they create; nil
	// leaves every file with the default owner.
	IDMap *IDMap
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
	// Opens is the share-mode table recording which files are open. Pass the
	// gateway's table to share it with other protocols; nil gets one of its
	// own, which still covers every SMB client of this server.
	Opens *lock.ShareTable
	// Locks is the byte-range lock table. Pass the gateway's table so that a
	// lock taken over SMB conflicts with one taken over NFS; nil leaves the
	// SMB server with a table of its own.
	Locks *lock.Manager
	// ConcurrentRequests bounds how many reads and writes one connection
	// handles at once. 0 selects the library default; 1 handles every request
	// in turn.
	ConcurrentRequests int
	// Limits bound what clients can make the server hold. Zero values mean
	// no limit.
	Limits Limits
	// AuthLimits slow down repeated authentication failures from one client
	// address. Zero values select the defaults.
	AuthLimits AuthLimits
	// DrainTimeout is how long Stop waits for requests in flight to finish
	// before closing the connections carrying them. 0 selects the default.
	DrainTimeout time.Duration
	// MaxStreamBytes caps a file's named streams, names and contents
	// together; 0 selects config.DefaultMaxStreamBytes.
	MaxStreamBytes int
	// Leases lets clients cache the files they read (see
	// config.SMBConfig.Leases).
	Leases bool
	// DurableHandles keeps open files through a dropped connection (see
	// config.SMBConfig.DurableHandles).
	DurableHandles bool
	// MaxDialect is the highest dialect offered, "3.1.1" or "3.0.2"; empty
	// selects 3.1.1.
	MaxDialect string
	// Logger receives server logs; nil discards them.
	Logger *zap.Logger
}

// Limits bound what one client can make the server hold. Zero means no
// limit; the gateway's configuration supplies defaults.
type Limits struct {
	// Connections caps connections to the server, and ConnectionsPerClient
	// caps those from one client address.
	Connections          int
	ConnectionsPerClient int
	// SessionsPerConnection caps authenticated sessions on one connection.
	SessionsPerConnection int
	// TreesPerSession caps share connections on one session.
	TreesPerSession int
	// OpensPerSession caps the files one session holds open.
	OpensPerSession int
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
	observer *observer
	opens    *lock.ShareTable
	gate     *authGate
	drain    time.Duration
}

// DefaultDrainTimeout is how long Stop waits for clients to finish. Long
// enough for a large write to the object store to complete, short enough
// that a restart is not held up by a client that has stopped responding.
const DefaultDrainTimeout = 30 * time.Second

// drainIdleFor is how long a connection must have nothing to do before the
// drain counts it as finished. A client copying a file sends requests back
// to back with small gaps; closing during one of those gaps would fail the
// copy, which is what draining is meant to avoid.
const drainIdleFor = time.Second

// NewServer creates an SMB server for fs and binds its listener. Give it a
// view with Windows naming, labelled for SMB metrics.
func NewServer(fs *vfs.Filesystem, opts ServerOptions) (*Server, error) {
	if len(opts.Users) == 0 && opts.KerberosKeytab == "" {
		return nil, errors.New("smb: at least one user, or a Kerberos keytab, is required")
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

	obs := &observer{}
	gate := newAuthGate(opts.AuthLimits, logger)
	drainTimeout := opts.DrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = DefaultDrainTimeout
	}
	authOpts, err := authOptions(opts, logger)
	if err != nil {
		return nil, err
	}
	shares := opts.Shares
	if len(shares) == 0 {
		shares = []ShareOptions{{Name: opts.ShareName}}
	}
	opens := opts.Opens
	if opens == nil {
		opens = lock.NewShareTable(lock.ShareOptions{})
	}
	var smbShares []smbvfs.Share
	for _, share := range shares {
		backend := newShareBackend(fs, share.Path, opens, logger)
		backend.maxStreamBytes = opts.MaxStreamBytes
		backend.ids = opts.IDMap
		backend.locks = opts.Locks
		smbShares = append(smbShares, smbvfs.NewDiskShare(share.Name, backend))
	}
	rules := newAccessRules(opts.Domain, opts.Users, shares)
	serverOpts := append(authOpts,
		server.WithShares(smbShares...),
		server.WithShareAccess(rules.access),
		server.WithLogger(slog.New(zapHandler{logger: logger})),
		server.WithObserver(obs),
		server.WithAuthGate(gate),
		server.WithLimits(server.Limits{
			SessionsPerConnection: opts.Limits.SessionsPerConnection,
			OpensPerSession:       opts.Limits.OpensPerSession,
			TreesPerSession:       opts.Limits.TreesPerSession,
		}),
	)
	if opts.EncryptionRequired {
		serverOpts = append(serverOpts, server.WithEncryptionRequired())
	}
	if opts.MaxDialect == "3.0.2" {
		serverOpts = append(serverOpts, server.WithDialect(wire.DialectSMB302))
	}
	if opts.Leases {
		serverOpts = append(serverOpts, server.WithLeases(0))
		if opts.DurableHandles {
			serverOpts = append(serverOpts, server.WithDurableHandles())
		}
	}
	if opts.ConcurrentRequests > 0 {
		serverOpts = append(serverOpts, server.WithMaxConcurrentRequests(opts.ConcurrentRequests))
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
		listener: newConnListener(inner, allowed, logger, opts.Limits.Connections, opts.Limits.ConnectionsPerClient),
		logger:   logger,
		observer: obs,
		opens:    opens,
		gate:     gate,
		drain:    drainTimeout,
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
	go s.publishSampledStats()
	go func() {
		defer close(s.done)
		if err := s.srv.Serve(s.ctx, s.listener); err != nil {
			s.logger.Error("SMB server error", zap.Error(err))
		}
	}()
	return nil
}

// Stop drains the server: it stops accepting clients, lets requests already
// in flight finish, and closes each connection as soon as it is idle. A
// client copying a file finishes the copy rather than seeing it fail.
// Connections still busy after DrainTimeout are closed anyway, so a client
// that has stopped responding cannot hold up a restart. It is safe to call
// more than once.
func (s *Server) Stop() error {
	return s.stop(s.drain)
}

// StopNow stops without draining, for when serving even one more request
// would be wrong: the gateway has lost the bucket lease, so another gateway
// may already be writing to it.
func (s *Server) StopNow() error {
	return s.stop(0)
}

func (s *Server) stop(drain time.Duration) error {
	s.stopOnce.Do(func() {
		s.logger.Info("Stopping SMB server", zap.Duration("drain_timeout", drain))

		// Drain stops the listener itself, so it runs before anything else
		// closes it: a closed listener would make the drain give up at once
		// and cut off the clients it is meant to let finish.
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), drain)
		defer cancel()
		if err := s.srv.Drain(ctx, drainIdleFor); err != nil {
			stats := s.Stats()
			s.logger.Warn("SMB clients were still busy when the drain timed out; closing their connections",
				zap.Duration("waited", time.Since(started)),
				zap.Int("connections", stats.Connections))
		}

		// The drain closes the connections; cancelling stops the accept loop
		// and anything still waiting on the connection context, and the
		// listener close covers a server that was never started.
		s.cancel()
		_ = s.listener.Close()
		s.listener.closeConns()
		if s.started.Load() {
			<-s.done
		}
		s.logger.Info("SMB server stopped", zap.Duration("took", time.Since(started)))
	})
	return nil
}

// publishSampledStats samples the counts that are held elsewhere rather than
// reported as they change: files clients hold open, and client addresses
// blocked from authenticating. Sampling the blocked clients also drops the
// records of clients that have gone away.
func (s *Server) publishSampledStats() {
	const interval = 10 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			metrics.SetSMBOpenFiles(0)
			metrics.SetSMBAuthBlockedClients(0)
			return
		case <-ticker.C:
			if s.opens != nil {
				metrics.SetSMBOpenFiles(s.opens.Len())
			}
			metrics.SetSMBAuthBlockedClients(s.gate.blockedClients())
		}
	}
}

// Running reports whether the server is serving clients: it has been started
// and its accept loop has not returned, whether from Stop or an error.
func (s *Server) Running() bool {
	if !s.started.Load() {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Address returns the server's listening address.
func (s *Server) Address() string {
	return s.listener.Addr().String()
}

// authOptions configures sign-in: NTLM for local accounts, Kerberos for
// domain accounts, or both, with the NEGOTIATE response advertising what
// is on offer so Windows knows to try Kerberos.
func authOptions(opts ServerOptions, logger *zap.Logger) ([]server.Option, error) {
	var ntlm, krb auth.Factory
	if len(opts.Users) > 0 {
		ntlm = ntlmssp.NewServer(newCredentials(opts.Users), opts.Domain)
	}
	if opts.KerberosKeytab == "" {
		return []server.Option{server.WithAuth(ntlm)}, nil
	}
	kt, err := kerberos.LoadKeytab(opts.KerberosKeytab)
	if err != nil {
		return nil, fmt.Errorf("smb: %w", err)
	}
	skew := opts.KerberosMaxClockSkew
	if skew <= 0 {
		skew = 5 * time.Minute
	}
	krb = kerberos.NewServer(kt, kerberos.WithMaxClockSkew(skew))
	logger.Info("SMB Kerberos sign-in enabled", zap.Strings("principals", kerberos.Principals(kt)))
	mechs := []asn1.ObjectIdentifier{spnego.OIDMSKRB5, spnego.OIDKRB5}
	if ntlm != nil {
		mechs = append(mechs, spnego.OIDNTLM)
	}
	return []server.Option{
		server.WithAuth(spnego.Negotiate(krb, ntlm)),
		server.WithNegotiateHint(spnego.NegTokenInit(mechs...)),
	}, nil
}

// credentials looks up NTLM keys for configured users. The NTLMv2 key mixes
// in the username and domain the client sent, so it is derived per login:
// usernames match case-insensitively and any client domain is accepted, as
// Windows clients send their own workgroup or domain name.
//
// Only the NT hash of each password is kept, so the configured password is
// not held in memory for the life of the process. The hash still
// authenticates as the password does, so it is no less of a secret.
type credentials struct {
	hashes map[string][]byte
}

func newCredentials(users []User) credentials {
	hashes := make(map[string][]byte, len(users))
	for _, user := range users {
		hash := user.NTLMHash
		if len(hash) == 0 {
			hash = ntlmssp.NTHash(user.Password)
		}
		hashes[strings.ToLower(user.Name)] = hash
	}
	return credentials{hashes: hashes}
}

// LookupNTOWFv2 implements ntlmssp.CredentialLookup.
func (c credentials) LookupNTOWFv2(_ context.Context, domain, user string) ([]byte, error) {
	hash, ok := c.hashes[strings.ToLower(user)]
	if !ok {
		return nil, ntlmssp.ErrUnknownUser
	}
	return ntlmssp.NTOWFv2FromHash(hash, user, domain), nil
}

// connListener drops connections from outside the allowlist at accept, the
// same way the NFS listener does, and tracks accepted connections so Stop
// can close them.
type connListener struct {
	net.Listener
	allowed []*net.IPNet
	logger  *zap.Logger
	// maxConns caps connections to the server, and maxConnsPerClient caps
	// those from one address, so one client cannot use them all up. Zero
	// means no limit.
	maxConns          int
	maxConnsPerClient int

	mu        sync.Mutex
	closed    bool
	conns     map[*trackedConn]struct{}
	perClient map[string]int
}

func newConnListener(inner net.Listener, allowed []*net.IPNet, logger *zap.Logger, maxConns, maxConnsPerClient int) *connListener {
	return &connListener{
		Listener:          inner,
		allowed:           allowed,
		logger:            logger,
		maxConns:          maxConns,
		maxConnsPerClient: maxConnsPerClient,
		conns:             make(map[*trackedConn]struct{}),
		perClient:         make(map[string]int),
	}
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
		client := clientKey(conn.RemoteAddr())
		tracked := &trackedConn{Conn: conn, owner: l, client: client}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = conn.Close()
			return nil, net.ErrClosed
		}
		if reason := l.overLimitLocked(client); reason != "" {
			l.mu.Unlock()
			l.logger.Warn("Refused SMB connection over limit",
				zap.String("remote_addr", conn.RemoteAddr().String()),
				zap.String("limit", reason))
			metrics.RecordSMBConnectionRefused(reason)
			_ = conn.Close()
			continue
		}
		l.conns[tracked] = struct{}{}
		l.perClient[client]++
		l.mu.Unlock()
		return tracked, nil
	}
}

// overLimitLocked names the limit a new connection would exceed, or "" when
// it fits. The caller holds l.mu.
func (l *connListener) overLimitLocked(client string) string {
	if l.maxConns > 0 && len(l.conns) >= l.maxConns {
		return "max_connections"
	}
	if l.maxConnsPerClient > 0 && l.perClient[client] >= l.maxConnsPerClient {
		return "max_connections_per_client"
	}
	return ""
}

// clientKey identifies the client a connection came from, by address without
// its port, so a client's connections are counted together.
func clientKey(addr net.Addr) string {
	if tcpAddr, ok := addr.(*net.TCPAddr); ok && tcpAddr.IP != nil {
		return tcpAddr.IP.String()
	}
	if addr == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
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
	owner  *connListener
	client string
	once   sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.owner.mu.Lock()
		delete(c.owner.conns, c)
		if n := c.owner.perClient[c.client] - 1; n > 0 {
			c.owner.perClient[c.client] = n
		} else {
			delete(c.owner.perClient, c.client)
		}
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
