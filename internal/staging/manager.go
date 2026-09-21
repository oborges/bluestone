package staging

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/metrics"
	"go.uber.org/zap"
)

// StagingManager manages staging files and write sessions
type StagingManager struct {
	config        *config.StagingConfig
	stagingRoot   string
	sessions      map[string]*WriteSession
	dirtyIndex    *DirtyFileIndex
	tombstones    map[string]*TombstoneState
	mu            sync.RWMutex
	tombstoneMu   sync.RWMutex
	pressureMu    sync.Mutex
	reservedBytes int64
	// bucketFull reports whether the bucket is refusing writes for its
	// quota (see SetBucketFullCheck).
	bucketFull func() bool
	// sidecarMu serializes read-modify-write updates of path metadata
	// sidecars. Lock order: mu before sidecarMu.
	sidecarMu sync.Mutex
}

var ErrPathConflicted = errors.New("staging path has unresolved conflict")

const (
	PressureLevelNormal   = "normal"
	PressureLevelHigh     = "high"
	PressureLevelCritical = "critical"

	BackpressureModeBlock    = "block"
	BackpressureModeFailFast = "fail_fast"
)

// PressureState captures the current staging pressure calculation.
type PressureState struct {
	UsedBytes              int64
	AvailableBytes         int64
	QuotaBytes             int64
	HighWatermarkBytes     int64
	CriticalWatermarkBytes int64
	ProjectedBytes         int64
	Level                  string
}

// ExternalChangeSnapshot describes the object-store change that caused a local
// dirty staged path to become conflicted.
type ExternalChangeSnapshot struct {
	ObjectKey    string
	Size         int64
	ETag         string
	LastModified time.Time
	Deleted      bool
	Reason       string
}

// EnsurePathMetadata creates or refreshes the durable sidecar for a staged path
// without marking it dirty. Existing observed COS state and generation are
// preserved.
func (sm *StagingManager) EnsurePathMetadata(path, stagingPath string, size int64) error {
	sm.sidecarMu.Lock()
	defer sm.sidecarMu.Unlock()

	metadataPath := sm.pathMetadataPath(stagingPath)
	state, err := readPathMetadataState(metadataPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if state == nil {
		state = &PathMetadataState{
			Version:        pathMetadataVersion,
			OriginalPath:   path,
			ObjectKey:      objectKeyFromPath(path),
			StagedFilePath: stagingPath,
			ConflictStatus: ConflictStatusNone,
			Size:           size,
		}
	}
	state.Version = pathMetadataVersion
	state.OriginalPath = path
	if state.ObjectKey == "" {
		state.ObjectKey = objectKeyFromPath(path)
	}
	state.StagedFilePath = stagingPath
	state.Size = size
	if state.ConflictStatus == "" {
		state.ConflictStatus = ConflictStatusNone
	}
	return writePathMetadataState(metadataPath, state)
}

// MarkPathDirtyMetadata persists the local dirty generation and staged file
// details used to compare a staged write with the object state observed before
// or during the write.
func (sm *StagingManager) MarkPathDirtyMetadata(path string, size int64) (*PathMetadataState, error) {
	// Read the session's attributes before taking the sidecar lock: the
	// lookup takes mu, which must be acquired first.
	attrs := sm.sessionAttributes(path)

	sm.sidecarMu.Lock()
	defer sm.sidecarMu.Unlock()

	stagingPath := sm.stagingFilePath(path)
	metadataPath := sm.pathMetadataPath(stagingPath)
	state, err := readPathMetadataState(metadataPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		state = &PathMetadataState{
			Version:      pathMetadataVersion,
			OriginalPath: path,
			ObjectKey:    objectKeyFromPath(path),
		}
	}

	now := time.Now()
	state.Version = pathMetadataVersion
	state.OriginalPath = path
	if state.ObjectKey == "" {
		state.ObjectKey = objectKeyFromPath(path)
	}
	state.LocalDirtyGeneration++
	if state.LocalDirtyGeneration <= 0 {
		state.LocalDirtyGeneration = 1
	}
	state.StagedFilePath = stagingPath
	state.ConflictStatus = ConflictStatusNone
	state.Size = size
	if state.DirtySince.IsZero() {
		state.DirtySince = now
	}
	state.LastModified = now
	if attrs != nil {
		state.Attributes = attrs
	}

	if err := writePathMetadataState(metadataPath, state); err != nil {
		return nil, err
	}
	return state, nil
}

func (sm *StagingManager) pathMetadataPath(stagingPath string) string {
	return stagingPath + ".metadata"
}

func (sm *StagingManager) removePathMetadata(path string) error {
	metadataPath := sm.pathMetadataPath(sm.stagingFilePath(path))
	if err := os.Remove(metadataPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// NewStagingManager creates a new staging manager
func NewStagingManager(cfg *config.StagingConfig) (*StagingManager, error) {
	normalizeBackpressureConfig(cfg)

	// Create staging directory structure
	activeDir := filepath.Join(cfg.RootDir, "active")
	if err := os.MkdirAll(activeDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create staging directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.RootDir, "lost+found"), 0700); err != nil {
		return nil, fmt.Errorf("failed to create staging lost+found directory: %w", err)
	}

	sm := &StagingManager{
		config:      cfg,
		stagingRoot: cfg.RootDir,
		sessions:    make(map[string]*WriteSession),
		dirtyIndex:  NewDirtyFileIndex(),
		tombstones:  make(map[string]*TombstoneState),
	}

	logging.Info("Staging manager initialized",
		zap.String("root_dir", cfg.RootDir),
		zap.Bool("enabled", cfg.Enabled))

	// Recover from disk if needed
	if err := sm.RecoverFromDisk(); err != nil {
		logging.Warn("Failed to recover from disk, continuing anyway",
			zap.Error(err))
	}
	sm.updatePressureMetrics()
	sm.updateConflictMetrics()

	return sm, nil
}

func normalizeBackpressureConfig(cfg *config.StagingConfig) {
	if cfg.BackpressureMode == "" {
		cfg.BackpressureMode = BackpressureModeBlock
	}
	if cfg.BackpressureHighWatermarkPct == 0 {
		cfg.BackpressureHighWatermarkPct = 80
	}
	if cfg.BackpressureCritWatermarkPct == 0 {
		cfg.BackpressureCritWatermarkPct = 95
	}
	if cfg.BackpressureWaitTimeout == "" {
		cfg.BackpressureWaitTimeout = "30s"
	}
	if cfg.BackpressureCheckInterval == "" {
		cfg.BackpressureCheckInterval = "250ms"
	}
}

// GetOrCreateSession gets an existing session or creates a new one
func (sm *StagingManager) GetOrCreateSession(path string) (*WriteSession, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.dirtyIndex.IsConflicted(path) {
		return nil, fmt.Errorf("%w: %s", ErrPathConflicted, path)
	}

	// A new write session recreates the path, superseding any accepted delete.
	sm.CancelPendingDelete(path)

	// Check if session exists
	if session, exists := sm.sessions[path]; exists {
		session.IncrementRefCount()
		logging.Debug("Reusing existing write session",
			zap.String("path", path),
			zap.Int32("ref_count", session.GetRefCount()))
		return session, nil
	}

	// Create new session
	stagingPath := sm.stagingFilePath(path)
	session, err := NewWriteSession(sm, path, stagingPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	sm.sessions[path] = session

	logging.Info("Created new write session",
		zap.String("path", path),
		zap.String("staging_path", stagingPath))

	return session, nil
}

// ReleaseSession decrements the reference count for a session
func (sm *StagingManager) ReleaseSession(path string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.sessions[path]
	if !exists {
		return
	}

	session.DecrementRefCount()

	logging.Debug("Released write session",
		zap.String("path", path),
		zap.Int32("ref_count", session.GetRefCount()))

	// Keep session alive even if RefCount == 0 for potential reopen
	// Cleanup happens after sync + idle timeout
}

// GetSessionsInDirectory organically returns all actively buffering files existing precisely inside specific logical directories!
func (sm *StagingManager) GetSessionsInDirectory(dirPath string) []*WriteSession {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var sessions []*WriteSession
	prefix := dirPath
	if prefix != "/" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	for path, session := range sm.sessions {
		if sm.dirtyIndex.IsConflicted(path) {
			continue
		}
		if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
			// Extracting only precisely adjacent file instances, ignoring deep nested arrays seamlessly!
			remainder := strings.TrimPrefix(path, prefix)
			if !strings.Contains(remainder, "/") {
				sessions = append(sessions, session)
			}
		}
	}
	return sessions
}

// MarkDirty marks a file as dirty (needs sync)
func (sm *StagingManager) MarkDirty(path string, size int64) {
	if sm.dirtyIndex.IsConflicted(path) {
		logging.Warn("Refusing to queue conflicted staged path for sync",
			zap.String("path", path),
			zap.Int64("size", size))
		return
	}

	state, err := sm.MarkPathDirtyMetadata(path, size)
	if err != nil {
		logging.Warn("Failed to persist dirty path metadata",
			zap.String("path", path),
			zap.Error(err))
	}

	sm.dirtyIndex.MarkDirtyWithState(path, size, state)
	sm.updateSyncQueueMetrics()
	sm.updatePressureMetrics()

	logging.Debug("Marked file as dirty",
		zap.String("path", path),
		zap.Int64("size", size),
		zap.Int("total_dirty", sm.dirtyIndex.Count()))
}

// MarkClean marks a file as clean (synced)
func (sm *StagingManager) MarkClean(path string) {
	sm.dirtyIndex.MarkClean(path)
	if err := sm.removePathMetadata(path); err != nil {
		logging.Warn("Failed to remove clean path metadata",
			zap.String("path", path),
			zap.Error(err))
	}
	sm.updateSyncQueueMetrics()
	sm.updatePressureMetrics()

	logging.Debug("Marked file as clean",
		zap.String("path", path),
		zap.Int("total_dirty", sm.dirtyIndex.Count()))
}

// ForgetDirty removes stale dirty bookkeeping when staged data is intentionally gone.
func (sm *StagingManager) ForgetDirty(path, reason string) {
	sm.dirtyIndex.MarkClean(path)
	if err := sm.removePathMetadata(path); err != nil {
		logging.Warn("Failed to remove forgotten path metadata",
			zap.String("path", path),
			zap.Error(err))
	}
	sm.updateSyncQueueMetrics()
	sm.updatePressureMetrics()

	logging.Info("Forgot dirty staging entry",
		zap.String("path", path),
		zap.String("reason", reason),
		zap.Int("total_dirty", sm.dirtyIndex.Count()))
}

// RecordConflict preserves dirty local staged data and makes the COS object the
// default winner for subsequent operations on the original path.
func (sm *StagingManager) RecordConflict(path string, change ExternalChangeSnapshot) (*ConflictMetadata, error) {
	if sm == nil {
		return nil, fmt.Errorf("staging manager is nil")
	}
	if !sm.dirtyIndex.IsDirty(path) {
		return nil, nil
	}

	sm.mu.RLock()
	session, sessionExists := sm.sessions[path]
	sm.mu.RUnlock()

	stagingPath := sm.stagingFilePath(path)
	if sessionExists {
		if err := session.Sync(); err != nil {
			return nil, fmt.Errorf("failed to sync conflicted staging file: %w", err)
		}
		stagingPath, _, _, _, _, _, _, _ = session.Snapshot()
	}

	info, err := os.Stat(stagingPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat conflicted staging file: %w", err)
	}

	detectedAt := time.Now()
	preservedPath := sm.conflictFilePath(path, detectedAt)
	if err := os.MkdirAll(filepath.Dir(preservedPath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create conflict directory: %w", err)
	}
	if err := copyFile(preservedPath, stagingPath); err != nil {
		return nil, fmt.Errorf("failed to preserve conflicted staging file: %w", err)
	}

	reason := change.Reason
	if reason == "" {
		reason = "external_object_change"
	}
	metadataPath := preservedPath + ".metadata.json"
	conflict := &ConflictMetadata{
		Path:                  path,
		ObjectKey:             change.ObjectKey,
		Size:                  info.Size(),
		PreservedPath:         preservedPath,
		PreservedMetadataPath: metadataPath,
		DetectedAt:            detectedAt,
		Reason:                reason,
		StagedPath:            stagingPath,
		ConflictStatus:        ConflictStatusConflicted,
		ObservedSize:          change.Size,
		ObservedETag:          change.ETag,
		ObservedLastModified:  change.LastModified,
		ExternalSize:          change.Size,
		ExternalETag:          change.ETag,
		ExternalLastModified:  change.LastModified,
		ExternalDeleted:       change.Deleted,
	}
	if dirty := sm.dirtyIndex.GetMetadata(path); dirty != nil {
		conflict.LocalDirtyGeneration = dirty.LocalDirtyGeneration
		if conflict.ObjectKey == "" {
			conflict.ObjectKey = dirty.ObjectKey
		}
		if conflict.ObservedETag == "" {
			conflict.ObservedETag = dirty.ObservedETag
			conflict.ObservedSize = dirty.ObservedSize
			conflict.ObservedLastModified = dirty.ObservedLastModified
		}
	}

	metadataBytes, err := json.MarshalIndent(conflict, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal conflict metadata: %w", err)
	}
	if err := os.WriteFile(metadataPath, metadataBytes, 0600); err != nil {
		return nil, fmt.Errorf("failed to write conflict metadata: %w", err)
	}

	sm.mu.Lock()
	if current, exists := sm.sessions[path]; exists {
		session = current
		delete(sm.sessions, path)
	}
	sm.mu.Unlock()

	if session != nil {
		session.Dirty = false
		if err := session.Close(); err != nil {
			logging.Warn("Failed to close conflicted staging session",
				zap.String("path", path),
				zap.Error(err))
		}
	}

	if err := os.Remove(stagingPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove conflicted active staging file: %w", err)
	}
	if err := os.Remove(stagingPath + ".metadata"); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove conflicted active staging metadata: %w", err)
	}

	sm.dirtyIndex.MarkConflicted(conflict)
	sm.updateSyncQueueMetrics()
	sm.updatePressureMetrics()
	sm.updateConflictMetrics()
	metrics.RecordStagingConflict()

	logging.Warn("Recorded staging conflict after external COS change",
		zap.String("path", path),
		zap.String("object_key", change.ObjectKey),
		zap.String("preserved_path", preservedPath),
		zap.Int64("local_size", info.Size()),
		zap.Int64("external_size", change.Size),
		zap.Bool("external_deleted", change.Deleted),
		zap.Time("external_last_modified", change.LastModified))

	return conflict, nil
}

// IsDirty returns true if the file is dirty
func (sm *StagingManager) IsDirty(path string) bool {
	return sm.dirtyIndex.IsDirty(path)
}

// IsConflicted returns true when a path has an unresolved staging conflict.
func (sm *StagingManager) IsConflicted(path string) bool {
	return sm.dirtyIndex.IsConflicted(path)
}

// DirtyPathsUnder returns dirty staged paths at path or below it.
func (sm *StagingManager) DirtyPathsUnder(path string) []string {
	if sm == nil {
		return nil
	}

	path = filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	prefix := path
	if prefix != "/" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	dirtyFiles := sm.GetDirtyFiles()
	paths := make([]string, 0, len(dirtyFiles))
	for _, metadata := range dirtyFiles {
		if metadata == nil {
			continue
		}
		dirtyPath := filepath.ToSlash(filepath.Clean(metadata.Path))
		if !strings.HasPrefix(dirtyPath, "/") {
			dirtyPath = "/" + dirtyPath
		}
		if dirtyPath == path || path == "/" || strings.HasPrefix(dirtyPath, prefix) {
			paths = append(paths, dirtyPath)
		}
	}
	sort.Strings(paths)
	return paths
}

// GetSession returns an existing session (without creating)
func (sm *StagingManager) GetSession(path string) (*WriteSession, bool) {
	if sm.dirtyIndex.IsConflicted(path) {
		return nil, false
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, exists := sm.sessions[path]
	return session, exists
}

// RecoverSessionFromStaging rebuilds an idle write session around an existing staging file.
func (sm *StagingManager) RecoverSessionFromStaging(path string) (*WriteSession, error) {
	stagingPath := sm.stagingFilePath(path)
	info, err := os.Stat(stagingPath)
	if err != nil {
		return nil, err
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	if session, exists := sm.sessions[path]; exists {
		return session, nil
	}

	session, err := NewWriteSession(sm, path, stagingPath)
	if err != nil {
		return nil, err
	}
	// Upload the recovered bytes with the attributes they were staged with.
	if state, err := readPathMetadataState(sm.pathMetadataPath(stagingPath)); err == nil && state.Attributes != nil {
		session.restoreAttributes(*state.Attributes)
	}

	session.Size = info.Size()
	session.Dirty = true
	session.RefCount = 0
	session.LastWrite = info.ModTime()
	session.LastAccess = info.ModTime()
	sm.sessions[path] = session

	logging.Info("Recovered missing staging session",
		zap.String("path", path),
		zap.String("staging_path", stagingPath),
		zap.Int64("size", info.Size()))

	return session, nil
}

// GetTotalStagingSize calculates the total byte quota utilized by active tracing sessions
func (sm *StagingManager) GetTotalStagingSize() int64 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var total int64
	for _, session := range sm.sessions {
		total += session.GetSize()
	}
	return total
}

// GetDirtyFiles returns a list of all dirty files
func (sm *StagingManager) GetDirtyFiles() []*DirtyFileMetadata {
	return sm.dirtyIndex.GetDirtyFiles()
}

// GetConflicts returns unresolved staging conflict records.
func (sm *StagingManager) GetConflicts() []*ConflictMetadata {
	return sm.dirtyIndex.GetConflicts()
}

// SyncQueueStats returns the current staging sync backlog.
func (sm *StagingManager) SyncQueueStats() (depth int, totalBytes int64, oldestAge time.Duration) {
	dirtyFiles := sm.GetDirtyFiles()
	now := time.Now()

	for _, metadata := range dirtyFiles {
		depth++
		totalBytes += metadata.Size
		if metadata.DirtySince.IsZero() {
			continue
		}
		age := now.Sub(metadata.DirtySince)
		if age > oldestAge {
			oldestAge = age
		}
	}

	return depth, totalBytes, oldestAge
}

// ConflictStats returns unresolved conflict count, paths, and newest conflict time.
func (sm *StagingManager) ConflictStats() (int, []string, time.Time) {
	conflicts := sm.GetConflicts()
	paths := make([]string, 0, len(conflicts))
	var last time.Time
	for _, conflict := range conflicts {
		paths = append(paths, conflict.Path)
		if conflict.DetectedAt.After(last) {
			last = conflict.DetectedAt
		}
	}
	sort.Strings(paths)
	return len(conflicts), paths, last
}

func (sm *StagingManager) updateSyncQueueMetrics() {
	depth, totalBytes, oldestAge := sm.SyncQueueStats()
	metrics.SetStagingSyncQueue(depth, totalBytes, oldestAge)
}

func (sm *StagingManager) updateConflictMetrics() {
	count, _, last := sm.ConflictStats()
	metrics.SetStagingConflicts(count, last)
}

// SetBucketFullCheck tells staging how to learn that the bucket is refusing
// writes for its hard quota. While it is, writes are refused with ENOSPC
// rather than accepted into staging: staged data could not be uploaded, and
// the client would only find out when staging filled.
func (sm *StagingManager) SetBucketFullCheck(full func() bool) {
	sm.pressureMu.Lock()
	defer sm.pressureMu.Unlock()
	sm.bucketFull = full
}

// ReserveWrite applies staging backpressure and reserves new bytes before a write.
func (sm *StagingManager) ReserveWrite(path string, requestedBytes, growthBytes int64) (func(), error) {
	if requestedBytes < 0 {
		requestedBytes = 0
	}
	if growthBytes < 0 {
		growthBytes = 0
	}

	releaseNoop := func() {}
	if sm == nil || sm.config == nil || sm.config.MaxStagingSizeGB <= 0 {
		return releaseNoop, nil
	}
	sm.pressureMu.Lock()
	bucketFull := sm.bucketFull
	sm.pressureMu.Unlock()
	if requestedBytes > 0 && bucketFull != nil && bucketFull() {
		logging.Info("Refusing write: bucket is over its hard quota",
			zap.String("path", path), zap.Int64("requested_bytes", requestedBytes))
		metrics.RecordBackpressureRejected()
		return releaseNoop, syscall.ENOSPC
	}

	timeout, err := sm.config.GetBackpressureWaitTimeout()
	if err != nil {
		timeout = 0
	}
	checkInterval, err := sm.config.GetBackpressureCheckInterval()
	if err != nil || checkInterval <= 0 {
		checkInterval = 250 * time.Millisecond
	}

	start := time.Now()
	blocked := false
	deadline := start.Add(timeout)

	for {
		sm.pressureMu.Lock()
		state := sm.pressureStateLocked(growthBytes)
		mode := strings.ToLower(sm.config.BackpressureMode)
		if mode == "" {
			mode = BackpressureModeBlock
		}

		switch {
		case !sm.config.BackpressureEnabled:
			if state.ProjectedBytes > state.QuotaBytes {
				sm.logBackpressureDecision(path, requestedBytes, state, "reject")
				metrics.RecordBackpressureRejected()
				sm.pressureMu.Unlock()
				return releaseNoop, syscall.ENOSPC
			}
			sm.reservedBytes += growthBytes
			sm.logBackpressureDecision(path, requestedBytes, state, "allow")
			sm.updatePressureMetricsLocked()
			sm.pressureMu.Unlock()
			return sm.releaseReservation(growthBytes), nil

		case state.Level == PressureLevelCritical || state.ProjectedBytes > state.QuotaBytes:
			sm.logBackpressureDecision(path, requestedBytes, state, "reject")
			metrics.RecordBackpressureRejected()
			sm.pressureMu.Unlock()
			return releaseNoop, syscall.ENOSPC

		case state.Level == PressureLevelHigh && mode == BackpressureModeBlock:
			if timeout <= 0 || time.Now().After(deadline) {
				if blocked {
					metrics.RecordBackpressureWait(time.Since(start))
				}
				sm.logBackpressureDecision(path, requestedBytes, state, "reject")
				metrics.RecordBackpressureRejected()
				sm.pressureMu.Unlock()
				return releaseNoop, syscall.ENOSPC
			}
			sm.logBackpressureDecision(path, requestedBytes, state, "block")
			if !blocked {
				metrics.RecordBackpressureBlocked()
				blocked = true
			}
			wait := checkInterval
			if remaining := time.Until(deadline); remaining < wait {
				wait = remaining
			}
			sm.pressureMu.Unlock()
			time.Sleep(wait)

		default:
			if blocked {
				metrics.RecordBackpressureWait(time.Since(start))
			}
			sm.reservedBytes += growthBytes
			sm.logBackpressureDecision(path, requestedBytes, state, "allow")
			sm.updatePressureMetricsLocked()
			sm.pressureMu.Unlock()
			return sm.releaseReservation(growthBytes), nil
		}
	}
}

func (sm *StagingManager) releaseReservation(bytes int64) func() {
	return func() {
		if bytes <= 0 {
			return
		}
		sm.pressureMu.Lock()
		if bytes > sm.reservedBytes {
			sm.reservedBytes = 0
		} else {
			sm.reservedBytes -= bytes
		}
		sm.updatePressureMetricsLocked()
		sm.pressureMu.Unlock()
	}
}

// CurrentPressure returns the current staging pressure without adding a new write.
func (sm *StagingManager) CurrentPressure() PressureState {
	sm.pressureMu.Lock()
	defer sm.pressureMu.Unlock()
	return sm.pressureStateLocked(0)
}

func (sm *StagingManager) pressureStateLocked(growthBytes int64) PressureState {
	quotaBytes := sm.config.MaxStagingSizeGB * 1024 * 1024 * 1024
	usedBytes := sm.GetTotalStagingSize() + sm.reservedBytes
	projectedBytes := usedBytes + growthBytes
	quotaAvailable := quotaBytes - usedBytes
	if quotaAvailable < 0 {
		quotaAvailable = 0
	}
	availableBytes := quotaAvailable
	if diskAvailable, ok := sm.stagingDiskAvailableBytes(); ok && diskAvailable < availableBytes {
		availableBytes = diskAvailable
	}

	level := PressureLevelNormal
	highBytes := quotaBytes
	criticalBytes := quotaBytes
	if quotaBytes > 0 {
		highBytes = quotaBytes * int64(sm.config.BackpressureHighWatermarkPct) / 100
		criticalBytes = quotaBytes * int64(sm.config.BackpressureCritWatermarkPct) / 100
		if projectedBytes >= criticalBytes || growthBytes > availableBytes {
			level = PressureLevelCritical
		} else if projectedBytes >= highBytes {
			level = PressureLevelHigh
		}
	}

	return PressureState{
		UsedBytes:              usedBytes,
		AvailableBytes:         availableBytes,
		QuotaBytes:             quotaBytes,
		HighWatermarkBytes:     highBytes,
		CriticalWatermarkBytes: criticalBytes,
		ProjectedBytes:         projectedBytes,
		Level:                  level,
	}
}

func (sm *StagingManager) stagingDiskAvailableBytes() (int64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(sm.stagingRoot, &stat); err != nil {
		return 0, false
	}
	return int64(stat.Bavail) * int64(stat.Bsize), true
}

func (sm *StagingManager) updatePressureMetrics() {
	sm.pressureMu.Lock()
	defer sm.pressureMu.Unlock()
	sm.updatePressureMetricsLocked()
}

func (sm *StagingManager) updatePressureMetricsLocked() {
	state := sm.pressureStateLocked(0)
	metrics.SetStagingPressure(state.UsedBytes, state.AvailableBytes, state.Level)
}

func (sm *StagingManager) logBackpressureDecision(path string, requestedBytes int64, state PressureState, decision string) {
	logging.Info("Staging backpressure decision",
		zap.String("path", path),
		zap.Int64("requested_bytes", requestedBytes),
		zap.Int64("available_bytes", state.AvailableBytes),
		zap.Int64("used_bytes", state.UsedBytes),
		zap.Int64("projected_bytes", state.ProjectedBytes),
		zap.String("pressure_level", state.Level),
		zap.String("decision", decision))
}

func (sm *StagingManager) conflictFilePath(path string, detectedAt time.Time) string {
	hash := sha256.Sum256([]byte(path))
	filename := fmt.Sprintf("%s_%s.data",
		detectedAt.UTC().Format("20060102T150405.000000000Z"),
		hex.EncodeToString(hash[:8]))
	return filepath.Join(sm.stagingRoot, "lost+found", filename)
}

func copyFile(dstPath, srcPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			_ = os.Remove(dstPath)
		}
	}()
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	removeOnError = false
	return nil
}

// stagingFilePath generates the staging file path for a logical path
func (sm *StagingManager) stagingFilePath(path string) string {
	// Use SHA256 hash of path as filename
	hash := sha256.Sum256([]byte(path))
	filename := hex.EncodeToString(hash[:16]) + ".data"
	return filepath.Join(sm.stagingRoot, "active", filename)
}

// RecoverFromDisk scans the staging directory and rebuilds state
func (sm *StagingManager) RecoverFromDisk() error {
	// Tombstones load first: an accepted delete supersedes any staged bytes
	// left on disk for the same path.
	sm.recoverTombstonesFromDisk()

	activeDir := filepath.Join(sm.stagingRoot, "active")

	// Check if directory exists
	if _, err := os.Stat(activeDir); os.IsNotExist(err) {
		return nil // Nothing to recover
	}

	// Scan directory
	entries, err := os.ReadDir(activeDir)
	if err != nil {
		return fmt.Errorf("failed to read staging directory: %w", err)
	}

	recovered := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".data") {
			continue
		}

		// Get file info
		filePath := filepath.Join(activeDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			logging.Warn("Failed to stat staging file",
				zap.String("file", filePath),
				zap.Error(err))
			continue
		}

		metadataPath := sm.pathMetadataPath(filePath)
		state, err := readPathMetadataState(metadataPath)
		if err != nil {
			logging.Warn("Failed to read valid staging metadata. Leaving data file stranded.",
				zap.String("file", entry.Name()),
				zap.String("metadata_path", metadataPath),
				zap.Error(err))
			continue
		}
		if state.ConflictStatus == ConflictStatusConflicted {
			logging.Warn("Skipping conflicted active staging metadata during recovery",
				zap.String("file", entry.Name()),
				zap.String("path", state.OriginalPath))
			continue
		}
		if sm.HasPendingDelete(state.OriginalPath) {
			if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
				logging.Warn("Failed to remove staged data superseded by pending delete",
					zap.String("file", filePath),
					zap.Error(err))
			}
			if err := os.Remove(metadataPath); err != nil && !os.IsNotExist(err) {
				logging.Warn("Failed to remove staged metadata superseded by pending delete",
					zap.String("metadata_path", metadataPath),
					zap.Error(err))
			}
			logging.Info("Dropped staged data superseded by pending delete during recovery",
				zap.String("path", state.OriginalPath))
			continue
		}
		state.StagedFilePath = filePath
		state.Size = info.Size()
		if state.LastModified.IsZero() {
			state.LastModified = info.ModTime()
		}
		if state.DirtySince.IsZero() {
			state.DirtySince = info.ModTime()
		}
		if err := writePathMetadataState(metadataPath, state); err != nil {
			logging.Warn("Failed to refresh recovered staging metadata",
				zap.String("file", entry.Name()),
				zap.String("metadata_path", metadataPath),
				zap.Error(err))
		}

		// Mark as dirty for re-sync safely preserving original maps!
		session, err := sm.RecoverSessionFromStaging(state.OriginalPath)
		if err != nil {
			logging.Warn("Failed to reconstruct Write Session for staged file",
				zap.String("path", state.OriginalPath),
				zap.Error(err))
			continue
		}

		// Force size resolution based on orphaned hash stats
		session.Size = info.Size()
		session.Dirty = true
		session.LastWrite = info.ModTime()
		session.LastAccess = info.ModTime()

		sm.dirtyIndex.RestoreDirty(dirtyMetadataFromPathState(state, info.Size(), info.ModTime()))

		logging.Debug("Orphaned staging file recovered natively",
			zap.String("file", entry.Name()),
			zap.String("original_path", state.OriginalPath),
			zap.Int64("size", info.Size()))

		recovered++
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".metadata") {
			continue
		}
		dataPath := filepath.Join(activeDir, strings.TrimSuffix(entry.Name(), ".metadata"))
		if _, err := os.Stat(dataPath); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			logging.Warn("Failed to stat staging file for metadata stale check",
				zap.String("metadata", entry.Name()),
				zap.Error(err))
			continue
		}
		metadataPath := filepath.Join(activeDir, entry.Name())
		if err := os.Remove(metadataPath); err != nil && !os.IsNotExist(err) {
			logging.Warn("Failed to remove stale staging metadata",
				zap.String("metadata_path", metadataPath),
				zap.Error(err))
		} else {
			logging.Info("Removed stale staging metadata without data file",
				zap.String("metadata_path", metadataPath))
		}
	}

	recoveredConflicts := sm.recoverConflictsFromDisk()

	if recovered > 0 {
		logging.Info("Recovered staging files automatically after daemon crash",
			zap.Int("count", recovered))
	}
	if recoveredConflicts > 0 {
		logging.Info("Recovered staging conflicts automatically after daemon crash",
			zap.Int("count", recoveredConflicts))
	}
	sm.updateSyncQueueMetrics()
	sm.updatePressureMetrics()
	sm.updateConflictMetrics()

	return nil
}

func (sm *StagingManager) recoverConflictsFromDisk() int {
	lostFoundDir := filepath.Join(sm.stagingRoot, "lost+found")
	entries, err := os.ReadDir(lostFoundDir)
	if err != nil {
		if !os.IsNotExist(err) {
			logging.Warn("Failed to read staging conflict directory",
				zap.String("dir", lostFoundDir),
				zap.Error(err))
		}
		return 0
	}

	recovered := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".metadata.json") {
			continue
		}
		metadataPath := filepath.Join(lostFoundDir, entry.Name())
		metadataBytes, err := os.ReadFile(metadataPath)
		if err != nil {
			logging.Warn("Failed to read conflict metadata",
				zap.String("metadata_path", metadataPath),
				zap.Error(err))
			continue
		}
		var conflict ConflictMetadata
		if err := json.Unmarshal(metadataBytes, &conflict); err != nil {
			logging.Warn("Failed to decode conflict metadata",
				zap.String("metadata_path", metadataPath),
				zap.Error(err))
			continue
		}
		if conflict.Path == "" {
			logging.Warn("Skipping conflict metadata without original path",
				zap.String("metadata_path", metadataPath))
			continue
		}
		if conflict.PreservedMetadataPath == "" {
			conflict.PreservedMetadataPath = metadataPath
		}
		if conflict.PreservedPath == "" {
			conflict.PreservedPath = strings.TrimSuffix(metadataPath, ".metadata.json")
		}
		if _, err := os.Stat(conflict.PreservedPath); err != nil {
			if os.IsNotExist(err) {
				if removeErr := os.Remove(metadataPath); removeErr != nil && !os.IsNotExist(removeErr) {
					logging.Warn("Failed to remove stale conflict metadata",
						zap.String("metadata_path", metadataPath),
						zap.Error(removeErr))
				}
				continue
			}
			logging.Warn("Failed to stat preserved conflict data",
				zap.String("preserved_path", conflict.PreservedPath),
				zap.Error(err))
			continue
		}
		if conflict.ObjectKey == "" {
			conflict.ObjectKey = objectKeyFromPath(conflict.Path)
		}
		if conflict.ConflictStatus == "" {
			conflict.ConflictStatus = ConflictStatusConflicted
		}
		if conflict.ObservedETag == "" {
			conflict.ObservedETag = conflict.ExternalETag
			conflict.ObservedSize = conflict.ExternalSize
			conflict.ObservedLastModified = conflict.ExternalLastModified
		}
		sm.dirtyIndex.MarkConflicted(&conflict)
		recovered++
	}

	return recovered
}

// CleanupSession removes a session and optionally deletes the staging file
func (sm *StagingManager) CleanupSession(path string, deleteStagingFile bool) error {
	if deleteStagingFile && sm.dirtyIndex.IsDirty(path) && sm.dirtyIndex.IsSyncing(path) {
		logging.Warn("Refusing to cleanup staging session during active sync",
			zap.String("path", path),
			zap.String("event", "cleanup_skip"),
			zap.String("reason", "active_sync"))
		return fmt.Errorf("cannot cleanup active syncing session: %s", path)
	}

	sm.mu.Lock()
	session, exists := sm.sessions[path]
	if exists {
		delete(sm.sessions, path)
	}
	sm.mu.Unlock()

	if !exists {
		return nil
	}

	if session.Multipart != nil && session.Multipart.Active {
		logging.Warn("Cleaning session with active multipart state without aborting upload",
			zap.String("path", path),
			zap.String("upload_id", session.Multipart.UploadID),
			zap.String("multipart_state", "active"),
			zap.String("event", "cleanup"),
			zap.String("reason", "session_cleanup"))
	}

	// Close the session
	if err := session.Close(); err != nil {
		logging.Warn("Failed to close session during cleanup",
			zap.String("path", path),
			zap.Error(err))
	}

	// Delete staging file if requested
	if deleteStagingFile {
		if err := os.Remove(session.StagingPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove staging file: %w", err)
		}
		// Safely clear `.metadata` journals maintaining boundaries safely tracking S3 maps
		os.Remove(session.StagingPath + ".metadata")
		sm.ForgetDirty(path, "staging_file_deleted")

		logging.Debug("Removed staging file",
			zap.String("path", path),
			zap.String("staging_path", session.StagingPath))
	}

	sm.updatePressureMetrics()

	return nil
}

// Shutdown closes all sessions and cleans up
func (sm *StagingManager) Shutdown() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	logging.Info("Shutting down staging manager",
		zap.Int("active_sessions", len(sm.sessions)))

	// Close all sessions
	for path, session := range sm.sessions {
		if err := session.Close(); err != nil {
			logging.Warn("Failed to close session during shutdown",
				zap.String("path", path),
				zap.Error(err))
		}
	}

	sm.sessions = make(map[string]*WriteSession)

	return nil
}

// Stats returns statistics about the staging manager
func (sm *StagingManager) Stats() map[string]interface{} {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	activeSessions := 0
	for _, session := range sm.sessions {
		if session.GetRefCount() > 0 {
			activeSessions++
		}
	}

	pressure := sm.CurrentPressure()
	conflictCount, conflictedPaths, lastConflict := sm.ConflictStats()

	stats := map[string]interface{}{
		"total_sessions":          len(sm.sessions),
		"active_sessions":         activeSessions,
		"dirty_files":             sm.dirtyIndex.Count(),
		"syncing_files":           sm.dirtyIndex.SyncingCount(),
		"pending_deletes":         sm.PendingDeleteCount(),
		"conflict_count":          conflictCount,
		"conflicted_paths":        conflictedPaths,
		"staging_used_bytes":      pressure.UsedBytes,
		"staging_available_bytes": pressure.AvailableBytes,
		"staging_pressure_level":  pressure.Level,
	}
	if !lastConflict.IsZero() {
		stats["last_conflict_time"] = lastConflict.Format(time.RFC3339Nano)
	}
	return stats
}

// Made with Bob
