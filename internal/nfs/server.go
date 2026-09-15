package nfs

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/oborges/bluestone/internal/logging"
	nfs "github.com/willscott/go-nfs"
)

// Server wraps the NFS server functionality
type Server struct {
	handler            nfs.Handler
	listener           net.Listener
	logger             *logging.KVLogger
	nfsVersions        []uint32
	concurrentHandlers int
	locker             nfs.ByteRangeLocker
	wg                 sync.WaitGroup
	ctx                context.Context
	cancel             context.CancelFunc
}

// ServerOptions carries optional NFS server behavior from configuration.
type ServerOptions struct {
	// AllowedClients restricts which client addresses may connect
	// (CIDRs or single IPs). Empty allows all clients.
	AllowedClients []string
	// ConcurrentHandlers bounds per-connection request parallelism.
	// 0 selects the built-in default; 1 restores serial handling.
	ConcurrentHandlers int
	// Locker backs NFSv4 byte-range locks. Pass NewLocker with the shared
	// lock table so locks conflict across protocols; nil disables NFSv4
	// locking.
	Locker nfs.ByteRangeLocker
}

// NewServer creates a new NFS server
func NewServer(handler nfs.Handler, address string, logger *logging.KVLogger, nfsVersions []uint32, opts ServerOptions) (*Server, error) {
	filter, err := NewClientFilter(opts.AllowedClients)
	if err != nil {
		return nil, fmt.Errorf("invalid allowed_clients: %w", err)
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("failed to create listener: %w", err)
	}
	if filter != nil {
		logger.Info("NFS client allowlist enabled",
			"allowed_clients", opts.AllowedClients)
	}
	listener = newFilteredListener(listener, filter, logger)

	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		handler:            handler,
		listener:           listener,
		logger:             logger,
		nfsVersions:        nfsVersions,
		concurrentHandlers: opts.ConcurrentHandlers,
		locker:             opts.Locker,
		ctx:                ctx,
		cancel:             cancel,
	}, nil
}

// Start begins serving NFS requests
func (s *Server) Start() error {
	concurrency := "default"
	if s.concurrentHandlers > 0 {
		concurrency = fmt.Sprintf("%d", s.concurrentHandlers)
	}
	s.logger.Info("Starting NFS server",
		"address", s.listener.Addr().String(),
		"versions", s.nfsVersions,
		"concurrent_handlers", concurrency)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		srv := &nfs.Server{
			Handler:            s.handler,
			EnabledNFSVersions: s.nfsVersions,
			ConcurrentHandlers: s.concurrentHandlers,
			Locker:             s.locker,
		}
		if err := srv.Serve(s.listener); err != nil {
			s.logger.Error("NFS server error", "error", err)
		}
	}()

	return nil
}

// Stop gracefully stops the NFS server
func (s *Server) Stop() error {
	s.logger.Info("Stopping NFS server")

	// Cancel context to signal shutdown
	s.cancel()

	// Close the listener to stop accepting new connections
	if err := s.listener.Close(); err != nil {
		s.logger.Error("Error closing listener", "error", err)
	}

	// Wait for all goroutines to finish
	s.wg.Wait()

	s.logger.Info("NFS server stopped")
	return nil
}

// Address returns the server's listening address
func (s *Server) Address() string {
	return s.listener.Addr().String()
}

// Made with Bob
