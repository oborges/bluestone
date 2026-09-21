package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/auth"
	"github.com/sonroyaalmerol/go-smb-server/smb/encryption"
	"github.com/sonroyaalmerol/go-smb-server/smb/ntlmssp"
	"github.com/sonroyaalmerol/go-smb-server/smb/signing"
	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

const (
	defaultMaxTransact uint32 = 65536
	defaultMaxRead     uint32 = 1 << 20
	defaultMaxWrite    uint32 = 1 << 20
	defaultMaxCredits  uint32 = 8192

	cleanupCloseTimeout = 5 * time.Second
)

type Server struct {
	addr        string
	authFactory auth.Factory
	shares      []vfs.Share
	shareByName map[string]vfs.Share
	dialect     uint16
	locker      vfs.ByteRangeLocker
	observer    Observer
	limits      Limits
	authGate    AuthGate
	resume      *resumeKeys
	files       *openFiles
	notify      *notifyHub
	leases      *leaseTable
	// leasesEnabled grants leases and level II oplocks (WithLeases).
	leasesEnabled     bool
	leaseBreakTimeout time.Duration
	maxConcurrent     int
	maxTransact       uint32
	maxRead           uint32
	maxWrite          uint32
	maxCredits        uint32
	requireEnc        bool
	log               *slog.Logger
	guid              [16]byte

	mu       sync.Mutex
	listener net.Listener
	conns    map[*conn]struct{}
}

type Option func(*Server)

func WithAddr(addr string) Option { return func(s *Server) { s.addr = addr } }

func WithAuth(f auth.Factory) Option { return func(s *Server) { s.authFactory = f } }

func WithShares(shares ...vfs.Share) Option {
	return func(s *Server) { s.shares = append(s.shares, shares...) }
}

func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

func WithMaxCredits(n uint32) Option { return func(s *Server) { s.maxCredits = n } }

func WithEncryptionRequired() Option { return func(s *Server) { s.requireEnc = true } }

// WithLocker records byte-range locks in l instead of the server's own
// in-memory table, so locks taken over SMB can share a table with locks taken
// over other protocols.
func WithLocker(l vfs.ByteRangeLocker) Option { return func(s *Server) { s.locker = l } }

// obs returns the server's observer, or one that does nothing for a Server
// built without New.
func (s *Server) obs() Observer {
	if s.observer == nil {
		return noopObserver{}
	}
	return s.observer
}

// WithMaxConcurrentRequests bounds how many reads and writes a single
// connection handles at once. 0 selects the default; 1 handles every request
// in turn, as the server did before.
func WithMaxConcurrentRequests(n int) Option {
	return func(s *Server) { s.maxConcurrent = n }
}

func WithDialect(d uint16) Option { return func(s *Server) { s.dialect = d } }

func New(opts ...Option) (*Server, error) {
	s := &Server{
		addr:          ":445",
		dialect:       wire.DialectSMB302,
		locker:        newMemLocker(),
		maxConcurrent: defaultMaxConcurrent,
		maxTransact:   defaultMaxTransact,
		maxRead:       defaultMaxRead,
		maxWrite:      defaultMaxWrite,
		maxCredits:    defaultMaxCredits,
		log:           slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if len(s.shares) == 0 {
		return nil, errors.New("server: at least one share is required")
	}
	if s.authFactory == nil {
		s.authFactory = auth.AlwaysAllowFactory()
	}
	s.shareByName = make(map[string]vfs.Share, len(s.shares))
	for _, sh := range s.shares {
		s.shareByName[sh.Name()] = sh
	}

	if _, ok := s.shareByName["IPC$"]; !ok {
		shareNames := make([][2]string, len(s.shares)+1)
		shareNames[0] = [2]string{"IPC$", "Remote IPC"}
		for i, sh := range s.shares {
			shareNames[i+1] = [2]string{sh.Name(), ""}
		}
		pb := vfs.NewPipeBackend()
		pb.Register("srvsvc", vfs.SrvsvcHandler(shareNames))
		ipcShare := vfs.NewDiskShare("IPC$", pb)
		s.shares = append(s.shares, ipcShare)
		s.shareByName["IPC$"] = ipcShare
	}

	if _, err := rand.Read(s.guid[:]); err != nil {
		return nil, fmt.Errorf("server: generate guid: %w", err)
	}
	return s, nil
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve accepts connections on ln until ctx is cancelled, then waits for
// open connections to finish. It closes ln when it returns.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.listener = nil
		s.mu.Unlock()
		_ = ln.Close()
	}()
	s.log.Info("smb server listening", "addr", ln.Addr().String())

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil
			}
			return fmt.Errorf("server: accept: %w", err)
		}
		wg.Go(func() {
			s.serveConn(ctx, c)
		})
	}
}

func (s *Server) addConn(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		s.conns = make(map[*conn]struct{})
	}
	s.conns[c] = struct{}{}
}

func (s *Server) removeConn(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

func (s *Server) activeConns() []*conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	return conns
}

// Drain stops accepting connections and closes each one once it has nothing
// in flight and has been quiet for idleFor, so a client in the middle of a
// copy finishes it rather than seeing the transfer fail. A copy is many
// requests in a row, so waiting only for a gap between two of them would
// close the connection mid-copy; idleFor is how long a connection must have
// nothing to do before it counts as finished.
//
// Connections still busy when ctx is done are closed anyway, and Drain
// returns ctx's error; otherwise it returns nil once every connection has
// gone. It does not wait for clients to disconnect by themselves: an SMB
// connection lives as long as the share is mounted, so that would never
// finish.
func (s *Server) Drain(ctx context.Context, idleFor time.Duration) error {
	if err := s.Shutdown(); err != nil {
		return err
	}

	const pollInterval = 20 * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		remaining := 0
		for _, c := range s.activeConns() {
			// Nothing being handled, nothing waiting to be written (closing
			// on a queued reply would lose it), and quiet for long enough
			// that the client is not between two requests of one copy.
			idle := c.running.Load() == 0 && c.queued.Load() == 0
			if idle && time.Since(c.lastActiveAt()) >= idleFor {
				c.close()
				continue
			}
			remaining++
		}
		if remaining == 0 && len(s.activeConns()) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			for _, c := range s.activeConns() {
				c.close()
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// lastActiveAt is when the connection last finished a request.
func (c *conn) lastActiveAt() time.Time {
	return time.Unix(0, c.lastActive.Load())
}

// close ends the connection, which stops its read loop and releases what it
// holds.
func (c *conn) close() {
	if underlying := c.fc.Underlying(); underlying != nil {
		_ = underlying.Close()
	}
}

// Shutdown stops the server accepting new connections, leaving the ones it
// has. Drain is the graceful form.
func (s *Server) Shutdown() error {
	s.mu.Lock()
	ln := s.listener
	s.listener = nil
	s.mu.Unlock()
	if ln != nil {
		return ln.Close()
	}
	return nil
}

type session struct {
	auth          auth.Authenticator
	identity      *auth.Identity
	authenticated bool
	// counted records that this session was reported to the observer, so
	// that it is reported once however many SESSION_SETUPs it took, and
	// balanced by exactly one SessionClosed, whether the client logs off or
	// its connection ends.
	counted        atomic.Bool
	mu             sync.RWMutex
	trees          map[uint32]*tree
	nextTreeID     uint32
	signer         *signing.Signer
	requireSign    bool
	encryptionKey  []byte
	decryptionKey  []byte
	requireEncrypt bool
	encCCM         *encryption.AESCCM
	decCCM         *encryption.AESCCM
}

type tree struct {
	share  vfs.Share
	mu     sync.RWMutex
	opens  map[[16]byte]*openHandle
	nextID uint64
}

// open returns an open handle by id.
func (t *tree) open(id [16]byte) (*openHandle, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	oh, ok := t.opens[id]
	return oh, ok
}

// addOpen records a new open handle and returns its id.
// openCount reports how many files the tree holds open.
func (t *tree) openCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.opens)
}

func (t *tree) addOpen(oh *openHandle) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.opens[oh.fileId] = oh
}

// removeOpen forgets an open handle.
// removeOpen forgets an open handle and the resume key naming it.
func (t *tree) removeOpen(id [16]byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.opens, id)
}

// allOpens is a snapshot of the tree's open handles.
func (t *tree) allOpens() []*openHandle {
	t.mu.RLock()
	defer t.mu.RUnlock()
	opens := make([]*openHandle, 0, len(t.opens))
	for _, oh := range t.opens {
		opens = append(opens, oh)
	}
	return opens
}

// nextFileID hands out the next open counter.
func (t *tree) nextFileID() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	return t.nextID
}

type openHandle struct {
	h         vfs.Handle
	fileId    [16]byte
	sessionID uint64
	// access is what the open was granted (MS-SMB2 section 2.2.13.1).
	access uint32
	// deleteOnClose is set when the open was made with
	// FILE_DELETE_ON_CLOSE: closing it marks the file delete-pending.
	deleteOnClose bool
	isDir         bool
	// stream is set on a handle to a named stream rather than a file.
	stream bool
	// lease is the lease or oplock the open caches under, if any; the
	// lease table's lock guards it.
	lease *lease
	// key names the file in the server's table of open files, and changes
	// when the file is renamed. The table's lock guards it.
	key fileKey

	// path is the share-relative name the client used, updated when the
	// file is renamed through any handle; read it with currentPath.
	pathMu sync.Mutex
	path   string

	// watch is the handle's change notification, set up by its first
	// CHANGE_NOTIFY.
	watchMu sync.Mutex
	watch   *watch
	// wrote records that a change to the file has been reported for this
	// handle's writes, so a burst of writes is reported once.
	wrote atomic.Bool
	// io counts reads and writes on this handle still being handled
	// alongside other requests. Only the read loop adds to it and waits on
	// it, so the two never race.
	io sync.WaitGroup

	enumDone bool
	enumMu   sync.Mutex

	// Directory enumeration is paged: clients ask repeatedly, sometimes one
	// entry at a time, until the server reports no more files. The entries
	// are captured on the first call so later calls continue where the
	// previous one stopped.
	enumStarted bool
	enumEntries []vfs.FileInfo
	enumIndex   int
}

// request is one message being handled. It embeds the connection, so
// handlers reach shared state as before, but owns the buffer its response is
// built in: that is what lets several requests be handled at once.
type request struct {
	*conn
	out []byte
	// after runs once out is queued: work that must not reach the client
	// before this response does.
	after []func()
	// concurrent is set for a read or write handled alongside other
	// requests rather than in turn on the read loop.
	concurrent bool
	// compound is set while handling a request that is part of a chain.
	compound bool
	// asyncID is set by a handler that answers with an interim response
	// and completes later: the response goes out as async.
	asyncID uint64
	// retryingCreate marks a CREATE tried again after lease breaks, which
	// does not wait for breaks a second time.
	retryingCreate bool
}

// finish queues the response and runs what waited for it.
func (r *request) finish() {
	if len(r.out) > 0 {
		r.send(r.out)
	}
	for _, fn := range r.after {
		fn()
	}
}

type conn struct {
	srv           *Server
	fc            *transport.FramedConn
	log           *slog.Logger
	sessionsMu    sync.RWMutex
	sessions      map[uint64]*session
	nextSess      uint64
	creditMu      sync.Mutex
	creditBalance uint32

	nextAsync    uint64
	pending      map[uint64]*pendingOp
	pendingByMsg map[uint64]*pendingOp
	pendingMu    sync.Mutex
	outbox       chan []byte
	connDone     chan struct{}
	writerDone   chan struct{}
	// inflight bounds concurrent request handlers, and handlers tracks them
	// so the connection waits for them before closing its open files.
	inflight chan struct{}
	handlers sync.WaitGroup
	// running counts requests being handled right now, and queued counts
	// replies waiting for the writer, so a draining server can tell a
	// connection in the middle of a copy from an idle one, and does not
	// close one whose reply has not reached the wire yet.
	running atomic.Int64
	queued  atomic.Int64
	// lastActive is when this connection last finished a request, so a
	// drain can tell a client in the middle of a copy, which is sending
	// requests back to back, from a mount that is merely open.
	lastActive atomic.Int64

	preauthHash []byte

	// negDialect and negCaps are what NEGOTIATE actually answered on this
	// connection. FSCTL_VALIDATE_NEGOTIATE_INFO must echo them exactly:
	// clients compare the two and drop the connection when they differ.
	negDialect uint16
	negCaps    uint32
	// clientGUID is the client's identity from NEGOTIATE, which scopes the
	// lease keys it chooses.
	clientGUID [16]byte
}

func (s *Server) serveConn(ctx context.Context, c net.Conn) {
	defer func() { _ = c.Close() }()
	s.obs().ConnectionOpened()
	defer s.obs().ConnectionClosed()
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sha := sha512.New()
	sha.Write(nil)
	cn := &conn{
		srv:           s,
		fc:            transport.NewFramedConn(c),
		log:           s.log,
		sessions:      make(map[uint64]*session),
		creditBalance: 1,
		pending:       make(map[uint64]*pendingOp),
		pendingByMsg:  make(map[uint64]*pendingOp),
		outbox:        make(chan []byte, 64),
		connDone:      make(chan struct{}),
		writerDone:    make(chan struct{}),
		inflight:      make(chan struct{}, s.concurrentRequests()),
		preauthHash:   sha.Sum(nil),
	}
	s.addConn(cn)
	defer s.removeConn(cn)
	defer cn.cleanup()
	go cn.writeLoop()

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		_ = cn.fc.Underlying().SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		msg, err := cn.fc.ReadMessage()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if !errors.Is(err, net.ErrClosed) {
				cn.log.Debug("read error", "err", err)
			}
			return
		}
		_ = cn.fc.Underlying().SetReadDeadline(time.Time{})

		if isSMB1Negotiate(msg) {
			// Windows opens with an SMB1 multi-protocol negotiate listing
			// "SMB 2.???". Answering with the wildcard dialect makes it
			// retry in SMB2; ignoring it makes the client hang, then reset.
			r := &request{conn: cn}
			r.replyWildcardNegotiate()
			cn.send(r.out)
			continue
		}

		if len(msg) >= 4 && msg[0] == 0xFD {
			dec, dErr := cn.openTransform(msg)
			if dErr != nil {
				cn.log.Debug("decrypt error", "err", dErr)
				return
			}
			msg = dec
		} else if len(msg) >= wire.HeaderSize && msg[0] == wire.SMB2ProtocolId[0] {
			sessID := binary.LittleEndian.Uint64(msg[40:48])
			if sess := cn.getSession(sessID); sess != nil && sess.requireEncrypt {
				cn.log.Debug("plaintext on encrypted session, dropping")
				return
			}
		}

		cmdBefore := uint16(0xFFFF)
		if len(msg) >= wire.HeaderSize {
			cmdBefore = binary.LittleEndian.Uint16(msg[12:14])
		}
		if cmdBefore == wire.CmdNegotiate || cmdBefore == wire.CmdSessionSetup {
			cn.updatePreauth(msg)
		}
		if cn.canHandleConcurrently(msg) {
			// ReadMessage hands back a buffer it reuses, so a request that
			// outlives this loop iteration needs its own copy.
			queued := append([]byte(nil), msg...)
			select {
			case cn.inflight <- struct{}{}:
			case <-connCtx.Done():
				return
			}
			// Registered on its handle before the next request is read, so
			// a CLOSE right behind it waits for it rather than overtaking.
			release := cn.holdHandle(queued)
			cn.handlers.Add(1)
			cn.running.Add(1)
			go func() {
				defer cn.handlers.Done()
				defer cn.running.Add(-1)
				defer func() { <-cn.inflight }()
				defer release()
				r := &request{conn: cn, concurrent: true}
				r.handleMessage(connCtx, queued)
				r.finish()
			}()
			continue
		}

		cn.running.Add(1)
		r := &request{conn: cn}
		r.handleMessage(connCtx, msg)
		r.finish()
		cn.running.Add(-1)
	}
}

// smb1NegotiateHeader is the start of an SMB1 NEGOTIATE request: the SMB1
// protocol id followed by command 0x72.
var smb1NegotiateHeader = []byte{0xFF, 'S', 'M', 'B', 0x72}

// isSMB1Negotiate reports whether msg is an SMB1 negotiate offering SMB2.
func isSMB1Negotiate(msg []byte) bool {
	return bytes.HasPrefix(msg, smb1NegotiateHeader) && bytes.Contains(msg, []byte("SMB 2."))
}

// replyWildcardNegotiate answers an SMB1 negotiate with an SMB2 negotiate
// response carrying the wildcard dialect, so the client renegotiates in
// SMB2 (MS-SMB2 section 3.3.5.3.1).
func (c *request) replyWildcardNegotiate() {
	hdr := wire.NewHeader(wire.CmdNegotiate)
	hdr.Flags = wire.FlagServerToRedir
	hdr.Credit = 1
	c.out = hdr.Append(c.out)
	resp := wire.NegotiateResponse{
		SecurityMode:    wire.SigningEnabled,
		DialectRevision: wire.DialectWildcard,
		ServerGuid:      c.srv.guid,
		Capabilities:    c.negotiateCapabilities(),
		MaxTransactSize: c.srv.maxTransact,
		MaxReadSize:     c.srv.maxRead,
		MaxWriteSize:    c.srv.maxWrite,
		SecurityBuffer:  ntlmssp.NegTokenInitNTLM(),
	}
	c.out = resp.Append(c.out)
}

// defaultMaxConcurrent is how many reads and writes one connection handles at
// once by default. Requests wait on the object store far more than on the
// gateway, so handling them in turn would leave a client waiting a round trip
// per request.
const defaultMaxConcurrent = 64

// concurrentRequests is the configured bound, with 0 meaning the default.
func (s *Server) concurrentRequests() int {
	if s.maxConcurrent > 0 {
		return s.maxConcurrent
	}
	return defaultMaxConcurrent
}

// canHandleConcurrently reports whether a message can be handled off the read
// loop. Only reads and writes qualify: they use a file handle that already
// exists, while every other request may create or destroy session, tree, or
// handle state, which stays ordered on the read loop. Compound and related
// requests stay there too, because their parts share state.
func (c *conn) canHandleConcurrently(msg []byte) bool {
	if c.srv.concurrentRequests() <= 1 || len(msg) < wire.HeaderSize || msg[0] != wire.SMB2ProtocolId[0] {
		return false
	}
	var hdr wire.Header
	if err := hdr.Parse(msg); err != nil {
		return false
	}
	if hdr.NextCommand != 0 || hdr.Flags&wire.FlagRelatedOps != 0 {
		return false
	}
	if hdr.Command != wire.CmdRead && hdr.Command != wire.CmdWrite {
		return false
	}
	sess := c.getSession(hdr.SessionId)
	return sess != nil && sess.authenticated
}

// writeLoop owns the connection's output: every response is written here, so
// handlers can run in more than one goroutine without interleaving frames.
// A write error closes the connection, which ends the read loop too.
func (c *conn) writeLoop() {
	defer close(c.writerDone)
	for {
		select {
		case resp := <-c.outbox:
			if sealed, ok := c.maybeSealResponse(resp); ok {
				resp = sealed
			}
			err := c.fc.WriteMessage(resp)
			c.queued.Add(-1)
			if err != nil {
				c.log.Debug("write error", "err", err)
				_ = c.fc.Underlying().Close()
				return
			}
		case <-c.connDone:
			return
		}
	}
}

// send queues a response. The buffer must not be modified afterwards.
func (c *conn) send(resp []byte) {
	c.queued.Add(1)
	select {
	case c.outbox <- resp:
	case <-c.connDone:
		c.queued.Add(-1)
	}
}

// sendCopy queues a copy of a response built in a shared buffer.
func (c *conn) sendCopy(resp []byte) {
	if len(resp) == 0 {
		return
	}
	c.send(append([]byte(nil), resp...))
}

func (c *request) handleMessage(ctx context.Context, msg []byte) {
	off := 0
	first := true
	// chainFailed is set when a request in the chain fails its signature
	// check, which fails the rest of the chain.
	chainFailed := false
	lastStatus := uint32(wire.StatusSuccess)
	// createErr is the status of a CREATE that failed. Related requests
	// after it name the file it would have opened, so they fail the same
	// way (MS-SMB2 section 3.3.5.2.7.2); any other failure, such as a READ
	// at end of file, leaves the rest of the chain to run, so its CLOSE
	// still closes the file.
	createErr := uint32(wire.StatusSuccess)
	var lastFileId [16]byte
	prevRespStart := -1
	// Responses are signed once the whole chain is built: appending the next
	// response pads the previous one and sets its NextCommand, and the
	// signature has to cover both (MS-SMB2 3.3.4.1.1).
	type unsigned struct {
		start  int
		signer *signing.Signer
	}
	var toSign []unsigned
	defer func() {
		for i, u := range toSign {
			end := len(c.out)
			if i+1 < len(toSign) {
				end = toSign[i+1].start
			}
			if err := u.signer.Sign(c.out[u.start:end]); err != nil {
				c.log.Debug("sign response failed", "err", err)
			}
		}
	}()

	for off+wire.HeaderSize <= len(msg) {
		sub := msg[off:]
		var hdr wire.Header
		if err := hdr.Parse(sub); err != nil {
			c.log.Debug("bad header in compound", "err", err)
			return
		}
		related := hdr.Flags&wire.FlagRelatedOps != 0 && !first
		if related {
			if fo := fileIdOffset(hdr.Command); fo >= 0 && fo+16 <= len(sub) {
				copy(sub[fo:fo+16], lastFileId[:])
			}
		}

		c.chargeCredits(uint32(hdr.CreditCharge))

		// Requests on one handle take effect in the order they arrive.
		// Reads and writes may run alongside each other, but anything else
		// naming the handle waits for those received before it: a CLOSE
		// that overtook a write would fail it with STATUS_INVALID_HANDLE,
		// and a truncate could land before the write it follows.
		if !c.concurrent {
			if oh := c.handleOf(&hdr, sub); oh != nil {
				oh.io.Wait()
			}
		}

		if sess := c.getSession(hdr.SessionId); sess != nil && sess.signer != nil {
			if hdr.Flags&wire.FlagSigned != 0 {
				ok, vErr := sess.signer.Verify(sub)
				if vErr != nil || !ok {
					chainFailed = true
					lastStatus = wire.StatusAccessDenied
				}
			} else if sess.requireSign {
				chainFailed = true
				lastStatus = wire.StatusAccessDenied
			}
		}

		if hdr.Command == wire.CmdCancel {
			// CANCEL has no response of its own (MS-SMB2 section
			// 3.3.5.16): the request it cancels completes instead.
			c.handleCancel(&hdr)
			c.srv.obs().RequestCompleted(hdr.Command, wire.StatusSuccess, 0)
			first = false
			if hdr.NextCommand == 0 {
				break
			}
			off += int(hdr.NextCommand)
			continue
		}

		if hdr.Command == wire.CmdChangeNotify && !chainFailed {
			sess := c.getSession(hdr.SessionId)
			var trCN *tree
			if sess != nil {
				trCN = sess.getTree(hdr.TreeId)
			}
			status, pending := c.handleChangeNotify(ctx, sub, &hdr, trCN)
			if pending {
				first = false
				if hdr.NextCommand == 0 {
					break
				}
				off += int(hdr.NextCommand)
				continue
			}
			lastStatus = status
		}

		if prevRespStart >= 0 {
			for len(c.out)%8 != 0 {
				c.out = append(c.out, 0)
			}
			delta := uint32(len(c.out) - prevRespStart)
			binary.LittleEndian.PutUint32(c.out[prevRespStart+20:prevRespStart+24], delta)
		}
		respStart := len(c.out)
		c.out = append(c.out, make([]byte, wire.HeaderSize)...)

		var status uint32
		started := time.Now()
		switch {
		case chainFailed:
			status = lastStatus
		case related && createErr != wire.StatusSuccess:
			status = createErr
		default:
			c.compound = hdr.NextCommand != 0 || !first
			status = c.dispatch(ctx, sub, &hdr, &lastFileId, related)
		}
		c.srv.obs().RequestCompleted(hdr.Command, status, time.Since(started))
		c.lastActive.Store(time.Now().UnixNano())
		lastStatus = status
		if hdr.Command == wire.CmdCreate && status != wire.StatusPending {
			createErr = status
		}

		// hdr.Credit still holds what the client asked for.
		hdr.Credit = c.grantCredits(hdr.Credit)

		hdr.Flags |= wire.FlagServerToRedir
		if !first {
			hdr.Flags |= wire.FlagRelatedOps
		}
		hdr.Flags &^= wire.FlagAsyncCommand
		interim := c.asyncID != 0
		if interim {
			// The request completes later; this is its interim response.
			hdr.Flags |= wire.FlagAsyncCommand
			hdr.AsyncId = c.asyncID
			c.asyncID = 0
		}
		hdr.Status = status
		hdr.EncodeAt(c.out[respStart:])

		if hdr.Command == wire.CmdNegotiate || hdr.Command == wire.CmdSessionSetup {
			c.updatePreauth(c.out[respStart:])
		}

		if sess := c.getSession(hdr.SessionId); sess != nil && sess.signer != nil && !interim {
			encrypting := sess.requireEncrypt && hdr.Command != wire.CmdNegotiate && hdr.Command != wire.CmdSessionSetup
			if !encrypting {
				hdr.Flags |= wire.FlagSigned
				binary.LittleEndian.PutUint32(c.out[respStart+16:respStart+20], hdr.Flags)
				toSign = append(toSign, unsigned{start: respStart, signer: sess.signer})
			}
		}

		prevRespStart = respStart
		first = false
		if hdr.NextCommand == 0 {
			break
		}
		off += int(hdr.NextCommand)
	}
}

// handleOf finds the open a request names, if it names one.
func (c *conn) handleOf(hdr *wire.Header, msg []byte) *openHandle {
	fo := fileIdOffset(hdr.Command)
	if fo < 0 || fo+16 > len(msg) {
		return nil
	}
	sess := c.getSession(hdr.SessionId)
	if sess == nil {
		return nil
	}
	tr := sess.getTree(hdr.TreeId)
	if tr == nil {
		return nil
	}
	var fid [16]byte
	copy(fid[:], msg[fo:fo+16])
	oh, _ := tr.open(fid)
	return oh
}

// holdHandle registers a read or write about to be handled alongside other
// requests on the handle it names, returning the function that releases it.
func (c *conn) holdHandle(msg []byte) func() {
	var hdr wire.Header
	if err := hdr.Parse(msg); err != nil {
		return func() {}
	}
	oh := c.handleOf(&hdr, msg)
	if oh == nil {
		return func() {}
	}
	oh.io.Add(1)
	return oh.io.Done
}

// fileIdOffset returns where the FileId sits in a request, so a related
// compound command can inherit the previous command's handle. The offsets
// are the request layouts in MS-SMB2 section 2.2; getting one wrong makes
// the server answer the chained command with STATUS_FILE_CLOSED.
func fileIdOffset(cmd uint16) int {
	switch cmd {
	case wire.CmdClose, wire.CmdFlush, wire.CmdQueryDirectory, wire.CmdLock, wire.CmdIoctl:
		// StructureSize plus 6 bytes of fixed fields.
		return 64 + 8
	case wire.CmdRead, wire.CmdWrite, wire.CmdSetInfo:
		// Read/Write: length and offset first. SetInfo: buffer fields first.
		return 64 + 16
	case wire.CmdQueryInfo:
		// Info class, buffer lengths, additional information, and flags.
		return 64 + 24
	default:
		return -1
	}
}

// chargeCredits deducts what a request costs, never below zero. Every request
// uses at least one credit, including those with a CreditCharge of zero.
func (c *conn) chargeCredits(charge uint32) {
	charge = max(charge, 1)
	c.creditMu.Lock()
	defer c.creditMu.Unlock()
	if c.creditBalance >= charge {
		c.creditBalance -= charge
		return
	}
	c.creditBalance = 0
}

// grantCredits grants the credits a client requested, as far as the
// server's maximum allows, and at least one while the client has none left.
//
// Granting only what was asked matters: topping the client up to the maximum
// in the NEGOTIATE response, which macOS does not count, left it believing it
// had a single credit, so it stalled before every compound request and
// eventually hung.
func (c *conn) grantCredits(requested uint16) uint16 {
	c.creditMu.Lock()
	defer c.creditMu.Unlock()
	grant := uint32(requested)
	if room := c.srv.maxCredits - min(c.creditBalance, c.srv.maxCredits); grant > room {
		grant = room
	}
	if grant == 0 && c.creditBalance == 0 {
		grant = 1
	}
	c.creditBalance += grant
	return uint16(grant)
}

func (c *conn) getSession(id uint64) *session {
	c.sessionsMu.RLock()
	defer c.sessionsMu.RUnlock()
	return c.sessions[id]
}

func (c *conn) putSession(id uint64, sess *session) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	c.sessions[id] = sess
}

func (c *conn) dropSession(id uint64) {
	c.sessionsMu.Lock()
	sess := c.sessions[id]
	delete(c.sessions, id)
	c.sessionsMu.Unlock()
	if sess != nil && sess.counted.CompareAndSwap(true, false) {
		c.srv.obs().SessionClosed()
	}
}

// sessionCount reports how many sessions the connection carries, including
// one still authenticating.
func (c *conn) sessionCount() int {
	c.sessionsMu.RLock()
	defer c.sessionsMu.RUnlock()
	return len(c.sessions)
}

// remoteAddr identifies the client for the authentication gate.
func (c *conn) remoteAddr() string {
	if addr := c.fc.Underlying().RemoteAddr(); addr != nil {
		return addr.String()
	}
	return ""
}

// openCount reports how many files a session holds open across its trees.
func (s *session) openCount() int {
	count := 0
	for _, tr := range s.allTrees() {
		count += tr.openCount()
	}
	return count
}

// treeCount reports how many trees a session has connected.
func (s *session) treeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.trees)
}

// eachSession calls fn for every session on the connection.
func (c *conn) eachSession(fn func(*session)) {
	c.sessionsMu.RLock()
	sessions := make([]*session, 0, len(c.sessions))
	for _, sess := range c.sessions {
		sessions = append(sessions, sess)
	}
	c.sessionsMu.RUnlock()
	for _, sess := range sessions {
		fn(sess)
	}
}

func (c *conn) cleanup() {
	close(c.connDone)
	c.handlers.Wait()
	// Let the writer finish what it has queued, but do not wait on a client
	// that has stopped reading.
	select {
	case <-c.writerDone:
	case <-time.After(cleanupCloseTimeout):
	}
	c.pendingMu.Lock()
	for _, op := range c.pending {
		op.cancel()
	}
	c.pending = nil
	c.pendingByMsg = nil
	c.pendingMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), cleanupCloseTimeout)
	defer cancel()
	c.eachSession(func(sess *session) {
		for _, tr := range sess.allTrees() {
			c.closeAllOpens(ctx, tr)
		}
		// Sessions the client never logged off from end with the connection.
		if sess.counted.CompareAndSwap(true, false) {
			c.srv.obs().SessionClosed()
		}
	})
}

func (c *conn) updatePreauth(msg []byte) {
	if len(c.preauthHash) == 0 {
		return
	}
	sha := sha512.New()
	sha.Write(c.preauthHash)
	sha.Write(msg)
	c.preauthHash = sha.Sum(nil)
}

func (s *session) getTree(id uint32) *tree {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.trees[id]
}

func (s *session) addTree(id uint32, t *tree) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trees[id] = t
}

func (s *session) dropTree(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.trees, id)
}

// allTrees is a snapshot of the session's tree connects.
func (s *session) allTrees() []*tree {
	s.mu.RLock()
	defer s.mu.RUnlock()
	trees := make([]*tree, 0, len(s.trees))
	for _, t := range s.trees {
		trees = append(trees, t)
	}
	return trees
}

// currentPath is the file's share-relative name, following renames.
func (oh *openHandle) currentPath() string {
	oh.pathMu.Lock()
	defer oh.pathMu.Unlock()
	return oh.path
}

func (oh *openHandle) setPath(p string) {
	oh.pathMu.Lock()
	defer oh.pathMu.Unlock()
	oh.path = p
}
