package server

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// Server-side copy limits (MS-SMB2 section 3.3.5.15.6). A client that asks
// for more is told what the limits are and retries within them.
const (
	maxCopyChunks     uint32 = 16
	maxCopyChunkSize  uint32 = 1 << 20
	maxCopyTotalBytes uint32 = 16 << 20
)

// copyBufferSize bounds the memory one chunk copy uses when the backend
// cannot copy the range itself.
const copyBufferSize = 256 << 10

// resumeKeys maps the tokens handed out by FSCTL_SRV_REQUEST_RESUME_KEY to
// the opens they name. A client asks for a key on the file it is copying
// from, then names that key when it asks the destination to pull the bytes,
// so the two handles can be on different trees.
type resumeKeys struct {
	mu   sync.Mutex
	keys map[[24]byte]*openHandle
	// byHandle lets a closing handle drop its key, so the table does not
	// grow with keys for files that are gone.
	byHandle map[*openHandle][24]byte
}

func newResumeKeys() *resumeKeys {
	return &resumeKeys{keys: make(map[[24]byte]*openHandle), byHandle: make(map[*openHandle][24]byte)}
}

func (r *resumeKeys) issue(oh *openHandle) ([24]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key, ok := r.byHandle[oh]; ok {
		return key, nil
	}
	var key [24]byte
	if _, err := rand.Read(key[:]); err != nil {
		return key, err
	}
	r.keys[key] = oh
	r.byHandle[oh] = key
	return key, nil
}

func (r *resumeKeys) lookup(key [24]byte) (*openHandle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oh, ok := r.keys[key]
	return oh, ok
}

func (r *resumeKeys) release(oh *openHandle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key, ok := r.byHandle[oh]; ok {
		delete(r.keys, key)
		delete(r.byHandle, oh)
	}
}

// resumeKeyTable returns the server's table, creating it for a Server built
// without New.
func (s *Server) resumeKeyTable() *resumeKeys {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resume == nil {
		s.resume = newResumeKeys()
	}
	return s.resume
}

// handleRequestResumeKey answers FSCTL_SRV_REQUEST_RESUME_KEY with a token
// naming this open, which the client then gives to the destination file to
// copy from (MS-SMB2 section 2.2.32.3).
func (c *request) handleRequestResumeKey(req *wire.IoctlRequest, oh *openHandle) uint32 {
	key, err := c.srv.resumeKeyTable().issue(oh)
	if err != nil {
		return c.errBody(wire.StatusInsufficientResources)
	}
	// SRV_REQUEST_RESUME_KEY_RSP: the 24-byte key, a context length of zero,
	// and four bytes of padding.
	out := make([]byte, 32)
	copy(out, key[:])
	c.out = wire.IoctlResponseAppend(c.out, req.CtlCode, req.FileId, nil, out, req.Flags)
	return wire.StatusSuccess
}

// handleCopyChunk answers FSCTL_SRV_COPYCHUNK, copying ranges from the file
// the resume key names into this open, so the bytes never travel to the
// client and back (MS-SMB2 section 2.2.32.1).
func (c *request) handleCopyChunk(ctx context.Context, req *wire.IoctlRequest, dst *openHandle) uint32 {
	var copyReq wire.CopyChunkRequest
	if err := copyReq.Parse(req.Input); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}

	src, ok := c.srv.resumeKeyTable().lookup(copyReq.SourceKey)
	if !ok {
		// A key the server does not know: the source was closed, or the
		// client invented it. Windows retries the copy through the client
		// when it sees this.
		return c.errBody(wire.StatusObjectNameNotFound)
	}

	if status := c.checkCopyLimits(req, copyReq); status != wire.StatusSuccess {
		return status
	}

	var total uint32
	for _, chunk := range copyReq.Chunks {
		n, err := copyRange(ctx, src.h, dst.h, int64(chunk.SourceOffset), int64(chunk.TargetOffset), int64(chunk.Length))
		total += uint32(n)
		if err != nil {
			c.log.Debug("copychunk failed", "source", src.currentPath(), "target", dst.currentPath(), "err", err)
			return c.errBody(osErrToStatus(err))
		}
	}

	c.srv.dataChanged(dst)
	c.log.Debug("copychunk", "source", src.currentPath(), "target", dst.currentPath(),
		"chunks", len(copyReq.Chunks), "bytes", total)
	resp := wire.CopyChunkResponse{ChunksWritten: uint32(len(copyReq.Chunks)), TotalBytesWritten: total}
	c.out = wire.IoctlResponseAppend(c.out, req.CtlCode, req.FileId, nil, resp.Encode(), req.Flags)
	return wire.StatusSuccess
}

// checkCopyLimits refuses a request that asks for more than the server
// copies at once, answering with the limits so the client can split the work
// (MS-SMB2 section 3.3.5.15.6).
func (c *request) checkCopyLimits(req *wire.IoctlRequest, copyReq wire.CopyChunkRequest) uint32 {
	var total uint64
	tooBig := uint32(len(copyReq.Chunks)) > maxCopyChunks
	for _, chunk := range copyReq.Chunks {
		if chunk.Length > maxCopyChunkSize {
			tooBig = true
		}
		total += uint64(chunk.Length)
	}
	if total > uint64(maxCopyTotalBytes) {
		tooBig = true
	}
	if !tooBig {
		return wire.StatusSuccess
	}

	limits := wire.CopyChunkResponse{
		ChunksWritten:     maxCopyChunks,
		ChunkBytesWritten: maxCopyChunkSize,
		TotalBytesWritten: maxCopyTotalBytes,
	}
	c.out = wire.IoctlResponseAppend(c.out, req.CtlCode, req.FileId, nil, limits.Encode(), req.Flags)
	return wire.StatusInvalidParameter
}

// copyRange copies length bytes from src at srcOffset to dst at dstOffset.
// A backend that can copy a range itself does so; otherwise the bytes move
// through the server, which still spares the client the round trip.
func copyRange(ctx context.Context, src, dst vfs.Handle, srcOffset, dstOffset, length int64) (int64, error) {
	if length <= 0 {
		return 0, nil
	}
	if copier, ok := dst.(vfs.ChunkCopier); ok {
		n, err := copier.CopyChunk(ctx, src, srcOffset, dstOffset, length)
		if !errors.Is(err, errors.ErrUnsupported) {
			return n, err
		}
		// The backend declined this particular copy; fall through.
	}

	buf := make([]byte, min(int64(copyBufferSize), length))
	var copied int64
	for copied < length {
		want := min(int64(len(buf)), length-copied)
		read, err := src.Read(ctx, srcOffset+copied, buf[:want])
		if read > 0 {
			written, writeErr := dst.Write(ctx, dstOffset+copied, buf[:read])
			copied += int64(written)
			if writeErr != nil {
				return copied, writeErr
			}
			if written < read {
				return copied, io.ErrShortWrite
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The source ended early: report what was copied rather
				// than inventing bytes.
				return copied, nil
			}
			return copied, err
		}
		if read == 0 {
			return copied, nil
		}
	}
	return copied, nil
}
