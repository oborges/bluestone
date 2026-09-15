package nfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"time"
)

// Server is a handle to the listening NFS server.
type Server struct {
	Handler
	ID [8]byte
	// EnabledNFSVersions limits NFS program versions served by this server.
	// Empty means all registered versions are enabled.
	EnabledNFSVersions []uint32
	// ConcurrentHandlers bounds how many requests are processed in parallel
	// per connection (Linux clients multiplex a whole mount over one TCP
	// connection). Zero selects the default. RPC replies carry XIDs, so
	// out-of-order completion is legal; NFSv4.0 clients are required to
	// serialize seqid-ordered state operations per owner themselves
	// (RFC 7530 section 9.1.7).
	ConcurrentHandlers int
	context.Context

	// Locker holds the byte ranges behind NFSv4 LOCK, LOCKT and LOCKU.
	// Supply a table shared with other protocol servers so locks conflict
	// across protocols. Nil disables NFSv4 locking (NFS4ERR_NOTSUPP).
	Locker ByteRangeLocker

	// NFSv4 lock protocol state (stateids, seqids, client leases), created
	// on first LOCK-family or RENEW operation.
	lockMgr     *nfs4LockManager
	lockMgrOnce sync.Once
}

// RegisterMessageHandler registers a handler for a specific NFSv3/MOUNTv3
// XDR procedure.
func RegisterMessageHandler(protocol uint32, proc uint32, handler HandleFunc) error {
	return RegisterVersionedMessageHandler(protocol, 3, proc, handler)
}

// RegisterVersionedMessageHandler registers a handler for a specific RPC
// program, version, and procedure.
func RegisterVersionedMessageHandler(protocol uint32, version uint32, proc uint32, handler HandleFunc) error {
	if registeredHandlers == nil {
		registeredHandlers = make(map[registeredHandlerID]HandleFunc)
	}
	for k := range registeredHandlers {
		if k.protocol == protocol && k.version == version && k.proc == proc {
			return errors.New("already registered")
		}
	}
	id := registeredHandlerID{protocol, version, proc}
	registeredHandlers[id] = handler
	return nil
}

// HandleFunc represents a handler for a specific protocol message.
type HandleFunc func(ctx context.Context, w *response, userHandler Handler) error

// TODO: store directly as a uint64 for more efficient lookups
type registeredHandlerID struct {
	protocol uint32
	version  uint32
	proc     uint32
}

var registeredHandlers map[registeredHandlerID]HandleFunc

// Serve listens on the provided listener port for incoming client requests.
func (s *Server) Serve(l net.Listener) error {
	defer l.Close()
	baseCtx := context.Background()
	if s.Context != nil {
		baseCtx = s.Context
	}
	if bytes.Equal(s.ID[:], []byte{0, 0, 0, 0, 0, 0, 0, 0}) {
		if _, err := rand.Reader.Read(s.ID[:]); err != nil {
			return err
		}
	}

	var tempDelay time.Duration

	for {
		conn, err := l.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				time.Sleep(tempDelay)
				continue
			}
			return err
		}
		tempDelay = 0
		c := s.newConn(conn)
		go c.serve(baseCtx)
	}
}

func (s *Server) newConn(nc net.Conn) *conn {
	c := &conn{
		Server: s,
		Conn:   nc,
	}
	return c
}

// TODO: keep an immutable map for each server instance to have less
// chance of races.
func (s *Server) handlerFor(prog uint32, version uint32, proc uint32) HandleFunc {
	if (prog == nfsServiceID || prog == mountServiceID) && !s.nfsVersionAllowed(prog, version) {
		return nil
	}
	for k, v := range registeredHandlers {
		if k.protocol == prog && k.version == version && k.proc == proc {
			return v
		}
	}
	return nil
}

func (s *Server) nfsVersionAllowed(prog uint32, version uint32) bool {
	if len(s.EnabledNFSVersions) == 0 {
		return true
	}
	if prog == mountServiceID {
		version = 3
	}
	for _, enabled := range s.EnabledNFSVersions {
		if enabled == version {
			return true
		}
	}
	return false
}

// Serve is a singleton listener paralleling http.Serve
func Serve(l net.Listener, handler Handler) error {
	srv := &Server{Handler: handler}
	return srv.Serve(l)
}
