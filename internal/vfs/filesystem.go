package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/oborges/bluestone/internal/buffer"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/feature"
	"github.com/oborges/bluestone/internal/ha"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/metrics"
	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/internal/staging"
	"github.com/oborges/bluestone/pkg/types"
	"go.uber.org/zap"
)

// Filesystem implements billy.Filesystem interface for COS
type Filesystem struct {
	ops            *posix.OperationsHandler
	logger         *logging.KVLogger
	root           string
	perfConfig     *config.PerformanceConfig
	sessionManager *buffer.SessionManager
	// Staging architecture components
	stagingManager *staging.StagingManager
	syncWorker     *staging.SyncWorker
	featureFlags   *feature.FeatureFlags
	// protocol attributes this view's requests in metrics.
	protocol string
	// windowsNames makes the view follow Windows naming (see
	// WithWindowsNames).
	windowsNames bool
	// changes carries every change made through the filesystem to its
	// subscribers; views share it.
	changes *changeFeed
}

// NewFilesystem creates a new COS filesystem with configuration
func NewFilesystem(ops *posix.OperationsHandler, logger *logging.KVLogger, root string, perfConfig *config.PerformanceConfig, stagingManager *staging.StagingManager, syncWorker *staging.SyncWorker, featureFlags *feature.FeatureFlags) *Filesystem {
	if perfConfig == nil {
		perfConfig = &config.PerformanceConfig{
			WriteBufferKB:        4096,
			MultipartThresholdMB: 100,
			MultipartChunkMB:     10,
			ReadAheadKB:          config.DefaultReadAheadKB,
			MaxBufferedWriteMB:   config.DefaultMaxBufferedWriteMB,
		}
	}

	bufferSize := int64(perfConfig.WriteBufferKB) * 1024
	sessionTimeout := 5 * time.Minute // Keep sessions alive for 5 minutes after last access
	stagingEnabled := featureFlags != nil && featureFlags.IsStagingEnabled()

	logger.Info("Initializing COS filesystem with configuration",
		"write_buffer_kb", perfConfig.WriteBufferKB,
		"write_buffer_bytes", bufferSize,
		"write_buffer_mb", float64(bufferSize)/(1024*1024),
		"multipart_threshold_mb", perfConfig.MultipartThresholdMB,
		"multipart_chunk_mb", perfConfig.MultipartChunkMB,
		"read_ahead_kb", perfConfig.ReadAheadKB,
		"max_buffered_write_mb", perfConfig.MaxBufferedWriteMB,
		"session_timeout", sessionTimeout,
		"staging_enabled", stagingEnabled)

	// Create session manager for path-scoped write buffering (legacy path)
	sessionManager := buffer.NewSessionManager(bufferSize, sessionTimeout)

	return &Filesystem{
		ops:            ops,
		logger:         logger,
		root:           root,
		perfConfig:     perfConfig,
		sessionManager: sessionManager,
		stagingManager: stagingManager,
		syncWorker:     syncWorker,
		featureFlags:   featureFlags,
		changes:        &changeFeed{},
	}
}

// ForProtocol returns a view of the filesystem whose requests are attributed
// to protocol in metrics. Views share all state; each protocol server serves
// its own view.
func (fs *Filesystem) ForProtocol(protocol string) *Filesystem {
	view := *fs
	view.protocol = protocol
	return &view
}

// requestContext carries the view's protocol to the operations it calls.
func (fs *Filesystem) requestContext() context.Context {
	return metrics.WithProtocol(context.Background(), fs.protocol)
}

// WithWindowsNames returns a view that follows Windows naming, for protocols
// whose clients expect it (SMB). Names match case-insensitively: an exact
// match wins, otherwise the first matching key in byte order, and a name with
// no match is used as given, so creating a name that matches an existing key
// opens that key. Characters Windows cannot use in names are presented as
// Unicode private-use characters that map back to the stored key. Views share
// all state; other views keep exact, case-sensitive names.
func (fs *Filesystem) WithWindowsNames() *Filesystem {
	view := *fs
	view.windowsNames = true
	return &view
}

// Create creates a new file
func (fs *Filesystem) Create(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
}

// Capacity is the space the filesystem advertises to clients.
type Capacity struct {
	TotalBytes     uint64
	AvailableBytes uint64
	TotalFiles     uint64
	AvailableFiles uint64
}

// Capacity reports staging-aware capacity so clients see pressure before
// write-back fails: available space stops at the staging high watermark.
func (fs *Filesystem) Capacity() Capacity {
	capacity := Capacity{
		TotalBytes:     1 << 50,
		AvailableBytes: 1 << 50,
		TotalFiles:     1 << 32,
		AvailableFiles: 1 << 32,
	}
	if fs.ops != nil && fs.ops.BucketFull() {
		// The bucket refuses writes, so nothing more fits whatever
		// staging has free.
		capacity.AvailableBytes = 0
		if fs.stagingManager != nil {
			if quota := fs.stagingManager.CurrentPressure().QuotaBytes; quota > 0 {
				capacity.TotalBytes = uint64(quota)
			}
		}
		return capacity
	}
	if fs.featureFlags == nil || !fs.featureFlags.IsStagingEnabled() || fs.stagingManager == nil {
		return capacity
	}

	pressure := fs.stagingManager.CurrentPressure()
	if pressure.QuotaBytes == 0 {
		return capacity
	}
	availableBytes := pressure.AvailableBytes
	if pressure.HighWatermarkBytes > 0 {
		safeAvailable := pressure.HighWatermarkBytes - pressure.UsedBytes
		if safeAvailable < 0 {
			safeAvailable = 0
		}
		if safeAvailable < availableBytes {
			availableBytes = safeAvailable
		}
	}
	capacity.TotalBytes = uint64(pressure.QuotaBytes)
	capacity.AvailableBytes = 0
	if availableBytes > 0 {
		capacity.AvailableBytes = uint64(availableBytes)
	}
	return capacity
}

// Open opens a file for reading
func (fs *Filesystem) Open(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDONLY, 0)
}

// OpenFile opens a file with specified flags and permissions
func (fs *Filesystem) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	fullPath := fs.keyPath(filename)
	if isReservedPath(fullPath) {
		return nil, &os.PathError{Op: "open", Path: filename, Err: os.ErrPermission}
	}

	// Generate unique file handle ID for tracking
	fileID := fmt.Sprintf("%p", &fullPath)

	// Log file open with detailed flags
	flagStr := ""
	if flag&os.O_RDONLY != 0 {
		flagStr += "RDONLY|"
	}
	if flag&os.O_WRONLY != 0 {
		flagStr += "WRONLY|"
	}
	if flag&os.O_RDWR != 0 {
		flagStr += "RDWR|"
	}
	if flag&os.O_CREATE != 0 {
		flagStr += "CREATE|"
	}
	if flag&os.O_TRUNC != 0 {
		flagStr += "TRUNC|"
	}
	if flag&os.O_APPEND != 0 {
		flagStr += "APPEND|"
	}

	useStagingPath := fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled()

	fs.logger.Debug("FILE OPEN",
		"file_id", fileID,
		"path", fullPath,
		"flags", flagStr,
		"perm", fmt.Sprintf("%o", perm),
		"staging_enabled", useStagingPath)

	file := &File{
		ops:            fs.ops,
		logger:         fs.logger,
		path:           fullPath,
		flag:           flag,
		perm:           perm,
		offset:         0,
		perfConfig:     fs.perfConfig,
		fileID:         fileID,
		sessionManager: fs.sessionManager,
		stagingManager: fs.stagingManager,
		syncWorker:     fs.syncWorker,
		featureFlags:   fs.featureFlags,
		protocol:       fs.protocol,
		changes:        fs.changes,
	}

	// A path with an accepted-but-unconfirmed delete must look nonexistent:
	// non-create opens fail, and creates must not prefetch the doomed object.
	pendingDelete := useStagingPath && fs.stagingManager != nil && fs.stagingManager.HasPendingDelete(fullPath)
	if pendingDelete && flag&os.O_CREATE == 0 {
		return nil, &os.PathError{Op: "open", Path: filename, Err: os.ErrNotExist}
	}

	// If using staging path, get or create staging session
	// For writable files: create if needed
	// For read-only files: get existing session if file is being staged
	fileExists := false
	existenceKnown := false
	if useStagingPath {
		if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE) != 0 {
			// Writable file: get or create session
			// Note: GetOrCreateSession already increments ref count
			session, err := fs.stagingManager.GetOrCreateSession(fullPath)
			if err != nil {
				fs.logger.Error("Failed to get staging session",
					"file_id", fileID,
					"path", fullPath,
					"error", err)
				return nil, err
			}
			file.stagingSession = session

			// A session that already carries data answers the existence
			// question locally. This keeps the object store entirely out of
			// the per-WRITE-RPC open path (NFS clients open per WRITE):
			// consulting COS here cost ~3 round trips per write for files
			// that exist only in staging.
			if session.Dirty || session.Prefetched || session.Size > 0 {
				fileExists = true
				existenceKnown = true
			}
		} else {
			// Read-only file: check if there's an existing staging session
			session, exists := fs.stagingManager.GetSession(fullPath)
			if exists {
				file.stagingSession = session
				session.IncrementRefCount()

				fs.logger.Debug("Staging session acquired for read",
					"file_id", fileID,
					"path", fullPath,
					"ref_count", session.GetRefCount())
			}
		}
	}

	writable := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE) != 0
	var existingAttrs *types.POSIXAttributes
	if !existenceKnown {
		info, err := fs.ops.Stat(fs.requestContext(), fullPath)
		fileExists = err == nil && !pendingDelete
		if fileExists {
			attrs := info.Attributes()
			existingAttrs = &attrs
		}

		// A writable open that keeps existing content needs that content
		// staged first. If the object store cannot say whether an object is
		// here, refuse: writes would land on an empty staged copy that later
		// replaces the object. Truncating opens discard the content anyway,
		// and a pending delete means the object is already gone.
		if err != nil && !errors.Is(err, os.ErrNotExist) && !pendingDelete &&
			writable && flag&os.O_TRUNC == 0 && file.stagingSession != nil {
			fs.stagingManager.ReleaseSessionDiscardingIfEmpty(fullPath)
			fs.logger.Error("Refusing writable open: object store cannot confirm existing content",
				"file_id", fileID,
				"path", fullPath,
				"error", err)
			return nil, &os.PathError{Op: "open", Path: filename, Err: err}
		}
	}

	if useStagingPath && file.stagingSession != nil && flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE) != 0 {
		// The staged bytes replace the object on sync, so they carry the
		// object's attributes, even when this open truncates it.
		if existingAttrs != nil {
			file.stagingSession.SeedAttributes(staging.StagedAttributesFrom(*existingAttrs))
		}

		// Automatically pre-fetch existing COS objects if modifying without truncating
		if fileExists && !existenceKnown && flag&os.O_TRUNC == 0 {
			err := file.stagingSession.Prefetch(func() error {
				fs.logger.Info("Prefetching existing COS object to local staging cache",
					"file_id", fileID,
					"path", fullPath)
				return fs.ops.DownloadToFile(fs.requestContext(), fullPath, file.stagingSession.StagingPath)
			})
			if err != nil {
				// Without the object's bytes staged, writes would land on an
				// empty or partial copy that later replaces the object.
				fs.stagingManager.ReleaseSessionDiscardingIfEmpty(fullPath)
				fs.logger.Error("Refusing writable open: prefetch of existing object failed",
					"file_id", fileID,
					"path", fullPath,
					"error", err)
				return nil, &os.PathError{Op: "open", Path: filename, Err: err}
			}
		}

		fs.logger.Debug("Staging session acquired for write",
			"file_id", fileID,
			"path", fullPath,
			"ref_count", file.stagingSession.GetRefCount())
	}

	// If creating a new file
	if flag&os.O_CREATE != 0 && !fileExists {
		// File will be created on first write
		file.isNew = true
		if useStagingPath && file.stagingSession != nil {
			// A new file gets the mode it was created with. Staged files
			// otherwise start at 0600, which NFS clients hide by setting a
			// mode right after creating, and SMB does not.
			file.stagingSession.SetCreationIfUnset(time.Now(), perm&os.ModePerm)
		}
	}

	// If truncating, clear the file
	if flag&os.O_TRUNC != 0 {
		if useStagingPath && file.stagingSession != nil {
			// Truncate staging session to size 0
			if err := file.stagingSession.Truncate(0); err != nil {
				fs.logger.Error("Failed to truncate staging file",
					"file_id", fileID,
					"path", fullPath,
					"error", err)
				return nil, fmt.Errorf("failed to truncate staging file: %w", err)
			}
			fs.stagingManager.MarkDirty(fullPath, 0)

			fs.logger.Info("Truncated staging file",
				"file_id", fileID,
				"path", fullPath,
				"size", 0)
		} else {
			// Legacy path: truncate directly to COS
			attrs := &types.POSIXAttributes{
				Mode:  perm,
				UID:   1000,
				GID:   1000,
				Mtime: time.Now(),
			}
			err := fs.ops.WriteFile(fs.requestContext(), fullPath, []byte{}, attrs)
			if err != nil {
				return nil, err
			}
			file.isNew = false
			file.loaded = true
			file.data = []byte{}
		}
	}

	// If appending to existing file, load it and set offset to end
	if flag&os.O_APPEND != 0 && fileExists && flag&os.O_TRUNC == 0 {
		if useStagingPath && file.stagingSession != nil {
			// Get size from staging session
			file.offset = file.stagingSession.Size
		} else {
			// Legacy path: load from COS
			data, err := fs.ops.ReadFile(fs.requestContext(), fullPath, 0, 0)
			if err != nil {
				return nil, err
			}
			file.data = data
			file.loaded = true
			file.offset = int64(len(data))
		}
	}

	switch {
	case flag&os.O_CREATE != 0 && !fileExists:
		fs.changed(Change{Action: ChangeAdded, Path: fullPath})
	case flag&os.O_TRUNC != 0 && fileExists:
		fs.changed(Change{Action: ChangeModified, Path: fullPath, Kind: ChangeData})
	}
	return file, nil
}

// stagingFileInfo implements os.FileInfo for dirty staging files
type stagingFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	attrs   staging.StagedAttributes
}

// stagedFileInfo describes a file from its staging session.
func stagedFileInfo(name string, session *staging.WriteSession) *stagingFileInfo {
	attrs := session.Attributes()
	// A modification time a client set explicitly wins over the staged
	// file's own last write; writing clears it again.
	modTime := session.LastWrite
	if !attrs.Mtime.IsZero() {
		modTime = attrs.Mtime
	}
	return &stagingFileInfo{
		name:    name,
		size:    session.GetSize(),
		modTime: modTime,
		attrs:   attrs,
	}
}

func (s *stagingFileInfo) Name() string       { return s.name }
func (s *stagingFileInfo) Size() int64        { return s.size }
func (s *stagingFileInfo) Mode() os.FileMode  { return s.attrs.Mode }
func (s *stagingFileInfo) ModTime() time.Time { return s.modTime }
func (s *stagingFileInfo) IsDir() bool        { return false }
func (s *stagingFileInfo) Sys() interface{} {
	return nil
}

// Attributes reports the staged file's attributes, with its last write as the
// modification time and, when no creation time is recorded, the creation time.
func (s *stagingFileInfo) Attributes() types.POSIXAttributes {
	attrs := *s.attrs.POSIX()
	attrs.Mtime = s.modTime
	if attrs.Btime.IsZero() {
		attrs.Btime = s.modTime
	}
	return attrs
}

// stagingDirInfo is a synthetic directory answer derived from staged state:
// staged data under a path proves the directory exists even when the object
// store cannot be asked.
type stagingDirInfo struct {
	name    string
	modTime time.Time
	mode    os.FileMode
}

func (s *stagingDirInfo) Name() string       { return s.name }
func (s *stagingDirInfo) Size() int64        { return 0 }
func (s *stagingDirInfo) Mode() os.FileMode  { return s.mode }
func (s *stagingDirInfo) ModTime() time.Time { return s.modTime }
func (s *stagingDirInfo) IsDir() bool        { return true }
func (s *stagingDirInfo) Sys() interface{}   { return nil }

// Attributes reports default directory attributes for a synthetic directory.
func (s *stagingDirInfo) Attributes() types.POSIXAttributes {
	attrs := *posix.DefaultAttributes(true)
	attrs.Mode = s.mode
	attrs.Mtime = s.modTime
	attrs.Btime = s.modTime
	return attrs
}

// NFSOwner reports the staged file's owner to the NFS server.
func (s *stagingFileInfo) NFSOwner() (uid, gid uint32) { return posix.OwnerIDs(s.Attributes()) }

// NFSOwner reports the synthetic directory's (default) owner to the NFS
// server.
func (s *stagingDirInfo) NFSOwner() (uid, gid uint32) { return posix.OwnerIDs(s.Attributes()) }

// FileAttributes returns the attributes the filesystem reports for an entry
// from Stat or ReadDir: mode, owner, times, creation time, and Windows
// attribute flags. Entries without stored attributes report defaults, with
// their modification time as the creation time.
func FileAttributes(info os.FileInfo) types.POSIXAttributes {
	if attributed, ok := info.(interface{ Attributes() types.POSIXAttributes }); ok {
		return attributed.Attributes()
	}
	attrs := *posix.DefaultAttributes(info.IsDir())
	attrs.Mode = info.Mode()
	attrs.Mtime = info.ModTime()
	attrs.Btime = info.ModTime()
	return attrs
}

func (fs *Filesystem) isStagingDirty(fullPath string) bool {
	if fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil {
		if session, exists := fs.stagingManager.GetSession(fullPath); exists {
			// If session has any modifications not fully uploaded
			return session.Dirty || session.Size == 0
		}
	}
	return false
}

// isReservedPath hides gateway-internal bucket objects (the HA lease) from
// the NFS namespace so clients cannot read, delete, or overwrite them.
func isReservedPath(fullPath string) bool {
	return fullPath == "/"+ha.LeaseObjectKey
}

// Stat returns file information
func (fs *Filesystem) Stat(filename string) (os.FileInfo, error) {
	info, err := fs.statPath(fs.keyPath(filename), filename)
	if err != nil {
		return nil, err
	}
	return fs.presentEntry(info), nil
}

// statPath answers Stat for a resolved key path.
func (fs *Filesystem) statPath(fullPath, filename string) (os.FileInfo, error) {
	if isReservedPath(fullPath) {
		return nil, &os.PathError{Op: "stat", Path: filename, Err: os.ErrNotExist}
	}

	if fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil {
		if fs.stagingManager.HasPendingDelete(fullPath) {
			return nil, &os.PathError{Op: "stat", Path: filename, Err: os.ErrNotExist}
		}
		if session, exists := fs.stagingManager.GetSession(fullPath); exists && (session.Dirty || session.Size == 0 || session.Prefetched) {
			fs.logger.Debug("Stat intercepted by dirty staging session", zap.String("path", fullPath))
			return stagedFileInfo(filepath.Base(fullPath), session), nil
		}
	}

	info, err := fs.ops.Stat(fs.requestContext(), fullPath)
	if err != nil && !os.IsNotExist(err) {
		// The object store could not answer, but local staging state can
		// vouch for some paths: a retained staged session answers for the
		// file itself, and staged data below a path proves the directory
		// exists. This keeps the namespace (and file creation, which stats
		// the parent directory) working during a backend outage.
		if fallback := fs.statFromStaging(fullPath); fallback != nil {
			fs.logger.Error("Stat answered from staging state: object store unreachable",
				zap.String("path", fullPath), zap.Error(err))
			return fallback, nil
		}
	}
	return info, err
}

// statFromStaging answers a Stat from local staging knowledge when the object
// store cannot: an existing session for the exact path, or staged data under
// the path implying a directory.
func (fs *Filesystem) statFromStaging(fullPath string) os.FileInfo {
	if fs.featureFlags == nil || !fs.featureFlags.IsStagingEnabled() || fs.stagingManager == nil {
		return nil
	}

	if session, exists := fs.stagingManager.GetSession(fullPath); exists {
		return stagedFileInfo(filepath.Base(fullPath), session)
	}

	if len(fs.stagingManager.GetSessionsInDirectory(fullPath)) > 0 ||
		len(fs.stagingManager.DirtyPathsUnder(fullPath)) > 0 {
		attrs := posix.DefaultAttributes(true)
		return &stagingDirInfo{
			name:    filepath.Base(fullPath),
			modTime: attrs.Mtime,
			mode:    attrs.Mode | os.ModeDir,
		}
	}
	return nil
}

// copyFile copies src to dst inside the bucket, without the bytes passing
// through the gateway or the client. It is what SMB's server-side copy
// becomes when a whole file is being copied.
//
// It returns errors.ErrUnsupported when a bucket-side copy would not produce
// the same bytes a read-and-write would: the caller then copies the data
// itself. That is the case while the source is staged, because the object in
// COS is not what a reader would see, and while the destination holds staged
// bytes a client wrote.
//
// A destination that is staged but holds no written bytes, as when Windows
// creates the file and sets its length before asking for the copy, is
// discarded and replaced by the copy. Nothing is lost: its bytes are all
// zeros the copy overwrites. The caller must have closed its own handle on
// dst, or the staged session is still in use and the copy is declined. The
// copy carries the attributes the staged file would have synced with, as a
// copy through the staged file would.
//
// onlyUnwritten restricts the copy to that case, for a caller that knows
// only that the destination's current bytes are not worth keeping if nobody
// wrote them.
func (fs *Filesystem) copyFile(src, dst string, onlyUnwritten bool) error {
	srcFull := fs.keyPath(src)
	dstFull := fs.keyPath(dst)
	if isReservedPath(srcFull) || isReservedPath(dstFull) {
		return &os.PathError{Op: "copy", Path: src, Err: os.ErrPermission}
	}
	if srcFull == dstFull {
		return nil
	}

	var discarded *staging.DiscardedSession
	stagingEnabled := fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil
	if onlyUnwritten && !stagingEnabled {
		return errors.ErrUnsupported
	}
	if stagingEnabled {
		sm := fs.stagingManager
		// Staged state is the truth for the source, and the object in COS
		// may be older or absent.
		if sm.IsDirty(srcFull) || sm.HasPendingDelete(srcFull) || sm.IsConflicted(srcFull) {
			return errors.ErrUnsupported
		}
		// A pending delete would remove the copy once processed.
		if sm.HasPendingDelete(dstFull) || sm.IsConflicted(dstFull) {
			return errors.ErrUnsupported
		}

		_, staged := sm.GetSession(dstFull)
		if onlyUnwritten && !staged {
			return errors.ErrUnsupported
		}
		if staged || sm.IsDirty(dstFull) {
			// The claim keeps the sync worker from uploading the staged
			// destination while it is discarded, and from starting on it
			// before the copy lands. If a worker already holds it, an
			// upload may be in flight that would land after the copy.
			if !sm.TryLockSync(dstFull) {
				return errors.ErrUnsupported
			}
			defer sm.UnlockSync(dstFull)
			var ok bool
			discarded, ok = sm.DiscardUnwrittenSession(dstFull)
			if !ok || (onlyUnwritten && discarded == nil) {
				return errors.ErrUnsupported
			}
		}
	}

	var attrs *types.POSIXAttributes
	if discarded != nil {
		attrs = discarded.Attributes.POSIX()
	}
	if err := fs.ops.CopyFile(fs.requestContext(), srcFull, dstFull, attrs); err != nil {
		if discarded != nil {
			// Still under the sync claim: put the destination back as the
			// client left it, rather than exposing whatever the bucket held.
			if restoreErr := fs.stagingManager.RestoreUnwrittenSession(dstFull, discarded); restoreErr != nil {
				fs.logger.Error("Failed to restore discarded copy destination",
					"dst", dstFull, "error", restoreErr)
			}
		}
		return err
	}
	fs.logger.Info("Copied inside the bucket",
		"src", srcFull,
		"dst", dstFull,
		"discarded_unwritten_destination", discarded != nil)
	return nil
}

// IsStaged reports whether the gateway holds staged state for name: a
// session, bytes waiting to sync, a pending delete, or a conflict.
func (fs *Filesystem) IsStaged(name string) bool {
	if fs.featureFlags == nil || !fs.featureFlags.IsStagingEnabled() || fs.stagingManager == nil {
		return false
	}
	full := fs.keyPath(name)
	sm := fs.stagingManager
	if _, ok := sm.GetSession(full); ok {
		return true
	}
	return sm.IsDirty(full) || sm.HasPendingDelete(full) || sm.IsConflicted(full)
}

// rename renames a file or directory.
func (fs *Filesystem) rename(oldpath, newpath string) error {
	oldFull := fs.keyPath(oldpath)
	newFull := fs.renameTargetPath(oldFull, newpath)
	if isReservedPath(oldFull) || isReservedPath(newFull) {
		return &os.PathError{Op: "rename", Path: oldpath, Err: os.ErrPermission}
	}

	stagingEnabled := fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil
	if stagingEnabled {
		// Conflicted staged paths keep busy semantics until resolved.
		if fs.stagingManager.IsConflicted(oldFull) || fs.stagingManager.IsConflicted(newFull) {
			return &os.PathError{
				Op:   "rename",
				Path: oldFull,
				Err:  fmt.Errorf("staged path has unresolved conflict: %w", syscall.EBUSY),
			}
		}

		// Directory renames with dirty staged children stay blocked: moving a
		// tree would have to re-key every child atomically.
		if err := fs.ensureNoDirtyStagedChildren("rename", oldFull); err != nil {
			return err
		}
		if err := fs.ensureNoDirtyStagedChildren("rename", newFull); err != nil {
			return err
		}

		// A source with an accepted delete no longer exists.
		if fs.stagingManager.HasPendingDelete(oldFull) {
			return &os.PathError{Op: "rename", Path: oldpath, Err: os.ErrNotExist}
		}

		if fs.stagingManager.IsDirty(oldFull) {
			// POSIX write-back semantics: the staged bytes move to the new
			// name and a crash-safe tombstone retires the old COS object.
			return fs.renameDirtyStagedFile(oldFull, newFull)
		}

		// Clean source over staged destination state: rename-over discards
		// the destination's staged bytes and pending delete before the
		// object-store rename overwrites the object.
		if err := fs.discardStagedDestination(newFull); err != nil {
			return err
		}
	}

	return fs.ops.RenameFile(fs.requestContext(), oldFull, newFull)
}

// renameDirtyStagedFile renames a dirty staged source by re-keying the staged
// state and tombstoning the source object. No COS copy is needed: the staged
// bytes sync to the destination key.
func (fs *Filesystem) renameDirtyStagedFile(oldFull, newFull string) error {
	if err := fs.stagingManager.RenameStagedPath(oldFull, newFull); err != nil {
		if os.IsNotExist(err) {
			// Staged bytes vanished (stale dirty entry); fall back to the
			// object-store rename.
			return fs.ops.RenameFile(fs.requestContext(), oldFull, newFull)
		}
		return err
	}

	// Finish the source delete inline when no upload is in flight; otherwise
	// the sync worker completes it after the upload lands.
	if fs.stagingManager.TryLockSync(oldFull) {
		if err := fs.ops.DeleteFile(fs.requestContext(), oldFull); err != nil {
			fs.logger.Error("COS delete of rename source failed; sync worker will retry",
				zap.String("old_path", oldFull),
				zap.Error(err))
		} else {
			fs.stagingManager.ResolvePendingDelete(oldFull)
		}
		fs.stagingManager.UnlockSync(oldFull)
	} else {
		fs.ops.InvalidateFileMutation(oldFull)
		fs.logger.Info("Rename source delete deferred until in-flight sync completes",
			zap.String("old_path", oldFull))
	}

	fs.ops.InvalidateFileMutation(newFull)
	return nil
}

// discardStagedDestination drops staged state for a rename destination that is
// about to be overwritten by an object-store rename.
func (fs *Filesystem) discardStagedDestination(path string) error {
	sm := fs.stagingManager

	if sm.IsDirty(path) {
		// An in-flight upload of the doomed destination bytes could land
		// after the object-store copy and corrupt the renamed object, and a
		// clean source leaves no dirty entry behind to self-heal it. Keep the
		// retryable busy answer for that narrow window.
		if !sm.TryLockSync(path) {
			return &os.PathError{
				Op:   "rename",
				Path: path,
				Err:  fmt.Errorf("destination staged data is syncing; retry rename: %w", syscall.EBUSY),
			}
		}
		// Claim taken: nothing is uploading the doomed bytes. Discard them.
		// ForgetDirty releases the claim we just took, which is fine here
		// because no worker can have held it.
		sm.ForgetDirty(path, "rename_over_destination")
		if err := sm.CleanupSession(path, true); err != nil {
			fs.logger.Error("Failed to discard destination staged session before rename",
				zap.String("path", path),
				zap.Error(err))
		}
		sm.UnlockSync(path)
	}

	// A pending delete on the destination would remove the freshly renamed
	// object once processed; the rename supersedes it.
	sm.CancelPendingDelete(path)
	return nil
}

// remove removes a file or directory.
func (fs *Filesystem) remove(filename string) error {
	fullPath := fs.keyPath(filename)
	if isReservedPath(fullPath) {
		return &os.PathError{Op: "remove", Path: filename, Err: os.ErrPermission}
	}

	stagingEnabled := fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil
	if stagingEnabled && fs.stagingManager.IsDirty(fullPath) {
		// Only files are marked dirty, so this is an unlink. POSIX write-back
		// semantics: removing a dirty file discards the staged bytes instead
		// of failing the unlink. A durable tombstone keeps the delete
		// crash-safe until the COS object is confirmed gone.
		return fs.removeDirtyStagedFile(fullPath)
	}

	// The path itself is not dirty, so any dirty staged data under it means
	// this is a directory with dirty children: block the rmdir.
	if err := fs.ensureNoDirtyStagedData("rmdir", fullPath); err != nil {
		return err
	}

	// Check if it's a directory (intercepting Staging files via fs.Stat)
	info, err := fs.Stat(filename)
	if err != nil {
		if os.IsNotExist(err) {
			fs.logger.Debug("Remove called on non-existent path (likely implicit directory), treating as success", zap.String("path", fullPath))
			return nil
		}
		return err
	}

	if info.IsDir() {
		return fs.ops.DeleteDirectory(fs.requestContext(), fullPath)
	}

	if !stagingEnabled {
		if err := fs.cleanupSessionsBeforeDelete(fullPath); err != nil {
			fs.logger.Error("Failed to cleanup sessions before delete",
				zap.String("path", fullPath),
				zap.Error(err))
			return err
		}
	}

	if err := fs.ops.DeleteFile(fs.requestContext(), fullPath); err != nil {
		if stagingEnabled && !os.IsNotExist(err) {
			// The object store could not perform the delete (outage or
			// transient failure). Accept it write-back style: a durable
			// tombstone hides the path immediately and the sync worker
			// retires the object once the backend responds.
			if _, terr := fs.stagingManager.RegisterPendingDelete(fullPath); terr == nil {
				fs.ops.InvalidateFileMutation(fullPath)
				fs.logger.Error("Delete accepted via tombstone: object store unreachable",
					zap.String("path", fullPath), zap.Error(err))
				return nil
			}
		}
		return err
	}

	fs.cleanupCleanStagingSessionAfterDelete(fullPath)
	return nil
}

// removeDirtyStagedFile accepts the delete of a dirty staged file. The
// tombstone is persisted before any destructive step, so once this returns
// success the delete survives crashes and cannot resurrect the file.
func (fs *Filesystem) removeDirtyStagedFile(fullPath string) error {
	immediate, err := fs.stagingManager.RegisterPendingDelete(fullPath)
	if err != nil {
		return err
	}

	if !immediate {
		// An upload for this path is in flight; the sync worker completes the
		// delete after the upload finishes. Hide the path now.
		fs.ops.InvalidateFileMutation(fullPath)
		fs.logger.Info("Delete deferred until in-flight sync completes",
			zap.String("path", fullPath))
		return nil
	}

	if err := fs.ops.DeleteFile(fs.requestContext(), fullPath); err != nil {
		// The tombstone persists; the sync worker retries the COS delete.
		fs.logger.Error("COS delete failed after tombstone was accepted; sync worker will retry",
			zap.String("path", fullPath),
			zap.Error(err))
		return nil
	}

	fs.stagingManager.ResolvePendingDelete(fullPath)
	fs.cleanupCleanStagingSessionAfterDelete(fullPath)
	return nil
}

// ensureNoDirtyStagedChildren blocks operations on a directory tree that has
// dirty staged files strictly below path (path itself is allowed).
func (fs *Filesystem) ensureNoDirtyStagedChildren(op, path string) error {
	if fs.featureFlags == nil || !fs.featureFlags.IsStagingEnabled() || fs.stagingManager == nil {
		return nil
	}

	dirtyPaths := fs.stagingManager.DirtyPathsUnder(path)
	children := dirtyPaths[:0]
	for _, dirtyPath := range dirtyPaths {
		if dirtyPath != path {
			children = append(children, dirtyPath)
		}
	}
	if len(children) == 0 {
		return nil
	}

	return &os.PathError{
		Op:   op,
		Path: path,
		Err:  fmt.Errorf("dirty staged data exists at %s; wait for sync before %s: %w", strings.Join(children, ","), op, syscall.EBUSY),
	}
}

func (fs *Filesystem) ensureNoDirtyStagedData(op, path string) error {
	if fs.featureFlags == nil || !fs.featureFlags.IsStagingEnabled() || fs.stagingManager == nil {
		return nil
	}

	dirtyPaths := fs.stagingManager.DirtyPathsUnder(path)
	if len(dirtyPaths) == 0 {
		return nil
	}

	return &os.PathError{
		Op:   op,
		Path: path,
		Err:  fmt.Errorf("dirty staged data exists at %s; wait for sync before %s: %w", strings.Join(dirtyPaths, ","), op, syscall.EBUSY),
	}
}

func (fs *Filesystem) cleanupCleanStagingSessionAfterDelete(path string) {
	if fs.featureFlags == nil || !fs.featureFlags.IsStagingEnabled() || fs.stagingManager == nil {
		return
	}
	if dirtyPaths := fs.stagingManager.DirtyPathsUnder(path); len(dirtyPaths) > 0 {
		return
	}
	if _, exists := fs.stagingManager.GetSession(path); !exists {
		return
	}
	if err := fs.stagingManager.CleanupSession(path, true); err != nil {
		fs.logger.Error("Failed to cleanup clean staging session after delete",
			zap.String("path", path),
			zap.Error(err))
	}
}

// cleanupSessionsBeforeDelete ensures any active sessions are flushed before file deletion.
func (fs *Filesystem) cleanupSessionsBeforeDelete(path string) error {
	ctx := fs.requestContext()

	// Handle staging path (new architecture)
	if fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil {
		session, exists := fs.stagingManager.GetSession(path)
		if exists && session.Dirty {
			fs.logger.Info("Flushing staging session before delete",
				zap.String("path", path),
				zap.Int64("size", session.Size))

			// Sync session to staging file
			if err := session.Sync(); err != nil {
				return fmt.Errorf("failed to sync staging session: %w", err)
			}

			// If sync worker is available, trigger immediate upload
			if fs.syncWorker != nil {
				// Read staging file
				data, err := os.ReadFile(session.StagingPath)
				if err != nil {
					return fmt.Errorf("failed to read staging file: %w", err)
				}

				// Upload directly to COS
				metadata := make(map[string]string)
				if err := fs.syncWorker.UploadToCOS(ctx, path, data, metadata); err != nil {
					return fmt.Errorf("failed to upload to COS: %w", err)
				}

				fs.logger.Info("Uploaded staging file to COS before delete",
					zap.String("path", path),
					zap.Int("size", len(data)))
			}

			// Cleanup session and staging file
			if err := fs.stagingManager.CleanupSession(path, true); err != nil {
				fs.logger.Error("Failed to cleanup staging session",
					zap.String("path", path),
					zap.Error(err))
			}
		}
		return nil
	}

	// Handle legacy buffer path
	if fs.sessionManager != nil {
		session, exists := fs.sessionManager.GetSession(path)
		if exists {
			session.Mu.Lock()
			bufferSize := session.Buffer.Size()
			session.Mu.Unlock()

			if bufferSize > 0 {
				fs.logger.Info("Flushing legacy buffer session before delete",
					zap.String("path", path),
					zap.Int64("buffer_size", bufferSize))

				// Get flush data from buffer
				session.Mu.Lock()
				data, _, err := session.Buffer.GetFlushData()
				session.Mu.Unlock()

				if err != nil {
					return fmt.Errorf("failed to get flush data: %w", err)
				}

				if len(data) > 0 {
					// Get current file attributes
					attrs := &types.POSIXAttributes{
						Mode:  0644,
						UID:   1000,
						GID:   1000,
						Mtime: time.Now(),
					}

					// Upload to COS
					if err := fs.ops.WriteFile(ctx, path, data, attrs); err != nil {
						return fmt.Errorf("failed to flush buffer to COS: %w", err)
					}

					fs.logger.Info("Flushed legacy buffer to COS before delete",
						zap.String("path", path),
						zap.Int("size", len(data)))
				}
			}

			// Close the session
			fs.sessionManager.CloseSession(path)
		}
	}

	return nil
}

// Join joins path elements
func (fs *Filesystem) Join(elem ...string) string {
	return filepath.Join(elem...)
}

// TempFile creates a temporary file
func (fs *Filesystem) TempFile(dir, prefix string) (billy.File, error) {
	// Generate a unique temporary filename
	tempName := fmt.Sprintf("%s%d", prefix, os.Getpid())
	fullPath := fs.Join(dir, tempName)
	return fs.Create(fullPath)
}

// ReadDir reads directory contents
func (fs *Filesystem) ReadDir(path string) ([]os.FileInfo, error) {
	start := time.Now()

	fullPath := fs.keyPath(path)

	// Track per-path calls
	metrics.GetGlobalCounters().RecordPathCall(fullPath)

	// Take the staged sessions before listing COS. A file whose sync
	// completes in between is then either still in this snapshot or already
	// in the object store the listing reads (the sync drops any cached
	// listing that lacks it); snapshotting afterwards could miss it in both.
	stagingEnabled := fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil
	var stagingSessions []*staging.WriteSession
	if stagingEnabled {
		stagingSessions = fs.stagingManager.GetSessionsInDirectory(fullPath)
	}

	listStart := time.Now()
	entries, err := fs.ops.ListDirectory(fs.requestContext(), fullPath)
	listDuration := time.Since(listStart)

	if err != nil {
		// If the object store cannot answer but staged sessions exist under
		// this directory, serve the staged entries: a partial listing keeps
		// applications working during a backend outage.
		if stagingEnabled {
			if staged := stagingSessions; len(staged) > 0 {
				fs.logger.Error("ReadDir serving staged entries only: object store unreachable",
					zap.String("path", fullPath), zap.Error(err))
				result := make([]os.FileInfo, 0, len(staged))
				for _, session := range staged {
					if fs.stagingManager.HasPendingDelete(session.Path) {
						continue
					}
					result = append(result, stagedFileInfo(filepath.Base(session.Path), session))
				}
				duration := time.Since(start)
				RecordReaddirCall(fullPath, len(result), duration, nil)
				metrics.RecordReadDir(duration)
				return fs.presentEntries(result), nil
			}
		}
		// Record trace even on error
		duration := time.Since(start)
		RecordReaddirCall(fullPath, 0, duration, err)
		metrics.RecordReadDir(duration)
		return nil, err
	}

	// Convert []*posix.FileInfo to []os.FileInfo, hiding reserved
	// gateway-internal objects from the namespace.
	convStart := time.Now()
	result := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		if isReservedPath(fs.Join(fullPath, entry.Name())) {
			continue
		}
		result = append(result, entry)
	}

	// Safely inject StagingManager Memory bounds natively into directories!
	if stagingEnabled {
		// Hide paths whose delete was accepted but not yet confirmed in COS.
		if fs.stagingManager.PendingDeleteCount() > 0 {
			visible := result[:0]
			for _, entry := range result {
				if fs.stagingManager.HasPendingDelete(fs.Join(fullPath, entry.Name())) {
					continue
				}
				visible = append(visible, entry)
			}
			result = visible
		}

		for _, session := range stagingSessions {
			if fs.stagingManager.HasPendingDelete(session.Path) {
				continue
			}
			// Ensure it doesn't already natively exist in COS results safely
			exists := false
			sessionName := filepath.Base(session.Path)
			for _, entry := range result {
				if entry.Name() == sessionName {
					exists = true
					break
				}
			}
			if !exists && (session.Dirty || session.Size == 0 || session.Prefetched) {
				result = append(result, stagedFileInfo(sessionName, session))
			}
		}
	}
	convDuration := time.Since(convStart)

	duration := time.Since(start)

	// Record trace with entry count
	RecordReaddirCall(fullPath, len(result), duration, nil)

	// Record metrics
	metrics.RecordReadDir(duration)
	metrics.RecordConversion(convDuration)

	// Log slow calls or first few calls
	counters := metrics.GetGlobalCounters()
	callCount := counters.ReadDirCalls.Load()
	if duration > 10*time.Millisecond || callCount <= 5 {
		fs.logger.Info("ReadDir call",
			"path", path,
			"duration_ms", duration.Milliseconds(),
			"call_number", callCount,
			"entries", len(result))
	}

	// Log timing breakdown for slow calls
	if duration > 10*time.Millisecond {
		fs.logger.Info("ReadDir timing breakdown",
			"path", path,
			"total_ms", duration.Milliseconds(),
			"list_ms", listDuration.Milliseconds(),
			"conversion_ms", convDuration.Milliseconds(),
			"entries", len(entries))
	}

	return fs.presentEntries(result), nil
}

// mkdirAll creates a directory and all parent directories.
func (fs *Filesystem) mkdirAll(filename string, perm os.FileMode) error {
	fullPath := fs.keyPath(filename)
	now := time.Now()
	attrs := &types.POSIXAttributes{
		Mode:  perm | os.ModeDir,
		UID:   1000,
		GID:   1000,
		Mtime: now,
		Btime: now,
	}
	return fs.ops.CreateDirectory(fs.requestContext(), fullPath, attrs)
}

// Lstat returns file information (same as Stat for COS)
func (fs *Filesystem) Lstat(filename string) (os.FileInfo, error) {
	return fs.Stat(filename)
}

// Symlink creates a symbolic link (not supported in COS)
func (fs *Filesystem) Symlink(target, link string) error {
	return fmt.Errorf("symlinks not supported")
}

// Readlink reads a symbolic link (not supported in COS)
func (fs *Filesystem) Readlink(link string) (string, error) {
	return "", fmt.Errorf("symlinks not supported")
}

// Chroot creates a chrooted filesystem
func (fs *Filesystem) Chroot(path string) (billy.Filesystem, error) {
	return fs.WithRoot(path), nil
}

// WithRoot returns a view of the directory at path: its root is that
// directory, and changes outside it are not reported to its subscribers as
// paths of their own. The view shares everything else, including the change
// feed and naming, with fs.
func (fs *Filesystem) WithRoot(path string) *Filesystem {
	view := *fs
	view.root = fs.keyPath(path)
	return &view
}

// Root returns the root path
func (fs *Filesystem) Root() string {
	return fs.root
}

// Chmod changes the mode of the named file
func (fs *Filesystem) Chmod(name string, mode os.FileMode) error {
	return fs.SetAttributes(name, posix.AttributeUpdate{Mode: &mode})
}

// setAttributes applies an attribute change to the named file or directory. A
// staged file records the change for its next sync (access and modification
// times are not staged); anything else gets a metadata-only update in COS
// that does not rewrite the object's bytes.
func (fs *Filesystem) setAttributes(name string, update posix.AttributeUpdate) error {
	fullPath := fs.keyPath(name)

	if fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil {
		if session, exists := fs.stagingManager.GetSession(fullPath); exists {
			applyStagedUpdate(session, update)
			return nil
		}
	}

	return fs.ops.UpdateAttributes(fs.requestContext(), fullPath, update)
}

// applyStagedUpdate records an attribute change on a staged file.
func applyStagedUpdate(session *staging.WriteSession, update posix.AttributeUpdate) {
	if update.Mode != nil {
		session.SetMode(*update.Mode)
	}
	if update.UID != nil || update.GID != nil {
		current := session.Attributes()
		uid, gid := current.UID, current.GID
		if update.UID != nil {
			uid = uint32(*update.UID)
		}
		if update.GID != nil {
			gid = uint32(*update.GID)
		}
		session.SetOwner(uid, gid)
	}
	if update.Btime != nil {
		session.SetBirthTime(*update.Btime)
	}
	if update.WindowsAttributes != nil {
		session.SetWindowsAttributes(*update.WindowsAttributes & posix.WindowsAttributesStored)
	}
	if update.Streams != nil {
		session.SetStreams(update.Streams)
	}
	if update.Atime != nil || update.Mtime != nil {
		var atime, mtime time.Time
		if update.Atime != nil {
			atime = *update.Atime
		}
		if update.Mtime != nil {
			mtime = *update.Mtime
		}
		session.SetTimes(atime, mtime)
	}
}

// Lchown changes the uid and gid of the named file (link itself)
func (fs *Filesystem) Lchown(name string, uid, gid int) error {
	// COS doesn't support symlinks, so this is the same as Chown
	return fs.Chown(name, uid, gid)
}

// Chown changes the uid and gid of the named file
func (fs *Filesystem) Chown(name string, uid, gid int) error {
	return fs.SetAttributes(name, posix.AttributeUpdate{UID: &uid, GID: &gid})
}

// Chtimes changes the access and modification times
func (fs *Filesystem) Chtimes(name string, atime time.Time, mtime time.Time) error {
	// SetAttributes records the times on the staging session when the file is
	// staged, and updates the object's metadata otherwise.
	return fs.SetAttributes(name, posix.AttributeUpdate{Atime: &atime, Mtime: &mtime})
}

// File implements billy.File interface
type File struct {
	ops            *posix.OperationsHandler
	logger         *logging.KVLogger
	path           string
	flag           int
	perm           os.FileMode
	offset         int64
	isNew          bool
	data           []byte
	loaded         bool
	size           int64                     // File size (for read-only files without data loaded)
	writeSession   *buffer.WriteSession      // Shared write session (survives handle close) - LEGACY
	flushCount     int                       // Number of flushes performed
	totalFlushed   int64                     // Total bytes flushed
	totalWrites    int                       // Total number of Write() calls
	perfConfig     *config.PerformanceConfig // Performance configuration
	fileID         string                    // Unique file handle ID for tracking
	sessionManager *buffer.SessionManager    // Session manager for path-scoped buffering - LEGACY
	// Staging architecture components
	stagingSession *staging.WriteSession // Staging write session
	stagingManager *staging.StagingManager
	syncWorker     *staging.SyncWorker
	featureFlags   *feature.FeatureFlags
	// protocol attributes this handle's requests in metrics.
	protocol string

	// changes receives the handle's writes, and wrote records that one has
	// been published, so a burst of writes is one change until Close.
	changes *changeFeed
	wrote   bool

	// mu guards the mutable handle state above (offset, size, data, loaded,
	// isNew, writeSession, counters) and closed, so one open file can serve
	// concurrent requests, as SMB clients send on a single handle.
	mu     sync.Mutex
	closed bool
}

// requestContext carries the handle's protocol to the operations it calls.
func (f *File) requestContext() context.Context {
	return metrics.WithProtocol(context.Background(), f.protocol)
}

// Name returns the file name
func (f *File) Name() string {
	return filepath.Base(f.path)
}

func (f *File) maxBufferedWriteBytes() int64 {
	limitMB := config.DefaultMaxBufferedWriteMB
	if f.perfConfig != nil && f.perfConfig.MaxBufferedWriteMB > 0 {
		limitMB = f.perfConfig.MaxBufferedWriteMB
	}
	return int64(limitMB) * 1024 * 1024
}

// Read reads data from the file
func (f *File) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}

	// STAGING PATH: Check staging session first for dirty files
	if f.featureFlags != nil && f.featureFlags.IsStagingEnabled() && f.stagingSession != nil {
		// Read from staging session
		n, err := f.stagingSession.Read(p, f.offset)
		if err != nil && err != io.EOF {
			f.logger.Error("Failed to read from staging session",
				"path", f.path,
				"offset", f.offset,
				"error", err)
			return 0, err
		}
		f.offset += int64(n)

		f.logger.Debug("Read from staging session",
			"path", f.path,
			"offset", f.offset-int64(n),
			"bytes", n)

		return n, err
	}

	// LEGACY PATH: Original read logic
	if err := f.ensureLoaded(); err != nil {
		return 0, err
	}

	// Check write session buffer first for read-after-write consistency
	if f.writeSession != nil {
		f.writeSession.Mu.Lock()
		bufferedData := f.writeSession.Buffer.Read(f.offset, int64(len(p)))
		f.writeSession.Mu.Unlock()

		if len(bufferedData) > 0 {
			n := copy(p, bufferedData)
			f.offset += int64(n)
			f.logger.Debug("Read from write session buffer",
				"path", f.path,
				"session_id", f.writeSession.SessionID,
				"offset", f.offset-int64(n),
				"bytes", n)
			return n, nil
		}
	}

	// If data is loaded in memory (writable file), use it
	if f.data != nil {
		if f.offset >= int64(len(f.data)) {
			return 0, io.EOF
		}
		n := copy(p, f.data[f.offset:])
		f.offset += int64(n)
		return n, nil
	}

	// Check if we're at or past EOF
	if f.offset >= f.size {
		return 0, io.EOF
	}

	// Read-only file: fetch data on-demand using range read
	data, err := f.ops.ReadFile(f.requestContext(), f.path, f.offset, int64(len(p)))
	if err != nil {
		// Check if it's EOF (no more data to read)
		if err.Error() == "EOF" || strings.Contains(err.Error(), "EOF") {
			return 0, io.EOF
		}
		return 0, err
	}

	// If we got no data, we're at EOF
	if len(data) == 0 {
		return 0, io.EOF
	}

	n := copy(p, data)
	f.offset += int64(n)

	// IMPORTANT: Per io.Reader contract, don't return EOF with data
	// Return data with nil error, EOF will be returned on next call
	return n, nil
}

// Write writes data at the handle offset and advances it.
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	n, err := f.writeLocked(p, f.offset)
	f.offset += int64(n)
	f.noteWrite(n)
	return n, err
}

// WriteAt writes data at off without moving the handle offset, so protocols
// with positional writes can share one open file across concurrent requests.
func (f *File) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, &os.PathError{Op: "writeat", Path: f.path, Err: os.ErrInvalid}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	n, err := f.writeLocked(p, off)
	f.noteWrite(n)
	return n, err
}

// writeLocked writes data at offset with session-based buffering. The caller
// holds f.mu and owns the handle offset.
func (f *File) writeLocked(p []byte, offset int64) (int, error) {
	f.totalWrites++

	// STAGING PATH: Write to staging session
	if f.featureFlags != nil && f.featureFlags.IsStagingEnabled() && f.stagingSession != nil {
		n, err := f.stagingSession.Write(p, offset)
		if err != nil {
			f.logger.Error("STAGING WRITE ERROR",
				"file_id", f.fileID,
				"path", f.path,
				"offset", offset,
				"bytes", len(p),
				"error", err)
			return 0, err
		}

		// Other handles may be writing the same session; read its size
		// under the session lock.
		sessionSize := f.stagingSession.GetSize()

		// Mark file as dirty and update size
		f.stagingManager.MarkDirty(f.path, sessionSize)

		f.logger.Info("STAGING WRITE",
			"file_id", f.fileID,
			"path", f.path,
			"offset", offset,
			"bytes", n,
			"session_size", sessionSize,
			"write_count", f.totalWrites)

		return n, nil
	}

	// LEGACY PATH: Original write logic with write buffer
	// NOTE: We do NOT call ensureLoaded() here because:
	// 1. Write buffer handles append-only writes efficiently
	// 2. We only need to download the file during flush (read-modify-write)
	// 3. Downloading on every write causes catastrophic performance issues

	// Get or create write session for this path
	if f.writeSession == nil {
		f.writeSession = f.sessionManager.GetOrCreateSession(f.path)
		f.logger.Info("FILE OPEN - Write session acquired",
			"file_id", f.fileID,
			"session_id", f.writeSession.SessionID,
			"path", f.path)
	}

	// Write to session buffer (thread-safe)
	f.writeSession.Mu.Lock()
	n, err := f.writeSession.Buffer.Write(offset, p)
	shouldFlush := f.writeSession.Buffer.ShouldFlush()
	bufferSize := f.writeSession.Buffer.Size()
	f.writeSession.Mu.Unlock()

	if err != nil {
		f.logger.Error("WRITE ERROR",
			"file_id", f.fileID,
			"session_id", f.writeSession.SessionID,
			"path", f.path,
			"offset", offset,
			"bytes", len(p),
			"error", err)
		return 0, err
	}

	f.logger.Info("WRITE",
		"file_id", f.fileID,
		"session_id", f.writeSession.SessionID,
		"path", f.path,
		"offset", offset,
		"bytes", n,
		"buffer_size_bytes", bufferSize,
		"buffer_size_mb", float64(bufferSize)/(1024*1024),
		"write_count", f.totalWrites)

	// Check if we should flush
	if shouldFlush {
		thresholdBytes := int64(f.perfConfig.WriteBufferKB) * 1024
		f.logger.Info("FLUSH TRIGGER: threshold reached",
			"file_id", f.fileID,
			"session_id", f.writeSession.SessionID,
			"path", f.path,
			"buffer_size_bytes", bufferSize,
			"buffer_size_mb", float64(bufferSize)/(1024*1024),
			"threshold_bytes", thresholdBytes,
			"threshold_mb", float64(thresholdBytes)/(1024*1024))

		if err := f.flushSessionBuffer(); err != nil {
			f.logger.Error("FLUSH ERROR",
				"file_id", f.fileID,
				"session_id", f.writeSession.SessionID,
				"path", f.path,
				"error", err)
			return n, err
		}
	}

	return n, nil
}

// flushSessionBuffer flushes the write session buffer to COS
func (f *File) flushSessionBuffer() error {
	if f.writeSession == nil {
		return nil
	}

	f.writeSession.Mu.Lock()
	bufferSize := f.writeSession.Buffer.Size()
	if bufferSize == 0 {
		f.writeSession.Mu.Unlock()
		return nil
	}

	start := time.Now()

	// Get data to flush
	data, startOffset, err := f.writeSession.Buffer.GetFlushData()
	if err != nil {
		f.writeSession.Mu.Unlock()
		f.logger.Error("Failed to get flush data",
			"file_id", f.fileID,
			"session_id", f.writeSession.SessionID,
			"path", f.path,
			"error", err)
		return err
	}
	flushSize := int64(len(data))
	f.writeSession.Mu.Unlock()

	f.logger.Info("FLUSH START",
		"file_id", f.fileID,
		"session_id", f.writeSession.SessionID,
		"path", f.path,
		"bytes", flushSize,
		"start_offset", startOffset,
		"flush_count", f.flushCount+1)

	// Check current file size in COS
	var currentSize int64
	if !f.isNew {
		info, err := f.ops.Stat(f.requestContext(), f.path)
		if err != nil && !strings.Contains(err.Error(), "not found") {
			f.logger.Error("Failed to stat file during flush",
				"file_id", f.fileID,
				"session_id", f.writeSession.SessionID,
				"path", f.path,
				"error", err)
			return err
		}
		if err == nil {
			currentSize = info.Size()
		}
	}

	// For new files or full rewrites, just write the data
	if f.isNew || startOffset == 0 {
		attrs := &types.POSIXAttributes{
			Mode:  f.perm,
			UID:   1000,
			GID:   1000,
			Mtime: time.Now(),
		}

		err := f.ops.WriteFile(f.requestContext(), f.path, data, attrs)
		if err != nil {
			f.logger.Error("Failed to write file during flush",
				"file_id", f.fileID,
				"session_id", f.writeSession.SessionID,
				"path", f.path,
				"bytes", len(data),
				"error", err)
			return err
		}
	} else if startOffset == currentSize && currentSize > 0 {
		// SEQUENTIAL APPEND: startOffset equals current file size
		// We can optimize this by reading once and appending
		mergedSize := currentSize + flushSize
		if mergedSize > f.maxBufferedWriteBytes() {
			return fmt.Errorf("buffered append merge for %s would allocate %d bytes, exceeding max_buffered_write_mb=%d; enable staging or raise the limit",
				f.path, mergedSize, f.maxBufferedWriteBytes()/(1024*1024))
		}

		f.logger.Info("SEQUENTIAL APPEND detected",
			"file_id", f.fileID,
			"path", f.path,
			"current_size", currentSize,
			"start_offset", startOffset,
			"append_bytes", flushSize)

		// Read existing file once
		existingData, err := f.ops.ReadFile(f.requestContext(), f.path, 0, 0)
		if err != nil {
			f.logger.Error("Failed to read existing file for sequential append",
				"file_id", f.fileID,
				"path", f.path,
				"error", err)
			return err
		}

		// Append new data
		finalData := make([]byte, len(existingData)+len(data))
		copy(finalData, existingData)
		copy(finalData[len(existingData):], data)

		attrs := &types.POSIXAttributes{
			Mode:  f.perm,
			UID:   1000,
			GID:   1000,
			Mtime: time.Now(),
		}

		err = f.ops.WriteFile(f.requestContext(), f.path, finalData, attrs)
		if err != nil {
			f.logger.Error("Failed to write file during sequential append",
				"file_id", f.fileID,
				"session_id", f.writeSession.SessionID,
				"path", f.path,
				"bytes", len(finalData),
				"error", err)
			return err
		}
	} else {
		// For random writes/updates, we need to read existing data and merge
		// This is a limitation of COS - we can't do partial updates
		needed := startOffset + int64(len(data))
		if needed > f.maxBufferedWriteBytes() {
			return fmt.Errorf("buffered random-write merge for %s would allocate %d bytes, exceeding max_buffered_write_mb=%d; enable staging or raise the limit",
				f.path, needed, f.maxBufferedWriteBytes()/(1024*1024))
		}

		f.logger.Info("RANDOM WRITE detected - requires full file download",
			"file_id", f.fileID,
			"path", f.path,
			"current_size", currentSize,
			"start_offset", startOffset,
			"write_bytes", flushSize)

		existingData, err := f.ops.ReadFile(f.requestContext(), f.path, 0, 0)
		if err != nil && !strings.Contains(err.Error(), "not found") {
			f.logger.Error("Failed to read existing file for merge",
				"file_id", f.fileID,
				"session_id", f.writeSession.SessionID,
				"path", f.path,
				"error", err)
			return err
		}

		// Merge the data
		if needed > int64(len(existingData)) {
			newData := make([]byte, needed)
			copy(newData, existingData)
			existingData = newData
		}
		copy(existingData[startOffset:], data)

		attrs := &types.POSIXAttributes{
			Mode:  f.perm,
			UID:   1000,
			GID:   1000,
			Mtime: time.Now(),
		}

		err = f.ops.WriteFile(f.requestContext(), f.path, existingData, attrs)
		if err != nil {
			f.logger.Error("Failed to write merged file during flush",
				"file_id", f.fileID,
				"session_id", f.writeSession.SessionID,
				"path", f.path,
				"bytes", len(existingData),
				"error", err)
			return err
		}
	}

	duration := time.Since(start)
	f.flushCount++
	f.totalFlushed += flushSize

	f.logger.Info("FLUSH COMPLETE",
		"file_id", f.fileID,
		"session_id", f.writeSession.SessionID,
		"path", f.path,
		"bytes", flushSize,
		"duration_ms", duration.Milliseconds(),
		"throughput_mbps", float64(flushSize)/duration.Seconds()/1024/1024,
		"total_flushes", f.flushCount,
		"total_flushed", f.totalFlushed)

	// Clear the session buffer after successful flush
	f.writeSession.Mu.Lock()
	thresholdBytes := int64(f.perfConfig.WriteBufferKB) * 1024
	f.writeSession.Buffer = buffer.NewWriteBuffer(thresholdBytes)
	f.writeSession.Mu.Unlock()

	return nil
}

// Close closes the file and releases the write session
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A repeated Close must not release the shared staging session again:
	// that would drop a reference another handle still holds.
	if f.closed {
		return nil
	}
	if f.wrote {
		// Watchers saw the first write; this shows them the final size.
		defer f.changes.publish(Change{Action: ChangeModified, Path: f.path, Kind: ChangeData, Protocol: f.protocol})
	}
	f.closed = true

	// STAGING PATH: Release staging session
	if f.featureFlags != nil && f.featureFlags.IsStagingEnabled() && f.stagingSession != nil {
		sessionSize := f.stagingSession.GetSize()
		isDirty := f.stagingSession.Dirty
		refCount := f.stagingSession.GetRefCount()

		f.logger.Debug("FILE CLOSE - Releasing staging session",
			"file_id", f.fileID,
			"path", f.path,
			"session_size", sessionSize,
			"ref_count", refCount,
			"dirty", isDirty)

		// Zero-byte files stay in staging like every other write and are
		// uploaded by the background sync worker (the idle trigger covers
		// them within seconds). The old synchronous zero-byte upload here
		// ran on every create's transient open/close (twice per create),
		// costing two COS PUTs plus full ancestor-cache invalidation, and
		// its durability semantics were no stronger than write-back. NFS
		// attribute operations are answered from the staging session.

		// Release session reference (session persists for other handles)
		f.stagingManager.ReleaseSession(f.path)

		// Log final statistics for this handle
		if f.totalWrites > 0 {
			f.logger.Debug("File handle closed with write statistics",
				"file_id", f.fileID,
				"path", f.path,
				"total_writes", f.totalWrites,
				"session_size", sessionSize)
		}

		return nil
	}

	// LEGACY PATH: Release write session (flush now to prevent small data loss)
	if f.writeSession != nil {
		if err := f.flushSessionBuffer(); err != nil {
			f.logger.Error("Failed to flush session buffer on close", "error", err)
			// Buffered data is still held; leave the handle open for a retry.
			f.closed = false
			return err
		}

		f.writeSession.Mu.Lock()
		bufferSize := f.writeSession.Buffer.Size()
		sessionID := f.writeSession.SessionID
		f.writeSession.Mu.Unlock()

		f.logger.Info("FILE CLOSE - Releasing write session",
			"file_id", f.fileID,
			"session_id", sessionID,
			"path", f.path,
			"buffer_size_bytes", bufferSize,
			"buffer_size_mb", float64(bufferSize)/(1024*1024))

		// Release session reference (session persists for other handles)
		f.sessionManager.ReleaseSession(f.path)
	}

	// Log final statistics for this handle
	if f.flushCount > 0 {
		f.logger.Debug("File handle closed with write statistics",
			"file_id", f.fileID,
			"path", f.path,
			"total_flushes", f.flushCount,
			"total_bytes", f.totalFlushed,
			"total_writes", f.totalWrites,
			"avg_flush_size", f.totalFlushed/int64(f.flushCount))
	}

	// Legacy path: handle old-style in-memory data if present
	if f.flag&(os.O_WRONLY|os.O_RDWR) != 0 && len(f.data) > 0 && f.writeSession == nil && f.stagingSession == nil {
		attrs := &types.POSIXAttributes{
			Mode:  f.perm,
			UID:   1000,
			GID:   1000,
			Mtime: time.Now(),
		}
		err := f.ops.WriteFile(f.requestContext(), f.path, f.data, attrs)
		if err != nil {
			return err
		}
	}

	return nil
}

// Seek sets the file offset
func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	if err := f.ensureLoaded(); err != nil && !f.isNew {
		return 0, err
	}

	fileSize := f.currentSizeLocked()

	switch whence {
	case io.SeekStart:
		f.offset = offset
	case io.SeekCurrent:
		f.offset += offset
	case io.SeekEnd:
		f.offset = fileSize + offset
	default:
		return 0, fmt.Errorf("invalid whence")
	}

	if f.offset < 0 {
		f.offset = 0
	}

	return f.offset, nil
}

// currentSizeLocked returns the live file size. Staged bytes change through
// this and other handles, so the size cached at load time goes stale.
func (f *File) currentSizeLocked() int64 {
	switch {
	case f.featureFlags != nil && f.featureFlags.IsStagingEnabled() && f.stagingSession != nil:
		return f.stagingSession.GetSize()
	case f.data != nil:
		return int64(len(f.data))
	default:
		return f.size
	}
}

// Lock locks the file (no-op for COS)
func (f *File) Lock() error {
	return nil
}

// Unlock unlocks the file (no-op for COS)
func (f *File) Unlock() error {
	return nil
}

// ReadAt reads data from the file at a specific offset
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	// Hold the handle lock only while loading state. The staging and object
	// reads below are positional, so concurrent ReadAt calls on one handle
	// run in parallel.
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return 0, os.ErrClosed
	}
	if err := f.ensureLoaded(); err != nil {
		f.mu.Unlock()
		return 0, err
	}

	// If data is loaded in memory (writable file), use it
	if f.data != nil {
		defer f.mu.Unlock()
		if off >= int64(len(f.data)) {
			return 0, io.EOF
		}
		n := copy(p, f.data[off:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}
	size := f.size
	f.mu.Unlock()

	// Read from local staging session natively to support FIO caching guarantees
	if f.featureFlags != nil && f.featureFlags.IsStagingEnabled() && f.stagingSession != nil {
		n, err := f.stagingSession.Read(p, off)
		if err == nil || (err == io.EOF && n > 0) {
			return n, err
		}
		if err == io.EOF && n == 0 {
			return 0, io.EOF
		}
	}

	// Check if we're at or past EOF
	if off >= size {
		return 0, io.EOF
	}

	// Read-only file: fetch data on-demand using range read
	data, err := f.ops.ReadFile(f.requestContext(), f.path, off, int64(len(p)))
	if err != nil {
		// Check if it's EOF (no more data to read)
		if err.Error() == "EOF" || strings.Contains(err.Error(), "EOF") {
			return 0, io.EOF
		}
		return 0, err
	}

	// If we got no data, we're at EOF
	if len(data) == 0 {
		return 0, io.EOF
	}

	n := copy(p, data)

	// Per io.ReaderAt contract: return EOF only if no bytes were read
	// If we read some bytes but less than requested, that's still success
	// The caller will detect EOF on the next call when off >= size
	if n < len(p) && off+int64(n) >= size {
		return n, io.EOF
	}

	return n, nil
}

// truncate changes the file size, shrinking or zero-extending it. NFS
// SETATTR size (truncate(1), ftruncate) arrives here.
func (f *File) truncate(size int64) error {
	if size < 0 {
		return &os.PathError{Op: "truncate", Path: f.path, Err: os.ErrInvalid}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return os.ErrClosed
	}
	if f.flag&(os.O_WRONLY|os.O_RDWR) == 0 {
		return &os.PathError{Op: "truncate", Path: f.path, Err: os.ErrPermission}
	}

	if f.featureFlags != nil && f.featureFlags.IsStagingEnabled() && f.stagingSession != nil {
		// Resize the staged bytes: they are what reads, Stat, and the sync
		// worker see. The handle's in-memory buffer is unused on this path.
		if err := f.stagingSession.Truncate(size); err != nil {
			f.logger.Error("Failed to truncate staging file",
				"file_id", f.fileID,
				"path", f.path,
				"size", size,
				"error", err)
			return err
		}
		f.stagingManager.MarkDirty(f.path, size)
		return nil
	}

	return f.truncateObjectLocked(size)
}

// truncateObjectLocked resizes the object directly when staging is disabled.
// Buffered writes are flushed first so the resize applies on top of them.
func (f *File) truncateObjectLocked(size int64) error {
	if size > f.maxBufferedWriteBytes() {
		return fmt.Errorf("truncate of %s to %d bytes exceeds max_buffered_write_mb=%d; enable staging or raise the limit",
			f.path, size, f.maxBufferedWriteBytes()/(1024*1024))
	}

	if f.writeSession != nil {
		f.writeSession.Mu.Lock()
		buffered := f.writeSession.Buffer.Size() > 0
		f.writeSession.Mu.Unlock()
		if err := f.flushSessionBuffer(); err != nil {
			return err
		}
		if buffered {
			// The flush created or rewrote the object.
			f.isNew = false
		}
	}

	ctx := f.requestContext()
	mode := f.perm
	var existing []byte
	switch {
	case f.data != nil:
		existing = f.data
	case !f.isNew:
		info, err := f.ops.Stat(ctx, f.path)
		if err != nil {
			return err
		}
		mode = info.Mode().Perm()
		if size > 0 && info.Size() > 0 {
			data, err := f.ops.ReadFile(ctx, f.path, 0, 0)
			if err != nil {
				return err
			}
			existing = data
		}
	}
	if mode == 0 {
		mode = 0644
	}

	resized := make([]byte, size)
	copy(resized, existing)
	attrs := &types.POSIXAttributes{
		Mode:  mode,
		UID:   1000,
		GID:   1000,
		Mtime: time.Now(),
	}
	if err := f.ops.WriteFile(ctx, f.path, resized, attrs); err != nil {
		return err
	}

	f.data = nil
	f.size = size
	f.loaded = true
	f.isNew = false
	return nil
}

// ensureLoaded loads file data from COS if not already loaded
func (f *File) ensureLoaded() error {
	if f.loaded || f.isNew {
		return nil
	}

	// STAGING PATH: If using staging, skip COS load - data is in staging file
	if f.featureFlags != nil && f.featureFlags.IsStagingEnabled() && f.stagingSession != nil {
		// Get size from staging session
		f.size = f.stagingSession.GetSize()
		f.loaded = true
		f.data = nil
		return nil
	}

	// For files using write buffer (session-based writes), don't load entire file
	// The write buffer handles append-only operations efficiently
	// Only load file size for metadata operations
	if f.writeSession != nil {
		info, err := f.ops.Stat(f.requestContext(), f.path)
		if err != nil {
			return err
		}
		f.size = info.Size()
		f.loaded = true
		f.data = nil
		return nil
	}

	// For files opened for writing (WRONLY or RDWR), only get size
	// The write buffer system will handle data efficiently on first write
	// We don't need to download the file until flush time (read-modify-write)
	if f.flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		// Writable file - get file size but don't load data
		// Write session will be created on first Write() call
		info, err := f.ops.Stat(f.requestContext(), f.path)
		if err != nil {
			return err
		}
		f.size = info.Size()
		f.loaded = true
		f.data = nil
		return nil
	}

	// For read-only files, don't load entire file into memory
	// Instead, use lazy loading and read from COS on demand
	// Read-only mode - get file size but don't load data
	// Data will be fetched on-demand in Read/ReadAt operations
	info, err := f.ops.Stat(f.requestContext(), f.path)
	if err != nil {
		return err
	}
	f.size = info.Size()
	f.loaded = true
	f.data = nil
	return nil
}

// Made with Bob
