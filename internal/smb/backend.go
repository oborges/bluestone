// Package smb serves the shared filesystem layer over SMB through the vendored
// go-smb-server library.
package smb

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"iter"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/oborges/bluestone/internal/lock"
	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/internal/vfs"
	smbvfs "github.com/sonroyaalmerol/go-smb-server/smb/vfs"
)

// Backend exposes a Bluestone filesystem view as an SMB share. Give it a view
// with Windows naming (vfs.Filesystem.WithWindowsNames), labelled for SMB
// metrics, so names behave as SMB clients expect.
type Backend struct {
	fs    *vfs.Filesystem
	opens *lock.ShareTable
}

// NewBackend returns an SMB share backend over fs. Opens are recorded in the
// share table, so a client that opens a file without sharing it blocks other
// clients until it closes; a nil table gets one of its own.
func NewBackend(fs *vfs.Filesystem, opens *lock.ShareTable) *Backend {
	if opens == nil {
		opens = lock.NewShareTable(lock.ShareOptions{})
	}
	return &Backend{fs: fs, opens: opens}
}

// SMB access bits an open can ask for (MS-SMB2 section 2.2.13).
const (
	accessReadData     uint32 = 0x00000001
	accessWriteData    uint32 = 0x00000002
	accessAppendData   uint32 = 0x00000004
	accessExecute      uint32 = 0x00000020
	accessDelete       uint32 = 0x00010000
	accessGenericAll   uint32 = 0x10000000
	accessGenericExec  uint32 = 0x20000000
	accessGenericWrite uint32 = 0x40000000
	accessGenericRead  uint32 = 0x80000000
	shareAccessRead    uint32 = 0x00000001
	shareAccessWrite   uint32 = 0x00000002
	shareAccessDelete  uint32 = 0x00000004
)

// requestedAccess reduces an SMB desired access mask to the file uses share
// modes are decided on. Opens that only touch metadata need nothing.
func requestedAccess(desired uint32, deleteOnClose bool) lock.Access {
	var access lock.Access
	if desired&(accessReadData|accessExecute|accessGenericRead|accessGenericExec|accessGenericAll) != 0 {
		access |= lock.AccessRead
	}
	if desired&(accessWriteData|accessAppendData|accessGenericWrite|accessGenericAll) != 0 {
		access |= lock.AccessWrite
	}
	if desired&(accessDelete|accessGenericAll) != 0 || deleteOnClose {
		access |= lock.AccessDelete
	}
	return access
}

// permittedAccess reduces an SMB share access mask to what the open lets
// other opens do.
func permittedAccess(share uint32) lock.Access {
	var permitted lock.Access
	if share&shareAccessRead != 0 {
		permitted |= lock.AccessRead
	}
	if share&shareAccessWrite != 0 {
		permitted |= lock.AccessWrite
	}
	if share&shareAccessDelete != 0 {
		permitted |= lock.AccessDelete
	}
	return permitted
}

// reserve records the open in the share table, reporting a conflict in the
// form the SMB server turns into STATUS_SHARING_VIOLATION.
func (b *Backend) reserve(path string, opts smbvfs.OpenOptions) (*lock.ShareHandle, error) {
	reservation, err := b.opens.Acquire(path,
		requestedAccess(opts.DesiredAccess, opts.DeleteOnClose),
		permittedAccess(opts.ShareAccess))
	switch {
	case errors.Is(err, lock.ErrSharingViolation):
		return nil, smbvfs.ErrSharingViolation
	case err != nil:
		return nil, err
	}
	return reservation, nil
}

var (
	_ smbvfs.Backend   = (*Backend)(nil)
	_ smbvfs.Remover   = (*Backend)(nil)
	_ smbvfs.SetInfoer = (*handle)(nil)
	_ smbvfs.Renamer   = (*handle)(nil)
)

// sharePath converts an SMB path (backslash-separated, relative to the share
// root, empty for the root) to a filesystem path.
func sharePath(name string) string {
	return path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))
}

// Open implements smbvfs.Backend: every SMB CREATE lands here.
func (b *Backend) Open(_ context.Context, opts smbvfs.OpenOptions) (smbvfs.Handle, error) {
	p := sharePath(opts.Path)
	info, err := b.fs.Stat(p)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	// Directories are not reserved: clients open them constantly to list and
	// look up names, and share modes there would only produce false
	// conflicts.
	var reservation *lock.ShareHandle
	if !opts.CreateDir && !(exists && info.IsDir()) {
		reservation, err = b.reserve(p, opts)
		if err != nil {
			return nil, err
		}
		defer func() {
			// Released unless a handle takes it over.
			if reservation != nil {
				reservation.Release()
			}
		}()
	}

	if opts.CreateDir {
		switch {
		case exists && (opts.Disposition == smbvfs.DispositionCreate || !info.IsDir()):
			return nil, fs.ErrExist
		case !exists && opts.Disposition == smbvfs.DispositionOpen:
			return nil, fs.ErrNotExist
		case !exists:
			if err := b.fs.MkdirAll(p, 0o755); err != nil {
				return nil, err
			}
		}
		return &handle{fs: b.fs, path: p, dir: true}, nil
	}

	if exists && info.IsDir() {
		// Directories are opened without the directory flag to list or query
		// them; only dispositions that keep existing content make sense.
		if opts.Disposition == smbvfs.DispositionOpen || opts.Disposition == smbvfs.DispositionOpenIf {
			return &handle{fs: b.fs, path: p, dir: true}, nil
		}
		return nil, fs.ErrExist
	}

	opened, err := func() (smbvfs.Handle, error) {
		switch opts.Disposition {
		case smbvfs.DispositionOpen:
			if !exists {
				return nil, fs.ErrNotExist
			}
			return &handle{fs: b.fs, path: p}, nil
		case smbvfs.DispositionOpenIf:
			if exists {
				return &handle{fs: b.fs, path: p}, nil
			}
			return b.openWritable(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
		case smbvfs.DispositionCreate:
			if exists {
				return nil, fs.ErrExist
			}
			return b.openWritable(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
		case smbvfs.DispositionOverwrite:
			if !exists {
				return nil, fs.ErrNotExist
			}
			return b.openWritable(p, os.O_RDWR|os.O_TRUNC)
		case smbvfs.DispositionSupersede, smbvfs.DispositionOverwriteIf:
			return b.openWritable(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
		default:
			return nil, fs.ErrInvalid
		}
	}()
	if err != nil {
		return nil, err
	}
	if h, ok := opened.(*handle); ok {
		h.reservation = reservation
		h.opens = b.opens
		reservation = nil // the handle releases it on close
	}
	return opened, nil
}

// openWritable opens a file for writing immediately; used when the CREATE
// itself creates or truncates the file. New files are opened with O_TRUNC so
// the empty file is staged and synced even if nothing is written.
func (b *Backend) openWritable(p string, flag int) (smbvfs.Handle, error) {
	f, err := b.fs.OpenFile(p, flag, 0o644)
	if err != nil {
		return nil, err
	}
	return &handle{fs: b.fs, path: p, file: f, writable: true}, nil
}

// Remove implements smbvfs.Remover for delete-on-close.
func (b *Backend) Remove(_ context.Context, name string) error {
	return b.fs.Remove(sharePath(name))
}

// Space implements smbvfs.SpaceReporter with the capacity NFS also reports:
// the share is as large as the staging area, since every write lands there
// before it reaches the bucket, and free space stops at the staging high
// watermark, where writes start waiting on uploads. Windows refuses a copy
// that will not fit rather than failing partway with STATUS_DISK_FULL.
func (b *Backend) Space(context.Context) (smbvfs.Space, error) {
	capacity := b.fs.Capacity()
	return smbvfs.Space{TotalBytes: capacity.TotalBytes, AvailableBytes: capacity.AvailableBytes}, nil
}

// handle is an open SMB file or directory. Files that already exist are
// opened read-only and upgraded to read-write on the first write, truncate,
// or attribute change: SMB CREATE carries no access intent to the backend,
// and a read-write open of an existing object downloads it into staging.
type handle struct {
	fs  *vfs.Filesystem
	dir bool

	mu       sync.Mutex
	path     string
	file     billy.File
	writable bool
	// reservation is this open's entry in the share table, released when the
	// handle closes; opens is where a rename moves it.
	reservation *lock.ShareHandle
	opens       *lock.ShareTable
	// copied is set once a server-side copy has made the file a copy of
	// another inside the bucket, so the rest of that copy's chunks have
	// nothing left to move. Any write through this handle clears it.
	copied *bucketCopy
}

// bucketCopy describes a bucket-side copy into a handle's file: the source,
// and both files as they were right after the copy.
type bucketCopy struct {
	src     string
	size    int64
	srcTime time.Time
	dstTime time.Time
}

// fileFor returns the open file, reopening it read-write when write is set
// and it is only open read-only.
func (h *handle) fileFor(write bool) (billy.File, error) {
	if h.dir {
		return nil, &fs.PathError{Op: "open", Path: h.path, Err: fs.ErrInvalid}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if write {
		h.copied = nil
	}
	if h.file != nil && (h.writable || !write) {
		return h.file, nil
	}
	flag := os.O_RDONLY
	if write {
		flag = os.O_RDWR
	}
	f, err := h.fs.OpenFile(h.path, flag, 0)
	if err != nil {
		return nil, err
	}
	if h.file != nil {
		_ = h.file.Close()
	}
	h.file, h.writable = f, write
	return f, nil
}

// Read implements smbvfs.Handle.
func (h *handle) Read(_ context.Context, offset int64, p []byte) (int, error) {
	f, err := h.fileFor(false)
	if err != nil {
		return 0, err
	}
	return f.ReadAt(p, offset)
}

// Write implements smbvfs.Handle.
func (h *handle) Write(_ context.Context, offset int64, p []byte) (int, error) {
	f, err := h.fileFor(true)
	if err != nil {
		return 0, err
	}
	writerAt, ok := f.(io.WriterAt)
	if !ok {
		return 0, &fs.PathError{Op: "write", Path: h.path, Err: errors.ErrUnsupported}
	}
	return writerAt.WriteAt(p, offset)
}

// Close implements smbvfs.Handle.
func (h *handle) Close(_ context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reservation.Release()
	h.reservation = nil
	if h.file == nil {
		return nil
	}
	err := h.file.Close()
	h.file = nil
	return err
}

// Stat implements smbvfs.Handle.
func (h *handle) Stat(_ context.Context) (smbvfs.FileInfo, error) {
	h.mu.Lock()
	p := h.path
	h.mu.Unlock()
	info, err := h.fs.Stat(p)
	if err != nil {
		return smbvfs.FileInfo{}, err
	}
	return fileInfo(info), nil
}

// Enumerate implements smbvfs.Handle for directory listings. Patterns match
// case-insensitively, as on Windows.
func (h *handle) Enumerate(_ context.Context, pattern string) iter.Seq2[smbvfs.FileInfo, error] {
	return func(yield func(smbvfs.FileInfo, error) bool) {
		if !h.dir {
			yield(smbvfs.FileInfo{}, &fs.PathError{Op: "readdir", Path: h.path, Err: fs.ErrInvalid})
			return
		}
		entries, err := h.fs.ReadDir(h.path)
		if err != nil {
			yield(smbvfs.FileInfo{}, err)
			return
		}
		for _, entry := range entries {
			matched, err := matchPattern(pattern, entry.Name())
			if err != nil {
				yield(smbvfs.FileInfo{}, err)
				return
			}
			if matched && !yield(fileInfo(entry), nil) {
				return
			}
		}
	}
}

// SetInfo implements smbvfs.SetInfoer. Zero times mean "leave unchanged".
func (h *handle) SetInfo(_ context.Context, req *smbvfs.SetInfoRequest) error {
	if req.EndOfFile != nil {
		f, err := h.fileFor(true)
		if err != nil {
			return err
		}
		if err := f.Truncate(*req.EndOfFile); err != nil {
			return err
		}
	}

	var update posix.AttributeUpdate
	changed := false
	setTime := func(target **time.Time, value *time.Time) {
		if value != nil && !value.IsZero() {
			*target = value
			changed = true
		}
	}
	setTime(&update.Btime, req.CreationTime)
	setTime(&update.Mtime, req.LastWriteTime)
	setTime(&update.Atime, req.LastAccessTime)
	if req.Attributes != nil {
		flags := *req.Attributes
		update.WindowsAttributes = &flags
		changed = true
	}
	if !changed {
		return nil
	}

	h.mu.Lock()
	p := h.path
	h.mu.Unlock()
	return h.fs.SetAttributes(p, update)
}

// Rename implements smbvfs.Renamer. newPath is relative to the share root.
func (h *handle) Rename(_ context.Context, newPath string, replaceIfExists bool) error {
	target := sharePath(newPath)

	h.mu.Lock()
	defer h.mu.Unlock()
	if !replaceIfExists && !strings.EqualFold(target, h.path) {
		if _, err := h.fs.Stat(target); err == nil {
			return fs.ErrExist
		}
	}
	// Release the open file first so staged data moves with the rename; the
	// next I/O reopens it under the new name.
	if h.file != nil {
		if err := h.file.Close(); err != nil {
			return err
		}
		h.file = nil
	}
	if err := h.fs.Rename(h.path, target); err != nil {
		return err
	}
	h.copied = nil
	if h.opens != nil {
		h.opens.Rename(h.path, target)
	}
	h.path = target
	return nil
}

// fileInfo converts a filesystem entry for SMB.
func fileInfo(info os.FileInfo) smbvfs.FileInfo {
	attrs := vfs.FileAttributes(info)
	return smbvfs.FileInfo{
		Name:         info.Name(),
		Size:         info.Size(),
		IsDir:        info.IsDir(),
		Attributes:   attrs.WindowsAttributes,
		CreationTime: attrs.Btime,
		LastAccess:   info.ModTime(),
		LastWrite:    info.ModTime(),
		ChangeTime:   info.ModTime(),
	}
}

// matchPattern reports whether name matches an SMB search pattern, ignoring
// case. An empty pattern and "*" match everything.
func matchPattern(pattern, name string) (bool, error) {
	if pattern == "" || pattern == "*" {
		return true, nil
	}
	return filepath.Match(strings.ToLower(pattern), strings.ToLower(name))
}

// CopyChunk implements smbvfs.ChunkCopier: a client copying a file inside
// the share asks the gateway to move the bytes, and when the copy covers the
// whole file it becomes a copy inside the bucket, with no bytes moving at
// all.
//
// Clients send a copy as chunks of at most 1 MiB, so a whole-file copy is
// recognized from its first chunk in one of two ways:
//
//   - the chunk covers the whole source, and the destination is no longer
//     than it
//   - the destination is already exactly as long as the source and holds no
//     bytes anyone wrote, as when Windows creates the file and sets its
//     length before asking for the copy. Its bytes are all zeros, so copying
//     the whole file early leaves nothing a partial copy would have kept.
//
// After that, later chunks of the same copy are acknowledged without moving
// anything, as long as neither file has changed since.
//
// Anything else returns errors.ErrUnsupported so the server copies the bytes
// itself. Even then they only travel gateway-side, never out to the client.
func (h *handle) CopyChunk(_ context.Context, src smbvfs.Handle, srcOffset, dstOffset, length int64) (int64, error) {
	source, ok := src.(*handle)
	if !ok || source == h || h.dir || source.dir {
		return 0, errors.ErrUnsupported
	}
	// A bucket-side copy puts every byte where it was in the source.
	if srcOffset != dstOffset {
		return 0, errors.ErrUnsupported
	}

	source.mu.Lock()
	srcPath := source.path
	source.mu.Unlock()

	// Held for the whole copy: this handle's writes, including the server
	// copying another chunk itself, wait until the copy has landed.
	h.mu.Lock()
	defer h.mu.Unlock()

	srcInfo, err := h.fs.Stat(srcPath)
	if err != nil {
		return 0, err
	}
	size := srcInfo.Size()
	// A range past the end of the source copies short, which the server
	// handles.
	if srcOffset+length > size {
		return 0, errors.ErrUnsupported
	}

	if h.copied != nil {
		if h.stillCopied(srcPath, srcInfo) {
			return length, nil
		}
		h.copied = nil
		return 0, errors.ErrUnsupported
	}

	if srcOffset != 0 {
		return 0, errors.ErrUnsupported
	}
	// A destination that is gone, removed by another client, is empty.
	var dstSize int64
	if dstInfo, err := h.fs.Stat(h.path); err == nil {
		dstSize = dstInfo.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	var copyErr error
	switch {
	case length == size && dstSize <= size:
		copyErr = h.copyFileLocked(srcPath, false)
	case dstSize == size:
		copyErr = h.copyFileLocked(srcPath, true)
	default:
		return 0, errors.ErrUnsupported
	}
	if copyErr != nil {
		return 0, copyErr
	}

	h.copied = &bucketCopy{src: srcPath, size: size, srcTime: srcInfo.ModTime()}
	if info, err := h.fs.Stat(h.path); err == nil && info.Size() == size {
		h.copied.dstTime = info.ModTime()
	} else {
		// The copy landed, but it cannot vouch for later chunks.
		h.copied = nil
	}
	return length, nil
}

// copyFileLocked copies srcPath over the handle's file inside the bucket,
// with h.mu held. The handle's open file is closed first: it holds the
// destination's staged session, which the copy can only discard once no
// handle holds it. The next I/O reopens it.
func (h *handle) copyFileLocked(srcPath string, onlyUnwritten bool) error {
	if h.file != nil {
		if err := h.file.Close(); err != nil {
			return err
		}
		h.file, h.writable = nil, false
	}
	return h.fs.CopyFile(srcPath, h.path, onlyUnwritten)
}

// stillCopied reports whether the handle's file is still the bucket-side
// copy of srcPath it was made, so a later chunk of the same copy is already
// in place. srcInfo is the source as it is now.
func (h *handle) stillCopied(srcPath string, srcInfo os.FileInfo) bool {
	c := h.copied
	if c.src != srcPath || srcInfo.Size() != c.size || !srcInfo.ModTime().Equal(c.srcTime) {
		return false
	}
	// Staged on either side means another handle has written since.
	if h.fs.IsStaged(srcPath) || h.fs.IsStaged(h.path) {
		return false
	}
	dstInfo, err := h.fs.Stat(h.path)
	return err == nil && dstInfo.Size() == c.size && dstInfo.ModTime().Equal(c.dstTime)
}
