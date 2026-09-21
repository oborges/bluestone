package smb

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"iter"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/lock"
	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/internal/vfs"
	"github.com/oborges/bluestone/pkg/types"
	smbvfs "github.com/sonroyaalmerol/go-smb-server/smb/vfs"
)

// Named data streams (alternate data streams) are kept in the file's
// attributes, which the gateway stores in the object's metadata: they move
// with renames and copies, go with the file when it is deleted, and never
// appear in the bucket as objects of their own. macOS stores Finder
// information, extended attributes and resource forks this way once the
// share advertises named streams, instead of writing an AppleDouble "._"
// file beside every file, and Windows keeps a download's "Zone.Identifier"
// mark.
//
// Object metadata is small, so a file's streams are capped: see
// config.SMBConfig.MaxStreamBytes. A write past the cap fails as disk full.

var (
	_ smbvfs.StreamOpener = (*Backend)(nil)
	_ smbvfs.StreamLister = (*handle)(nil)
)

// maxStreamNameLength matches the longest name NTFS allows.
const maxStreamNameLength = 255

// streamLimit is the cap on a file's streams, names and contents together.
func (b *Backend) streamLimit() int {
	if b.maxStreamBytes > 0 {
		return b.maxStreamBytes
	}
	return config.DefaultMaxStreamBytes
}

// streamsBytes is what a file's streams count against the cap.
func streamsBytes(streams map[string][]byte) int {
	total := 0
	for name, data := range streams {
		total += len(name) + len(data)
	}
	return total
}

// findStream looks a stream up the way Windows does, ignoring case, and
// returns the name it is stored under.
func findStream(streams map[string][]byte, name string) (string, bool) {
	if _, ok := streams[name]; ok {
		return name, true
	}
	for stored := range streams {
		if strings.EqualFold(stored, name) {
			return stored, true
		}
	}
	return name, false
}

func validStreamName(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > maxStreamNameLength {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool {
		return r < 0x20 || r == '\\' || r == '/' || r == ':'
	})
}

// fileStreams reads the named streams of the file at p.
func (b *Backend) fileStreams(p string) (map[string][]byte, os.FileInfo, error) {
	info, err := b.fs.Stat(p)
	if err != nil {
		return nil, nil, err
	}
	return vfs.FileAttributes(info).Streams, info, nil
}

// updateStream changes one of the file's streams: sets it to data, or
// removes it when data is nil. Changes are serialized so two handles
// closing different streams of one file do not lose either.
func (b *Backend) updateStream(p, name string, data []byte, remove bool) error {
	b.streamMu.Lock()
	defer b.streamMu.Unlock()
	current, _, err := b.fileStreams(p)
	if err != nil {
		return err
	}
	streams := types.CloneStreams(current)
	if streams == nil {
		streams = map[string][]byte{}
	}
	stored, exists := findStream(streams, name)
	if remove {
		if !exists {
			return fs.ErrNotExist
		}
		delete(streams, stored)
	} else {
		streams[stored] = append([]byte(nil), data...)
		if streamsBytes(streams) > b.streamLimit() {
			return syscall.ENOSPC
		}
	}
	return b.fs.SetAttributes(p, posix.AttributeUpdate{Streams: streams})
}

// OpenStream implements smbvfs.StreamOpener. The disposition applies to the
// stream; a missing file is created, empty, when the disposition creates.
func (b *Backend) OpenStream(_ context.Context, opts smbvfs.OpenOptions, stream string) (smbvfs.Handle, error) {
	if !validStreamName(stream) {
		return nil, fs.ErrInvalid
	}
	p := sharePath(opts.Path)
	creates := opts.Disposition != smbvfs.DispositionOpen && opts.Disposition != smbvfs.DispositionOverwrite

	streams, _, err := b.fileStreams(p)
	switch {
	case errors.Is(err, fs.ErrNotExist) && creates:
		f, createErr := b.fs.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
		if createErr != nil {
			return nil, createErr
		}
		if createErr := f.Close(); createErr != nil {
			return nil, createErr
		}
		streams = nil
	case err != nil:
		return nil, err
	}

	name, exists := findStream(streams, stream)
	data := streams[name]
	switch opts.Disposition {
	case smbvfs.DispositionOpen:
		if !exists {
			return nil, fs.ErrNotExist
		}
	case smbvfs.DispositionCreate:
		if exists {
			return nil, fs.ErrExist
		}
	case smbvfs.DispositionOverwrite:
		if !exists {
			return nil, fs.ErrNotExist
		}
		data = nil
	case smbvfs.DispositionOverwriteIf, smbvfs.DispositionSupersede:
		data = nil
	case smbvfs.DispositionOpenIf:
	default:
		return nil, fs.ErrInvalid
	}

	// Share modes apply to each stream on its own, as on Windows: holding
	// a file exclusively does not keep others from its streams.
	reservation, err := b.reserve(p+":"+strings.ToLower(name), opts)
	if err != nil {
		return nil, err
	}
	h := &streamHandle{b: b, path: p, name: name, data: append([]byte(nil), data...), reservation: reservation}
	if !exists || data == nil && len(streams[name]) > 0 {
		// A stream created or emptied by the open exists from now on,
		// even if nothing is written to it.
		if err := b.updateStream(p, name, nil, false); err != nil {
			reservation.Release()
			return nil, err
		}
	}
	return h, nil
}

// Streams implements smbvfs.StreamLister for a file or directory handle.
func (h *handle) Streams(context.Context) ([]smbvfs.StreamInfo, error) {
	h.mu.Lock()
	p := h.path
	h.mu.Unlock()
	info, err := h.fs.Stat(p)
	if err != nil {
		return nil, err
	}
	streams := vfs.FileAttributes(info).Streams
	out := make([]smbvfs.StreamInfo, 0, len(streams))
	for name, data := range streams {
		out = append(out, smbvfs.StreamInfo{Name: name, Size: int64(len(data))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// removeStream deletes one named stream, for Remove on "file:stream".
func (b *Backend) removeStream(name string) (bool, error) {
	base, stream, ok := strings.Cut(name, ":")
	if !ok {
		return false, nil
	}
	return true, b.updateStream(sharePath(base), stream, nil, true)
}

// streamHandle is an open named stream. Its contents are held in memory,
// being small, and saved to the file's attributes when the handle closes,
// so writing a stream in pieces rewrites the object's metadata once.
type streamHandle struct {
	b    *Backend
	path string
	name string

	mu          sync.Mutex
	data        []byte
	dirty       bool
	reservation *lock.ShareHandle
}

var (
	_ smbvfs.Handle    = (*streamHandle)(nil)
	_ smbvfs.SetInfoer = (*streamHandle)(nil)
)

func (h *streamHandle) Read(_ context.Context, offset int64, p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if offset >= int64(len(h.data)) {
		return 0, io.EOF
	}
	return copy(p, h.data[offset:]), nil
}

func (h *streamHandle) Write(_ context.Context, offset int64, p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	end := offset + int64(len(p))
	if err := h.checkSize(end); err != nil {
		return 0, err
	}
	if end > int64(len(h.data)) {
		h.data = append(h.data, make([]byte, end-int64(len(h.data)))...)
	}
	copy(h.data[offset:], p)
	h.dirty = true
	return len(p), nil
}

// checkSize refuses to grow the stream past the cap, counting the file's
// other streams as they are now, so the client hears about it on the write
// rather than only when the handle closes.
func (h *streamHandle) checkSize(size int64) error {
	if size <= int64(len(h.data)) {
		return nil
	}
	limit := int64(h.b.streamLimit())
	if size > limit {
		return syscall.ENOSPC
	}
	streams, _, err := h.b.fileStreams(h.path)
	if err != nil {
		return err
	}
	others := 0
	for name, data := range streams {
		if name != h.name {
			others += len(name) + len(data)
		}
	}
	if int64(others)+int64(len(h.name))+size > limit {
		return syscall.ENOSPC
	}
	return nil
}

// SetInfo implements smbvfs.SetInfoer: a stream can be truncated or
// extended. Times and attributes belong to the file and are left to it.
func (h *streamHandle) SetInfo(_ context.Context, req *smbvfs.SetInfoRequest) error {
	if req.EndOfFile == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	size := *req.EndOfFile
	if err := h.checkSize(size); err != nil {
		return err
	}
	if size < int64(len(h.data)) {
		h.data = h.data[:size]
	} else {
		h.data = append(h.data, make([]byte, size-int64(len(h.data)))...)
	}
	h.dirty = true
	return nil
}

func (h *streamHandle) Close(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reservation != nil {
		h.reservation.Release()
		h.reservation = nil
	}
	if !h.dirty {
		return nil
	}
	h.dirty = false
	return h.b.updateStream(h.path, h.name, h.data, false)
}

func (h *streamHandle) Stat(context.Context) (smbvfs.FileInfo, error) {
	info, err := h.b.fs.Stat(h.path)
	if err != nil {
		return smbvfs.FileInfo{}, err
	}
	fi := fileInfo(info)
	h.mu.Lock()
	fi.Size = int64(len(h.data))
	h.mu.Unlock()
	fi.Name += ":" + h.name
	fi.IsDir = false
	return fi, nil
}

func (h *streamHandle) Enumerate(context.Context, string) iter.Seq2[smbvfs.FileInfo, error] {
	return func(func(smbvfs.FileInfo, error) bool) {}
}
