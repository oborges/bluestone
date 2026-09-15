package nfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
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
	"github.com/oborges/bluestone/internal/metrics"
	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/internal/staging"
	"github.com/oborges/bluestone/pkg/types"
	nfs "github.com/willscott/go-nfs"
	"go.uber.org/zap"
)

// Logger wraps zap.Logger for NFS operations
type Logger struct {
	zap *zap.Logger
}

// NewLogger creates a new logger wrapper
func NewLogger(zapLogger *zap.Logger) *Logger {
	return &Logger{zap: zapLogger}
}

// Info logs an info message
func (l *Logger) Info(msg string, keysAndValues ...interface{}) {
	fields := make([]zap.Field, 0, len(keysAndValues)/2)
	for i := 0; i < len(keysAndValues); i += 2 {
		if i+1 < len(keysAndValues) {
			key := fmt.Sprint(keysAndValues[i])
			value := keysAndValues[i+1]
			fields = append(fields, zap.Any(key, value))
		}
	}
	l.zap.Info(msg, fields...)
}

// Error logs an error message
func (l *Logger) Error(msg string, keysAndValues ...interface{}) {
	fields := make([]zap.Field, 0, len(keysAndValues)/2)
	for i := 0; i < len(keysAndValues); i += 2 {
		if i+1 < len(keysAndValues) {
			key := fmt.Sprint(keysAndValues[i])
			value := keysAndValues[i+1]
			fields = append(fields, zap.Any(key, value))
		}
	}
	l.zap.Error(msg, fields...)
}

// Debug logs a debug message
func (l *Logger) Debug(msg string, keysAndValues ...interface{}) {
	fields := make([]zap.Field, 0, len(keysAndValues)/2)
	for i := 0; i < len(keysAndValues); i += 2 {
		if i+1 < len(keysAndValues) {
			key := fmt.Sprint(keysAndValues[i])
			value := keysAndValues[i+1]
			fields = append(fields, zap.Any(key, value))
		}
	}
	l.zap.Debug(msg, fields...)
}

// COSHandler implements nfs.Handler interface for IBM Cloud COS
type COSHandler struct {
	ops        *posix.OperationsHandler
	logger     *Logger
	handleMap  map[string]*handleEntry
	handleLock sync.RWMutex
	maxHandles int
}

type handleEntry struct {
	path []string
	hash string
}

// NewCOSHandler creates a new NFS handler for COS
func NewCOSHandler(ops *posix.OperationsHandler, logger *Logger) *COSHandler {
	return &COSHandler{
		ops:        ops,
		logger:     logger,
		handleMap:  make(map[string]*handleEntry),
		maxHandles: 10000,
	}
}

// Mount handles NFS mount requests
func (h *COSHandler) Mount(ctx context.Context, conn net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	h.logger.Info("NFS mount request",
		"path", string(req.Dirpath),
		"remote", conn.RemoteAddr().String())

	// Create a billy filesystem wrapper
	fs := &COSFilesystem{
		ops:    h.ops,
		logger: h.logger,
		root:   string(req.Dirpath),
	}

	// Return success with null auth
	return nfs.MountStatusOk, fs, []nfs.AuthFlavor{nfs.AuthFlavorNull}
}

// Change returns a billy.Change interface for write operations
func (h *COSHandler) Change(fs billy.Filesystem) billy.Change {
	if c, ok := fs.(billy.Change); ok {
		return c
	}
	return nil
}

// FSStat fills in filesystem statistics
func (h *COSHandler) FSStat(ctx context.Context, fs billy.Filesystem, stat *nfs.FSStat) error {
	// Set reasonable defaults for COS
	stat.TotalSize = 1 << 50      // 1 PB
	stat.FreeSize = 1 << 50       // 1 PB
	stat.AvailableSize = 1 << 50  // 1 PB
	stat.TotalFiles = 1 << 32     // 4 billion
	stat.FreeFiles = 1 << 32      // 4 billion
	stat.AvailableFiles = 1 << 32 // 4 billion
	stat.CacheHint = 0

	return nil
}

// ToHandle converts a filesystem path to an opaque file handle
// Uses SHA256 hash for deterministic, fixed-size handles (32 bytes)
func (h *COSHandler) ToHandle(fs billy.Filesystem, path []string) []byte {
	pathStr := strings.Join(path, "/")

	// Create deterministic hash (32 bytes, well within NFS 64-byte limit)
	hash := sha256.Sum256([]byte(pathStr))
	hashStr := hex.EncodeToString(hash[:])

	// Store mapping in memory
	h.handleLock.Lock()
	h.handleMap[hashStr] = &handleEntry{
		path: path,
		hash: hashStr,
	}
	h.handleLock.Unlock()

	// Return raw hash bytes (not hex string)
	return hash[:]
}

// FromHandle converts an opaque file handle back to a filesystem and path
func (h *COSHandler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	if len(fh) == 0 {
		return nil, []string{}, nil // Root directory
	}

	// Convert handle bytes to hex string for lookup
	hashStr := hex.EncodeToString(fh)

	// Look up path in memory
	h.handleLock.RLock()
	entry, ok := h.handleMap[hashStr]
	h.handleLock.RUnlock()

	if !ok {
		return nil, nil, fmt.Errorf("invalid file handle: %s", hashStr[:16])
	}

	// Return filesystem instance with decoded path
	fs := &COSFilesystem{
		ops:    h.ops,
		logger: h.logger,
		root:   "/",
	}

	return fs, entry.path, nil
}

// InvalidateHandle removes a file handle from the cache
func (h *COSHandler) InvalidateHandle(fs billy.Filesystem, fh []byte) error {
	h.handleLock.Lock()
	defer h.handleLock.Unlock()

	hashStr := hex.EncodeToString(fh)
	delete(h.handleMap, hashStr)
	return nil
}

// HandleLimit returns the maximum number of handles that can be cached
func (h *COSHandler) HandleLimit() int {
	return h.maxHandles
}

// EnableTracing enables READDIR tracing
func (h *COSHandler) EnableTracing() {
	EnableTracing()
}

// DisableTracing disables READDIR tracing
func (h *COSHandler) DisableTracing() {
	DisableTracing()
}

// GetTrace returns the trace for a specific path
func (h *COSHandler) GetTrace(path string) *PathTrace {
	return GetTrace(path)
}

// GetAllTraces returns all traces
func (h *COSHandler) GetAllTraces() map[string]*PathTrace {
	return GetAllTraces()
}

// ClearTraces clears all traces
func (h *COSHandler) ClearTraces() {
	ClearTraces()
}

// COSFilesystem implements billy.Filesystem interface for COS
type COSFilesystem struct {
	ops            *posix.OperationsHandler
	logger         *Logger
	root           string
	perfConfig     *config.PerformanceConfig
	sessionManager *buffer.SessionManager
	// Staging architecture components
	stagingManager *staging.StagingManager
	syncWorker     *staging.SyncWorker
	featureFlags   *feature.FeatureFlags
}

// NewCOSFilesystem creates a new COS filesystem (deprecated, use NewCOSFilesystemWithConfig)
func NewCOSFilesystem(ops *posix.OperationsHandler, logger *Logger, root string) *COSFilesystem {
	// Use default config if not provided
	defaultConfig := &config.PerformanceConfig{
		WriteBufferKB:        4096,
		MultipartThresholdMB: 100,
		MultipartChunkMB:     10,
		ReadAheadKB:          config.DefaultReadAheadKB,
		MaxBufferedWriteMB:   config.DefaultMaxBufferedWriteMB,
	}
	return &COSFilesystem{
		ops:        ops,
		logger:     logger,
		root:       root,
		perfConfig: defaultConfig,
	}
}

// NewCOSFilesystemWithConfig creates a new COS filesystem with configuration
func NewCOSFilesystemWithConfig(ops *posix.OperationsHandler, logger *Logger, root string, perfConfig *config.PerformanceConfig, stagingManager *staging.StagingManager, syncWorker *staging.SyncWorker, featureFlags *feature.FeatureFlags) *COSFilesystem {
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

	return &COSFilesystem{
		ops:            ops,
		logger:         logger,
		root:           root,
		perfConfig:     perfConfig,
		sessionManager: sessionManager,
		stagingManager: stagingManager,
		syncWorker:     syncWorker,
		featureFlags:   featureFlags,
	}
}

// Create creates a new file
func (fs *COSFilesystem) Create(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
}

// FSStat reports staging-aware capacity so NFS clients see pressure before writeback fails.
func (fs *COSFilesystem) FSStat(ctx context.Context, stat *nfs.FSStat) error {
	if fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil {
		pressure := fs.stagingManager.CurrentPressure()
		totalBytes := pressure.QuotaBytes
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
		total := uint64(totalBytes)
		available := uint64(0)
		if availableBytes > 0 {
			available = uint64(availableBytes)
		}
		if totalBytes == 0 {
			total = 1 << 50
			available = total
		}

		stat.TotalSize = total
		stat.FreeSize = available
		stat.AvailableSize = available
		stat.TotalFiles = 1 << 32
		stat.FreeFiles = 1 << 32
		stat.AvailableFiles = 1 << 32
		stat.CacheHint = time.Second
		return nil
	}

	stat.TotalSize = 1 << 50
	stat.FreeSize = 1 << 50
	stat.AvailableSize = 1 << 50
	stat.TotalFiles = 1 << 32
	stat.FreeFiles = 1 << 32
	stat.AvailableFiles = 1 << 32
	stat.CacheHint = time.Second
	return nil
}

// Open opens a file for reading
func (fs *COSFilesystem) Open(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDONLY, 0)
}

// OpenFile opens a file with specified flags and permissions
func (fs *COSFilesystem) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	fullPath := fs.Join(fs.root, filename)
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

	file := &COSFile{
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

	if !existenceKnown {
		_, err := fs.ops.Stat(context.Background(), fullPath)
		fileExists = err == nil && !pendingDelete
	}

	if useStagingPath && file.stagingSession != nil && flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE) != 0 {
		// Automatically pre-fetch existing COS objects if modifying without truncating
		if fileExists && !existenceKnown && flag&os.O_TRUNC == 0 {
			err := file.stagingSession.Prefetch(func() error {
				fs.logger.Info("Prefetching existing COS object to local staging cache",
					"file_id", fileID,
					"path", fullPath)
				return fs.ops.DownloadToFile(context.Background(), fullPath, file.stagingSession.StagingPath)
			})
			if err != nil {
				fs.logger.Error("Prefetch error intercepted", zap.Error(err))
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
			err := fs.ops.WriteFile(context.Background(), fullPath, []byte{}, attrs)
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
			data, err := fs.ops.ReadFile(context.Background(), fullPath, 0, 0)
			if err != nil {
				return nil, err
			}
			file.data = data
			file.loaded = true
			file.offset = int64(len(data))
		}
	}

	return file, nil
}

// stagingFileInfo implements os.FileInfo for dirty staging files
type stagingFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	mode    os.FileMode
	uid     uint32
	gid     uint32
}

func (s *stagingFileInfo) Name() string       { return s.name }
func (s *stagingFileInfo) Size() int64        { return s.size }
func (s *stagingFileInfo) Mode() os.FileMode  { return s.mode }
func (s *stagingFileInfo) ModTime() time.Time { return s.modTime }
func (s *stagingFileInfo) IsDir() bool        { return false }
func (s *stagingFileInfo) Sys() interface{} {
	return nil
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

func (fs *COSFilesystem) isStagingDirty(fullPath string) bool {
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
func (fs *COSFilesystem) Stat(filename string) (os.FileInfo, error) {
	fullPath := fs.Join(fs.root, filename)
	if isReservedPath(fullPath) {
		return nil, &os.PathError{Op: "stat", Path: filename, Err: os.ErrNotExist}
	}

	if fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil {
		if fs.stagingManager.HasPendingDelete(fullPath) {
			return nil, &os.PathError{Op: "stat", Path: filename, Err: os.ErrNotExist}
		}
		if session, exists := fs.stagingManager.GetSession(fullPath); exists && (session.Dirty || session.Size == 0 || session.Prefetched) {
			fs.logger.Debug("Stat intercepted by dirty staging session", zap.String("path", fullPath))
			return &stagingFileInfo{
				name:    filepath.Base(fullPath),
				size:    session.Size,
				modTime: session.LastWrite,
				mode:    session.Mode,
				uid:     session.UID,
				gid:     session.GID,
			}, nil
		}
	}

	info, err := fs.ops.Stat(context.Background(), fullPath)
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
func (fs *COSFilesystem) statFromStaging(fullPath string) os.FileInfo {
	if fs.featureFlags == nil || !fs.featureFlags.IsStagingEnabled() || fs.stagingManager == nil {
		return nil
	}

	if session, exists := fs.stagingManager.GetSession(fullPath); exists {
		return &stagingFileInfo{
			name:    filepath.Base(fullPath),
			size:    session.Size,
			modTime: session.LastWrite,
			mode:    session.Mode,
			uid:     session.UID,
			gid:     session.GID,
		}
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

// Rename renames a file
func (fs *COSFilesystem) Rename(oldpath, newpath string) error {
	oldFull := fs.Join(fs.root, oldpath)
	newFull := fs.Join(fs.root, newpath)
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

	return fs.ops.RenameFile(context.Background(), oldFull, newFull)
}

// renameDirtyStagedFile renames a dirty staged source by re-keying the staged
// state and tombstoning the source object. No COS copy is needed: the staged
// bytes sync to the destination key.
func (fs *COSFilesystem) renameDirtyStagedFile(oldFull, newFull string) error {
	if err := fs.stagingManager.RenameStagedPath(oldFull, newFull); err != nil {
		if os.IsNotExist(err) {
			// Staged bytes vanished (stale dirty entry); fall back to the
			// object-store rename.
			return fs.ops.RenameFile(context.Background(), oldFull, newFull)
		}
		return err
	}

	// Finish the source delete inline when no upload is in flight; otherwise
	// the sync worker completes it after the upload lands.
	if fs.stagingManager.TryLockSync(oldFull) {
		if err := fs.ops.DeleteFile(context.Background(), oldFull); err != nil {
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
func (fs *COSFilesystem) discardStagedDestination(path string) error {
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

// Remove removes a file or directory
func (fs *COSFilesystem) Remove(filename string) error {
	fullPath := fs.Join(fs.root, filename)
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
		return fs.ops.DeleteDirectory(context.Background(), fullPath)
	}

	if !stagingEnabled {
		if err := fs.cleanupSessionsBeforeDelete(fullPath); err != nil {
			fs.logger.Error("Failed to cleanup sessions before delete",
				zap.String("path", fullPath),
				zap.Error(err))
			return err
		}
	}

	if err := fs.ops.DeleteFile(context.Background(), fullPath); err != nil {
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
func (fs *COSFilesystem) removeDirtyStagedFile(fullPath string) error {
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

	if err := fs.ops.DeleteFile(context.Background(), fullPath); err != nil {
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
func (fs *COSFilesystem) ensureNoDirtyStagedChildren(op, path string) error {
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

func (fs *COSFilesystem) ensureNoDirtyStagedData(op, path string) error {
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

func (fs *COSFilesystem) cleanupCleanStagingSessionAfterDelete(path string) {
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
func (fs *COSFilesystem) cleanupSessionsBeforeDelete(path string) error {
	ctx := context.Background()

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
func (fs *COSFilesystem) Join(elem ...string) string {
	return filepath.Join(elem...)
}

// TempFile creates a temporary file
func (fs *COSFilesystem) TempFile(dir, prefix string) (billy.File, error) {
	// Generate a unique temporary filename
	tempName := fmt.Sprintf("%s%d", prefix, os.Getpid())
	fullPath := fs.Join(dir, tempName)
	return fs.Create(fullPath)
}

// ReadDir reads directory contents
func (fs *COSFilesystem) ReadDir(path string) ([]os.FileInfo, error) {
	start := time.Now()

	fullPath := fs.Join(fs.root, path)

	// Track per-path calls
	metrics.GetGlobalCounters().RecordPathCall(fullPath)

	listStart := time.Now()
	entries, err := fs.ops.ListDirectory(context.Background(), fullPath)
	listDuration := time.Since(listStart)

	if err != nil {
		// If the object store cannot answer but staged sessions exist under
		// this directory, serve the staged entries: a partial listing keeps
		// applications working during a backend outage.
		if fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil {
			if staged := fs.stagingManager.GetSessionsInDirectory(fullPath); len(staged) > 0 {
				fs.logger.Error("ReadDir serving staged entries only: object store unreachable",
					zap.String("path", fullPath), zap.Error(err))
				result := make([]os.FileInfo, 0, len(staged))
				for _, session := range staged {
					if fs.stagingManager.HasPendingDelete(session.Path) {
						continue
					}
					result = append(result, &stagingFileInfo{
						name:    filepath.Base(session.Path),
						size:    session.Size,
						modTime: session.LastWrite,
						mode:    session.Mode,
						uid:     session.UID,
						gid:     session.GID,
					})
				}
				duration := time.Since(start)
				RecordReaddirCall(fullPath, len(result), duration, nil)
				metrics.RecordReadDir(duration)
				return result, nil
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
	if fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil {
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

		stagingSessions := fs.stagingManager.GetSessionsInDirectory(fullPath)
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
				result = append(result, &stagingFileInfo{
					name:    sessionName,
					size:    session.Size,
					modTime: session.LastWrite,
					mode:    session.Mode,
					uid:     session.UID,
					gid:     session.GID,
				})
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

	return result, nil
}

// MkdirAll creates a directory and all parent directories
func (fs *COSFilesystem) MkdirAll(filename string, perm os.FileMode) error {
	fullPath := fs.Join(fs.root, filename)
	attrs := &types.POSIXAttributes{
		Mode:  perm | os.ModeDir,
		UID:   1000,
		GID:   1000,
		Mtime: time.Now(),
	}
	return fs.ops.CreateDirectory(context.Background(), fullPath, attrs)
}

// Lstat returns file information (same as Stat for COS)
func (fs *COSFilesystem) Lstat(filename string) (os.FileInfo, error) {
	return fs.Stat(filename)
}

// Symlink creates a symbolic link (not supported in COS)
func (fs *COSFilesystem) Symlink(target, link string) error {
	return fmt.Errorf("symlinks not supported")
}

// Readlink reads a symbolic link (not supported in COS)
func (fs *COSFilesystem) Readlink(link string) (string, error) {
	return "", fmt.Errorf("symlinks not supported")
}

// Chroot creates a chrooted filesystem
func (fs *COSFilesystem) Chroot(path string) (billy.Filesystem, error) {
	newRoot := fs.Join(fs.root, path)
	return &COSFilesystem{
		ops:            fs.ops,
		logger:         fs.logger,
		root:           newRoot,
		perfConfig:     fs.perfConfig,
		sessionManager: fs.sessionManager,
		stagingManager: fs.stagingManager,
		syncWorker:     fs.syncWorker,
		featureFlags:   fs.featureFlags,
	}, nil
}

// Root returns the root path
func (fs *COSFilesystem) Root() string {
	return fs.root
}

// Chmod changes the mode of the named file
func (fs *COSFilesystem) Chmod(name string, mode os.FileMode) error {
	// COS doesn't support chmod directly, but we can update metadata
	fullPath := fs.Join(fs.root, name)

	if fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil {
		if session, exists := fs.stagingManager.GetSession(fullPath); exists {
			session.UpdateAttributes(mode, session.UID, session.GID)
			return nil // Staged files bypass COS metadata swaps safely bound natively!
		}
	}

	// Get current file info
	info, err := fs.ops.Stat(context.Background(), fullPath)
	if err != nil {
		return err
	}

	// Update with new mode
	attrs := &types.POSIXAttributes{
		Mode:  mode,
		UID:   1000,
		GID:   1000,
		Mtime: time.Now(),
	}

	// For files, we need to read and rewrite with new attributes
	if !info.IsDir() {
		data, err := fs.ops.ReadFile(context.Background(), fullPath, 0, 0)
		if err != nil {
			return err
		}
		return fs.ops.WriteFile(context.Background(), fullPath, data, attrs)
	}

	// For directories, just update the marker
	return fs.ops.CreateDirectory(context.Background(), fullPath, attrs)
}

// Lchown changes the uid and gid of the named file (link itself)
func (fs *COSFilesystem) Lchown(name string, uid, gid int) error {
	// COS doesn't support symlinks, so this is the same as Chown
	return fs.Chown(name, uid, gid)
}

// Chown changes the uid and gid of the named file
func (fs *COSFilesystem) Chown(name string, uid, gid int) error {
	fullPath := fs.Join(fs.root, name)

	if fs.featureFlags != nil && fs.featureFlags.IsStagingEnabled() && fs.stagingManager != nil {
		if session, exists := fs.stagingManager.GetSession(fullPath); exists {
			session.UpdateAttributes(session.Mode, uint32(uid), uint32(gid))
			return nil // Staged files bypass COS metadata swaps safely bounded
		}
	}

	// Get current file info
	info, err := fs.ops.Stat(context.Background(), fullPath)
	if err != nil {
		return err
	}

	// Update with new ownership
	attrs := &types.POSIXAttributes{
		Mode:  info.Mode(),
		UID:   uid,
		GID:   gid,
		Mtime: time.Now(),
	}

	// For files, read and rewrite with new attributes
	if !info.IsDir() {
		data, err := fs.ops.ReadFile(context.Background(), fullPath, 0, 0)
		if err != nil {
			return err
		}
		return fs.ops.WriteFile(context.Background(), fullPath, data, attrs)
	}

	// For directories, update the marker
	return fs.ops.CreateDirectory(context.Background(), fullPath, attrs)
}

// Chtimes changes the access and modification times
func (fs *COSFilesystem) Chtimes(name string, atime time.Time, mtime time.Time) error {
	fullPath := fs.Join(fs.root, name)

	if fs.isStagingDirty(fullPath) {
		return nil // Staged files bypass COS metadata swaps
	}

	// Get current file info
	info, err := fs.ops.Stat(context.Background(), fullPath)
	if err != nil {
		return err
	}

	// Update with new times
	attrs := &types.POSIXAttributes{
		Mode:  info.Mode(),
		UID:   1000,
		GID:   1000,
		Mtime: mtime,
		Atime: atime,
	}

	// Use efficient metadata update (no need to read/rewrite entire file)
	return fs.ops.UpdateAttributes(context.Background(), fullPath, attrs)
}

// COSFile implements billy.File interface
type COSFile struct {
	ops            *posix.OperationsHandler
	logger         *Logger
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
}

// Name returns the file name
func (f *COSFile) Name() string {
	return filepath.Base(f.path)
}

func (f *COSFile) maxBufferedWriteBytes() int64 {
	limitMB := config.DefaultMaxBufferedWriteMB
	if f.perfConfig != nil && f.perfConfig.MaxBufferedWriteMB > 0 {
		limitMB = f.perfConfig.MaxBufferedWriteMB
	}
	return int64(limitMB) * 1024 * 1024
}

// Read reads data from the file
func (f *COSFile) Read(p []byte) (int, error) {
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
	data, err := f.ops.ReadFile(context.Background(), f.path, f.offset, int64(len(p)))
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

// Write writes data to the file with session-based buffering
func (f *COSFile) Write(p []byte) (int, error) {
	f.totalWrites++

	// STAGING PATH: Write to staging session
	if f.featureFlags != nil && f.featureFlags.IsStagingEnabled() && f.stagingSession != nil {
		n, err := f.stagingSession.Write(p, f.offset)
		if err != nil {
			f.logger.Error("STAGING WRITE ERROR",
				"file_id", f.fileID,
				"path", f.path,
				"offset", f.offset,
				"bytes", len(p),
				"error", err)
			return 0, err
		}

		f.offset += int64(n)

		// Mark file as dirty and update size
		f.stagingManager.MarkDirty(f.path, f.stagingSession.Size)

		f.logger.Info("STAGING WRITE",
			"file_id", f.fileID,
			"path", f.path,
			"offset", f.offset-int64(n),
			"bytes", n,
			"session_size", f.stagingSession.Size,
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
	n, err := f.writeSession.Buffer.Write(f.offset, p)
	shouldFlush := f.writeSession.Buffer.ShouldFlush()
	bufferSize := f.writeSession.Buffer.Size()
	f.writeSession.Mu.Unlock()

	if err != nil {
		f.logger.Error("WRITE ERROR",
			"file_id", f.fileID,
			"session_id", f.writeSession.SessionID,
			"path", f.path,
			"offset", f.offset,
			"bytes", len(p),
			"error", err)
		return 0, err
	}

	f.offset += int64(n)

	f.logger.Info("WRITE",
		"file_id", f.fileID,
		"session_id", f.writeSession.SessionID,
		"path", f.path,
		"offset", f.offset-int64(n),
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
func (f *COSFile) flushSessionBuffer() error {
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
		info, err := f.ops.Stat(context.Background(), f.path)
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

		err := f.ops.WriteFile(context.Background(), f.path, data, attrs)
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
		existingData, err := f.ops.ReadFile(context.Background(), f.path, 0, 0)
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

		err = f.ops.WriteFile(context.Background(), f.path, finalData, attrs)
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

		existingData, err := f.ops.ReadFile(context.Background(), f.path, 0, 0)
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

		err = f.ops.WriteFile(context.Background(), f.path, existingData, attrs)
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
func (f *COSFile) Close() error {
	// STAGING PATH: Release staging session
	if f.featureFlags != nil && f.featureFlags.IsStagingEnabled() && f.stagingSession != nil {
		sessionSize := f.stagingSession.Size
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
				"session_size", f.stagingSession.Size)
		}

		return nil
	}

	// LEGACY PATH: Release write session (flush now to prevent small data loss)
	if f.writeSession != nil {
		if err := f.flushSessionBuffer(); err != nil {
			f.logger.Error("Failed to flush session buffer on close", "error", err)
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
		err := f.ops.WriteFile(context.Background(), f.path, f.data, attrs)
		if err != nil {
			return err
		}
	}

	return nil
}

// Seek sets the file offset
func (f *COSFile) Seek(offset int64, whence int) (int64, error) {
	if err := f.ensureLoaded(); err != nil && !f.isNew {
		return 0, err
	}

	var fileSize int64
	if f.data != nil {
		fileSize = int64(len(f.data))
	} else {
		fileSize = f.size
	}

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

// Lock locks the file (no-op for COS)
func (f *COSFile) Lock() error {
	return nil
}

// Unlock unlocks the file (no-op for COS)
func (f *COSFile) Unlock() error {
	return nil
}

// ReadAt reads data from the file at a specific offset
func (f *COSFile) ReadAt(p []byte, off int64) (int, error) {
	if err := f.ensureLoaded(); err != nil {
		return 0, err
	}

	// If data is loaded in memory (writable file), use it
	if f.data != nil {
		if off >= int64(len(f.data)) {
			return 0, io.EOF
		}
		n := copy(p, f.data[off:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}

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
	if off >= f.size {
		return 0, io.EOF
	}

	// Read-only file: fetch data on-demand using range read
	data, err := f.ops.ReadFile(context.Background(), f.path, off, int64(len(p)))
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
	if n < len(p) && off+int64(n) >= f.size {
		return n, io.EOF
	}

	return n, nil
}

// Truncate truncates the file to a specified size
func (f *COSFile) Truncate(size int64) error {
	if err := f.ensureLoaded(); err != nil && !f.isNew {
		return err
	}

	if size < int64(len(f.data)) {
		f.data = f.data[:size]
	} else if size > int64(len(f.data)) {
		newData := make([]byte, size)
		copy(newData, f.data)
		f.data = newData
	}

	return nil
}

// ensureLoaded loads file data from COS if not already loaded
func (f *COSFile) ensureLoaded() error {
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
		info, err := f.ops.Stat(context.Background(), f.path)
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
		info, err := f.ops.Stat(context.Background(), f.path)
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
	info, err := f.ops.Stat(context.Background(), f.path)
	if err != nil {
		return err
	}
	f.size = info.Size()
	f.loaded = true
	f.data = nil
	return nil
}

// Made with Bob
