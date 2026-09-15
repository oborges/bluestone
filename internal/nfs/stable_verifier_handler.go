package nfs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io/fs"
	"net"
	"sync"

	"github.com/go-git/go-billy/v5"
	"github.com/oborges/bluestone/internal/logging"
	gonfs "github.com/willscott/go-nfs"
)

// StableVerifierHandler wraps a CachingHandler to provide stable verifiers
// This prevents BadCookie errors that cause clients to restart enumeration
type StableVerifierHandler struct {
	handler   gonfs.Handler
	verifiers sync.Map // map[string]uint64 - path -> stable verifier
	logger    *logging.KVLogger
}

// NewStableVerifierHandler creates a handler that returns stable verifiers per directory
func NewStableVerifierHandler(handler gonfs.Handler, logger *logging.KVLogger) gonfs.Handler {
	return &StableVerifierHandler{
		handler: handler,
		logger:  logger,
	}
}

// VerifierFor returns a stable verifier for a directory path
// Unlike the default implementation, this returns the SAME verifier every time
// for the same path, preventing BadCookie errors during pagination
func (h *StableVerifierHandler) VerifierFor(path string, contents []fs.FileInfo) uint64 {
	// Check if we already have a verifier for this path
	if v, ok := h.verifiers.Load(path); ok {
		verifier := v.(uint64)
		h.logger.Info("STABLE VERIFIER: Reusing verifier",
			"path", path,
			"verifier", verifier,
			"entries", len(contents))
		return verifier
	}

	// Generate a stable verifier based on path only (not contents)
	// This ensures the same verifier is returned even if contents change slightly
	vHash := sha256.New()
	vHash.Write([]byte(path))
	verify := vHash.Sum(nil)[0:8]
	verifier := binary.BigEndian.Uint64(verify)

	// Store for future use
	h.verifiers.Store(path, verifier)

	h.logger.Info("STABLE VERIFIER: Generated new verifier",
		"path", path,
		"verifier", verifier,
		"entries", len(contents))

	return verifier
}

// DataForVerifier checks if we have cached data for a verifier
// Since we use stable verifiers, we delegate to the wrapped handler
func (h *StableVerifierHandler) DataForVerifier(path string, verifier uint64) []fs.FileInfo {
	h.logger.Info("STABLE VERIFIER: DataForVerifier called",
		"path", path,
		"requested_verifier", verifier)
	
	// Check if this is our stable verifier for this path
	if v, ok := h.verifiers.Load(path); ok {
		storedVerifier := v.(uint64)
		h.logger.Info("STABLE VERIFIER: Comparing verifiers",
			"path", path,
			"stored", storedVerifier,
			"requested", verifier,
			"match", storedVerifier == verifier)
			
		if storedVerifier == verifier {
			// Verifier matches - delegate to wrapped handler if it's a CachingHandler
			if ch, ok := h.handler.(gonfs.CachingHandler); ok {
				data := ch.DataForVerifier(path, verifier)
				if data != nil {
					h.logger.Info("STABLE VERIFIER: Cache hit",
						"path", path,
						"verifier", verifier,
						"entries", len(data))
				} else {
					h.logger.Info("STABLE VERIFIER: Cache miss (no data)",
						"path", path,
						"verifier", verifier)
				}
				return data
			}
		}
	} else {
		h.logger.Info("STABLE VERIFIER: No stored verifier for path",
			"path", path)
	}

	// No match or no cached data
	return nil
}

// InvalidateHandle clears the stable verifier when a handle is invalidated
func (h *StableVerifierHandler) InvalidateHandle(fs billy.Filesystem, handle []byte) error {
	// Get the path for this handle
	if _, p, err := h.FromHandle(handle); err == nil {
		path := fs.Join(p...)
		h.verifiers.Delete(path)
		h.logger.Debug("Invalidated stable verifier", "path", path)
	}

	// Delegate to wrapped handler
	return h.handler.InvalidateHandle(fs, handle)
}

// All other Handler methods are delegated to the wrapped handler
func (h *StableVerifierHandler) Mount(ctx context.Context, conn net.Conn, req gonfs.MountRequest) (gonfs.MountStatus, billy.Filesystem, []gonfs.AuthFlavor) {
	return h.handler.Mount(ctx, conn, req)
}

func (h *StableVerifierHandler) Change(fs billy.Filesystem) billy.Change {
	return h.handler.Change(fs)
}

func (h *StableVerifierHandler) FSStat(ctx context.Context, fs billy.Filesystem, stat *gonfs.FSStat) error {
	return h.handler.FSStat(ctx, fs, stat)
}

func (h *StableVerifierHandler) ToHandle(fs billy.Filesystem, path []string) []byte {
	return h.handler.ToHandle(fs, path)
}

func (h *StableVerifierHandler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	return h.handler.FromHandle(fh)
}

func (h *StableVerifierHandler) HandleLimit() int {
	return h.handler.HandleLimit()
}

// Made with Bob
