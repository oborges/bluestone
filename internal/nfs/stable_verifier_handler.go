package nfs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io/fs"
	"net"

	"github.com/go-git/go-billy/v5"
	"github.com/oborges/bluestone/internal/logging"
	gonfs "github.com/willscott/go-nfs"
)

// StableVerifierHandler wraps a CachingHandler to provide stable verifiers
// This prevents BadCookie errors that cause clients to restart enumeration
type StableVerifierHandler struct {
	handler gonfs.Handler
	logger  *logging.KVLogger
}

// NewStableVerifierHandler creates a handler that returns stable verifiers per directory
func NewStableVerifierHandler(handler gonfs.Handler, logger *logging.KVLogger) gonfs.Handler {
	return &StableVerifierHandler{
		handler: handler,
		logger:  logger,
	}
}

// stableVerifier is the verifier of a directory path. It comes from the path
// alone, so it is the same on every call with nothing kept per directory, and
// does not move when the contents change.
func stableVerifier(path string) uint64 {
	sum := sha256.Sum256([]byte(path))
	return binary.BigEndian.Uint64(sum[:8])
}

// VerifierFor returns a stable verifier for a directory path
// Unlike the default implementation, this returns the SAME verifier every time
// for the same path, preventing BadCookie errors during pagination
func (h *StableVerifierHandler) VerifierFor(path string, contents []fs.FileInfo) uint64 {
	verifier := stableVerifier(path)

	h.logger.Info("STABLE VERIFIER: Verifier for directory",
		"path", path,
		"verifier", verifier,
		"entries", len(contents))

	return verifier
}

// DataForVerifier checks if we have cached data for a verifier
// Since we use stable verifiers, we delegate to the wrapped handler
func (h *StableVerifierHandler) DataForVerifier(path string, verifier uint64) []fs.FileInfo {
	stored := stableVerifier(path)
	h.logger.Info("STABLE VERIFIER: DataForVerifier called",
		"path", path,
		"stored", stored,
		"requested", verifier,
		"match", stored == verifier)

	if stored != verifier {
		return nil
	}

	// Verifier matches - delegate to wrapped handler if it's a CachingHandler
	ch, ok := h.handler.(gonfs.CachingHandler)
	if !ok {
		return nil
	}
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

// InvalidateHandle delegates to the wrapped handler; a verifier is not stored,
// so there is none to clear.
func (h *StableVerifierHandler) InvalidateHandle(fs billy.Filesystem, handle []byte) error {
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
