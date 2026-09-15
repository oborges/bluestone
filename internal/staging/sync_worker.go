package staging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IBM/ibm-cos-sdk-go/service/s3"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/metrics"
	"github.com/oborges/bluestone/internal/posix"
	"go.uber.org/zap"
)

var errMultipartSnapshotChanged = errors.New("multipart snapshot changed during upload")

const syncStabilityDelay = 5 * time.Second

// COSClient interface for uploading objects
type COSClient interface {
	PutObject(ctx context.Context, key string, data []byte, metadata map[string]string) error
	PutObjectStream(ctx context.Context, key string, body io.ReadSeeker, metadata map[string]string) error
	GetObjectStream(ctx context.Context, key string) (io.ReadCloser, error)
	CreateMultipartUpload(ctx context.Context, key string, metadata map[string]string) (string, error)
	UploadPart(ctx context.Context, key, uploadID string, partNumber int64, body io.ReadSeeker) (string, error)
	CompleteMultipartUpload(ctx context.Context, key, uploadID string, completedParts []*s3.CompletedPart) error
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error
	DeleteObject(ctx context.Context, key string) error
}

// SyncWorker handles background synchronization of dirty files to COS
type SyncWorker struct {
	manager    *StagingManager
	cosClient  COSClient
	config     *config.StagingConfig
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	syncTicker *time.Ticker

	onObjectMutated func(path string)
	onObjectSynced  func(path string)

	uploadMu           sync.Mutex
	uploadByPath       map[string]uploadAccumulator
	objectLocksMu      sync.Mutex
	objectLocks        map[string]*sync.Mutex
	totalSyncedFiles   int64
	totalUploadedBytes int64
	lastSync           syncObservation
	recentSyncs        []syncObservation
}

type uploadAccumulator struct {
	bytes    int64
	duration time.Duration
}

type syncObservation struct {
	Path              string
	SizeBytes         int64
	UploadDuration    time.Duration
	VisibilityLatency time.Duration
	ThroughputMiBps   float64
	CompletedAt       time.Time
}

// NewSyncWorker creates a new sync worker
func NewSyncWorker(manager *StagingManager, cosClient COSClient, cfg *config.StagingConfig) *SyncWorker {
	ctx, cancel := context.WithCancel(context.Background())

	return &SyncWorker{
		manager:      manager,
		cosClient:    cosClient,
		config:       cfg,
		ctx:          ctx,
		cancel:       cancel,
		uploadByPath: make(map[string]uploadAccumulator),
		objectLocks:  make(map[string]*sync.Mutex),
	}
}

// Start begins the background sync process
func (sw *SyncWorker) Start() {
	syncInterval, _ := sw.config.GetSyncInterval()
	sw.syncTicker = time.NewTicker(syncInterval)

	// Start worker goroutines
	for i := 0; i < sw.config.SyncWorkerCount; i++ {
		sw.wg.Add(1)
		go sw.workerLoop(i)
	}

	logging.Info("Sync worker started",
		zap.Int("workers", sw.config.SyncWorkerCount),
		zap.Duration("interval", syncInterval))
}

// Stop gracefully stops the sync worker
func (sw *SyncWorker) Stop() {
	logging.Info("Stopping sync worker")

	if sw.syncTicker != nil {
		sw.syncTicker.Stop()
	}

	sw.cancel()
	sw.wg.Wait()

	logging.Info("Sync worker stopped")
}

// workerLoop is the main loop for each worker goroutine
func (sw *SyncWorker) workerLoop(workerID int) {
	defer sw.wg.Done()

	logging.Debug("Sync worker started",
		zap.Int("worker_id", workerID))

	for {
		select {
		case <-sw.ctx.Done():
			logging.Debug("Sync worker stopping",
				zap.Int("worker_id", workerID))
			return

		case <-sw.syncTicker.C:
			sw.manager.updateSyncQueueMetrics()
			sw.processPendingDeletes(workerID)
			sw.processDirtyFiles(workerID)
			sw.manager.updateSyncQueueMetrics()
		}
	}
}

// SetObjectMutatedCallback registers a hook invoked after the sync worker
// removes an object from COS (deferred deletes), so upstream caches never
// serve state observed before the mutation. Must be called before Start.
func (sw *SyncWorker) SetObjectMutatedCallback(fn func(path string)) {
	sw.onObjectMutated = fn
}

// SetObjectSyncedCallback registers a hook invoked after the sync worker
// uploads an object. Uploads do not change the namespace, so the handler can
// invalidate more narrowly than for deletes. Must be called before Start.
func (sw *SyncWorker) SetObjectSyncedCallback(fn func(path string)) {
	sw.onObjectSynced = fn
}

func (sw *SyncWorker) notifyObjectMutated(path string) {
	if sw.onObjectMutated != nil {
		sw.onObjectMutated(path)
	}
}

func (sw *SyncWorker) notifyObjectSynced(path string) {
	if sw.onObjectSynced != nil {
		sw.onObjectSynced(path)
	}
}

// processPendingDeletes completes accepted deletes whose COS object is not
// confirmed gone yet. A tombstone is only resolved after the object delete
// succeeds, so failures are retried on every tick.
func (sw *SyncWorker) processPendingDeletes(workerID int) {
	for _, tomb := range sw.manager.PendingDeletes() {
		path := tomb.Path

		// Claiming the sync lock guarantees no upload is in flight for the
		// path; deletes must be ordered after any in-flight upload.
		if !sw.manager.dirtyIndex.LockFile(path) {
			continue
		}

		// Re-check under the lock: the path may have been recreated.
		if !sw.manager.HasPendingDelete(path) {
			sw.manager.dirtyIndex.UnlockFile(path)
			continue
		}

		// The accepted delete supersedes any staged bytes left for the path.
		sw.manager.ForgetDirty(path, "delete_pending")
		if err := sw.manager.CleanupSession(path, true); err != nil {
			logging.Warn("Failed to cleanup staged data for pending delete",
				zap.Int("worker_id", workerID),
				zap.String("path", path),
				zap.Error(err))
		}

		ctx, cancel := context.WithTimeout(sw.ctx, 60*time.Second)
		err := sw.cosClient.DeleteObject(ctx, tomb.ObjectKey)
		cancel()
		if err != nil {
			logging.Error("Failed to delete COS object for pending delete; will retry",
				zap.Int("worker_id", workerID),
				zap.String("path", path),
				zap.String("object_key", tomb.ObjectKey),
				zap.Error(err))
			sw.manager.dirtyIndex.UnlockFile(path)
			continue
		}

		sw.manager.ResolvePendingDelete(path)
		sw.manager.dirtyIndex.UnlockFile(path)
		sw.notifyObjectMutated(path)

		logging.Info("Completed deferred delete",
			zap.Int("worker_id", workerID),
			zap.String("path", path),
			zap.String("object_key", tomb.ObjectKey))
	}
}

// processDirtyFiles processes all dirty files that need syncing
func (sw *SyncWorker) processDirtyFiles(workerID int) {
	dirtyFiles := sw.manager.GetDirtyFiles()

	if len(dirtyFiles) == 0 {
		return
	}

	logging.Debug("Processing dirty files",
		zap.Int("worker_id", workerID),
		zap.Int("count", len(dirtyFiles)))

	for _, metadata := range dirtyFiles {
		// Check if we should sync this file
		if !sw.shouldSync(metadata) {
			continue
		}
		if sw.manager.IsConflicted(metadata.Path) {
			logging.Warn("Skipping sync for conflicted staged path",
				zap.Int("worker_id", workerID),
				zap.String("path", metadata.Path))
			continue
		}

		if !sw.manager.dirtyIndex.LockFile(metadata.Path) {
			continue
		}

		// Double-check the file is still dirty after acquiring the lock (avoids snapshot race conditions between workers)
		if !sw.manager.dirtyIndex.IsDirty(metadata.Path) {
			sw.manager.dirtyIndex.UnlockFile(metadata.Path)
			continue
		}

		// Sync the file
		if err := sw.syncFileWithWorker(metadata.Path, workerID); err != nil {
			logging.Error("Failed to sync file",
				zap.Int("worker_id", workerID),
				zap.String("path", metadata.Path),
				zap.Error(err))

			// Update error count
			sw.manager.dirtyIndex.IncrementSyncAttempts(metadata.Path, err)
		} else {
			logging.Info("Successfully synced file",
				zap.Int("worker_id", workerID),
				zap.String("path", metadata.Path),
				zap.Int64("size", metadata.Size))
		}
		sw.manager.dirtyIndex.UnlockFile(metadata.Path)
	}
}

// shouldSync determines if a file should be synced based on triggers
func (sw *SyncWorker) shouldSync(metadata *DirtyFileMetadata) bool {
	if sw.manager.IsConflicted(metadata.Path) {
		return false
	}

	now := time.Now()
	if !metadata.LastModified.IsZero() {
		idleSinceModified := now.Sub(metadata.LastModified)
		if idleSinceModified < syncStabilityDelay {
			logging.Debug("Deferring sync until dirty file is stable",
				zap.String("path", metadata.Path),
				zap.Duration("idle_since_modified", idleSinceModified),
				zap.Duration("required_idle", syncStabilityDelay))
			return false
		}
	}

	session, sessionExists := sw.manager.GetSession(metadata.Path)
	if sessionExists {
		_, _, _, _, _, refCount, lastWrite, _ := session.Snapshot()
		if refCount > 0 {
			idleTime := now.Sub(lastWrite)
			if idleTime < syncStabilityDelay {
				logging.Debug("Deferring sync while file is actively being written",
					zap.String("path", metadata.Path),
					zap.Int32("ref_count", refCount),
					zap.Duration("idle_time", idleTime))
				return false
			}
		}
	}

	// Quick-Flush 80% Buffer Disk Quotas
	totalSize := sw.manager.GetTotalStagingSize()
	maxSize := sw.manager.config.MaxStagingSizeGB * 1024 * 1024 * 1024
	if maxSize > 0 && float64(totalSize) >= float64(maxSize)*0.8 {
		logging.Warn("Disk Quota at 80%, actively forcing flush", zap.String("path", metadata.Path))
		return true
	}

	// Check size threshold (convert MB to bytes)
	syncThreshold := sw.config.SyncThresholdMB * 1024 * 1024
	if metadata.Size >= syncThreshold {
		logging.Debug("File exceeds size threshold",
			zap.String("path", metadata.Path),
			zap.Int64("size", metadata.Size),
			zap.Int64("threshold", syncThreshold))
		return true
	}

	// Check age threshold
	maxDirtyAge, _ := sw.config.GetMaxDirtyAge()
	age := now.Sub(metadata.DirtySince)
	if age >= maxDirtyAge {
		logging.Debug("File exceeds age threshold",
			zap.String("path", metadata.Path),
			zap.Duration("age", age),
			zap.Duration("threshold", maxDirtyAge))
		return true
	}

	// Check if session is idle (no active handles)
	if sessionExists && session.GetRefCount() == 0 {
		// Session is idle, check idle time
		_, _, _, _, _, _, lastWrite, _ := session.Snapshot()
		idleTime := now.Sub(lastWrite)
		if idleTime >= syncStabilityDelay {
			logging.Debug("File session is idle",
				zap.String("path", metadata.Path),
				zap.Duration("idle_time", idleTime))
			return true
		}
	}

	return false
}

// syncFile synchronizes a single file to COS
func (sw *SyncWorker) syncFile(path string) error {
	return sw.syncFileWithWorker(path, -1)
}

func (sw *SyncWorker) syncFileWithWorker(path string, workerID int) error {
	unlockObject := sw.lockObject(path)
	defer unlockObject()

	return sw.syncFileLocked(path, workerID)
}

func (sw *SyncWorker) syncFileLocked(path string, workerID int) error {
	if sw.manager.IsConflicted(path) {
		return fmt.Errorf("%w: %s", ErrPathConflicted, path)
	}

	if sw.manager.HasPendingDelete(path) {
		logging.Info("Skipping sync for path with pending delete",
			zap.Int("worker_id", workerID),
			zap.String("path", path))
		return nil
	}

	// Get the session
	session, exists := sw.manager.GetSession(path)
	if !exists {
		recovered, err := sw.manager.RecoverSessionFromStaging(path)
		if err != nil {
			if os.IsNotExist(err) {
				sw.manager.ForgetDirty(path, "missing_session_and_staging_file")
				logging.Info("Dropped orphaned dirty staging entry",
					zap.Int("worker_id", workerID),
					zap.String("path", path),
					zap.String("reason", "missing_session_and_staging_file"))
				return nil
			}
			return fmt.Errorf("session not found for path: %s: %w", path, err)
		}
		session = recovered
	}
	dirtySince := time.Now()
	var snapshotLastModified time.Time
	if metadata := sw.manager.dirtyIndex.GetMetadata(path); metadata != nil && !metadata.DirtySince.IsZero() {
		dirtySince = metadata.DirtySince
		snapshotLastModified = metadata.LastModified
	}

	// Sync the session (flushes to staging file)
	if err := session.Sync(); err != nil {
		return fmt.Errorf("failed to sync session: %w", err)
	}
	stagingPath, size, _, _, _, _, lastWrite, multipartPartSize := session.Snapshot()
	if multipartPartSize <= 0 {
		multipartPartSize = 20 * 1024 * 1024
	}

	// Read staging file using file stream to prevent OOM on large files
	file, err := os.Open(stagingPath)
	if err != nil {
		return fmt.Errorf("failed to open staging file: %w", err)
	}
	defer file.Close()

	if size >= multipartPartSize {
		// Upload with every attribute the staged file carries, not just mode
		// and owner: the upload replaces the object's metadata.
		cosMetadata := posix.EncodePOSIXAttributes(session.Attributes().POSIX())
		if session.Multipart != nil {
			session.Multipart.Reset()
		}
		isSnapshotCurrent := func() bool {
			_, currentSize, _, _, _, _, currentLastWrite, _ := session.Snapshot()
			if currentSize != size || !currentLastWrite.Equal(lastWrite) {
				return false
			}
			if currentMetadata := sw.manager.dirtyIndex.GetMetadata(path); currentMetadata != nil && currentMetadata.LastModified.After(snapshotLastModified) {
				return false
			}
			return true
		}
		if err := sw.uploadMultipartWithRetry(path, file, size, multipartPartSize, cosMetadata, workerID, isSnapshotCurrent); err != nil {
			if session.Multipart != nil {
				session.Multipart.Reset()
			}
			return fmt.Errorf("failed to multipart upload to COS: %w", err)
		}
		if session.Multipart != nil {
			session.Multipart.Reset()
		}
	} else {
		uploadStart := time.Now()
		var monolithicReader io.ReadSeeker = file
		mmapReader, err := NewMMapReader(file, 0, size)
		if err == nil && size > 0 {
			defer mmapReader.Close()
			monolithicReader = mmapReader
		}

		// Upload with every attribute the staged file carries, not just mode
		// and owner: the upload replaces the object's metadata.
		cosMetadata := posix.EncodePOSIXAttributes(session.Attributes().POSIX())

		// Upload to COS with retry (monolithic loop)
		if err := sw.uploadWithRetryStream(path, monolithicReader, cosMetadata); err != nil {
			return fmt.Errorf("failed to upload to COS: %w", err)
		}
		sw.addUploadSample(path, size, time.Since(uploadStart))
	}

	if currentMetadata := sw.manager.dirtyIndex.GetMetadata(path); currentMetadata != nil && currentMetadata.LastModified.After(snapshotLastModified) {
		return fmt.Errorf("snapshot changed during upload; leaving file dirty for retry")
	}

	uploadedBytes, uploadDuration := sw.consumeUploadSamples(path)
	if uploadedBytes == 0 && size > 0 {
		uploadedBytes = size
	}
	sw.recordSuccessfulSync(path, uploadedBytes, dirtySince, uploadDuration)

	// Mark as clean
	sw.manager.MarkClean(path)

	// The object just changed in COS; caches that observed the pre-sync
	// object (e.g. a stat during the dirty window) must not outlive it.
	sw.notifyObjectSynced(path)

	if sw.config.CleanAfterSync && session.GetRefCount() == 0 {
		if err := sw.manager.CleanupSession(path, true); err != nil {
			return fmt.Errorf("failed to cleanup synced staging session: %w", err)
		}
		logging.Info("Cleaned synced staging session",
			zap.String("path", path))
	}

	return nil
}

func (sw *SyncWorker) lockObject(path string) func() {
	sw.objectLocksMu.Lock()
	lock := sw.objectLocks[path]
	if lock == nil {
		lock = &sync.Mutex{}
		sw.objectLocks[path] = lock
	}
	sw.objectLocksMu.Unlock()

	lock.Lock()
	return lock.Unlock
}

func (sw *SyncWorker) uploadMultipartWithRetry(path string, file *os.File, size, partSize int64, metadata map[string]string, workerID int, isSnapshotCurrent func() bool) error {
	var lastErr error

	retryBackoff, _ := sw.config.GetRetryBackoffInitial()
	maxRetryBackoff, _ := sw.config.GetRetryBackoffMax()

	for attempt := 0; attempt < sw.config.MaxSyncRetries; attempt++ {
		if attempt > 0 {
			backoff := retryBackoff * time.Duration(1<<uint(attempt-1))
			if backoff > maxRetryBackoff {
				backoff = maxRetryBackoff
			}
			sw.logMultipartEvent("retry", path, "", "retrying", workerID, 0, 0, "", fmt.Sprintf("attempt=%d backoff=%s previous_error=%v", attempt+1, backoff, lastErr), nil)
			select {
			case <-time.After(backoff):
			case <-sw.ctx.Done():
				return sw.ctx.Err()
			}
		}

		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("failed to rewind staging file: %w", err)
		}
		if err := sw.uploadMultipartOnce(path, file, size, partSize, metadata, workerID, isSnapshotCurrent); err == nil {
			return nil
		} else {
			lastErr = err
			if errors.Is(err, errMultipartSnapshotChanged) {
				return err
			}
			if isNoSuchUpload(err) {
				sw.logMultipartEvent("error", path, "", "invalid_upload_id", workerID, 0, 0, "", "NoSuchUpload; restarting multipart from clean upload session", err)
			}
		}
	}

	return fmt.Errorf("multipart upload failed after %d attempts: %w", sw.config.MaxSyncRetries, lastErr)
}

func (sw *SyncWorker) uploadMultipartOnce(path string, file *os.File, size, partSize int64, metadata map[string]string, workerID int, isSnapshotCurrent func() bool) error {
	uploadID, err := sw.cosClient.CreateMultipartUpload(sw.ctx, path, metadata)
	if err != nil {
		sw.logMultipartEvent("error", path, "", "create_failed", workerID, 0, 0, "", "create_multipart_failed", err)
		return err
	}
	sw.logMultipartEvent("create", path, uploadID, "active", workerID, 0, 0, "", "new_multipart_upload", nil)

	completed := false
	abortReason := "error"
	defer func() {
		if completed {
			return
		}
		if abortErr := sw.cosClient.AbortMultipartUpload(sw.ctx, path, uploadID); abortErr != nil {
			sw.logMultipartEvent("error", path, uploadID, "abort_failed", workerID, 0, 0, "", abortReason, abortErr)
			return
		}
		sw.logMultipartEvent("abort", path, uploadID, "aborted", workerID, 0, 0, "", abortReason, nil)
	}()

	completedParts := make([]*s3.CompletedPart, 0, (size+partSize-1)/partSize)
	for offset, partNumber := int64(0), int64(1); offset < size; offset, partNumber = offset+partSize, partNumber+1 {
		currentPartSize := partSize
		if remaining := size - offset; remaining < currentPartSize {
			currentPartSize = remaining
		}

		uploadReader, closeReader := sw.multipartPartReader(file, offset, currentPartSize)
		partStart := time.Now()
		etag, err := sw.cosClient.UploadPart(sw.ctx, path, uploadID, partNumber, uploadReader)
		closeReader()
		if err != nil {
			abortReason = fmt.Sprintf("upload_part_failed part=%d", partNumber)
			sw.logMultipartEvent("error", path, uploadID, "active", workerID, partNumber, currentPartSize, "", abortReason, err)
			return err
		}

		sw.addUploadSample(path, currentPartSize, time.Since(partStart))
		completedParts = append(completedParts, &s3.CompletedPart{
			ETag:       &etag,
			PartNumber: &partNumber,
		})
		sw.logMultipartEvent("upload_part", path, uploadID, "active", workerID, partNumber, currentPartSize, etag, "part_uploaded", nil)
	}

	sort.Slice(completedParts, func(i, j int) bool {
		return *completedParts[i].PartNumber < *completedParts[j].PartNumber
	})

	if !isSnapshotCurrent() {
		abortReason = "snapshot_changed_before_complete"
		sw.logMultipartEvent("abort", path, uploadID, "snapshot_changed", workerID, 0, size, "", abortReason, nil)
		return errMultipartSnapshotChanged
	}

	if err := sw.cosClient.CompleteMultipartUpload(sw.ctx, path, uploadID, completedParts); err != nil {
		abortReason = "complete_failed"
		sw.logMultipartEvent("error", path, uploadID, "complete_failed", workerID, 0, 0, "", abortReason, err)
		return err
	}
	completed = true
	sw.logMultipartEvent("complete", path, uploadID, "completed", workerID, 0, size, "", fmt.Sprintf("parts=%d", len(completedParts)), nil)
	return nil
}

func (sw *SyncWorker) multipartPartReader(file *os.File, offset, size int64) (io.ReadSeeker, func()) {
	mmapReader, err := NewMMapReader(file, offset, size)
	if err == nil {
		return mmapReader, func() { _ = mmapReader.Close() }
	}
	return io.NewSectionReader(file, offset, size), func() {}
}

func (sw *SyncWorker) logMultipartEvent(event, path, uploadID, state string, workerID int, partNumber, partSize int64, etag, reason string, err error) {
	fields := []zap.Field{
		zap.String("path", path),
		zap.String("upload_id", uploadID),
		zap.String("multipart_state", state),
		zap.Int("worker_id", workerID),
		zap.Int64("part_number", partNumber),
		zap.Int64("part_size", partSize),
		zap.String("etag", etag),
		zap.String("event", event),
		zap.String("reason", reason),
	}
	if err != nil {
		fields = append(fields, zap.Error(err))
		logging.Error("Multipart upload lifecycle event", fields...)
		return
	}
	logging.Info("Multipart upload lifecycle event", fields...)
}

func isNoSuchUpload(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "NoSuchUpload") || strings.Contains(message, "Invalid uploadId")
}

func (sw *SyncWorker) addUploadSample(path string, bytes int64, duration time.Duration) {
	if bytes <= 0 {
		return
	}

	sw.uploadMu.Lock()
	defer sw.uploadMu.Unlock()

	acc := sw.uploadByPath[path]
	acc.bytes += bytes
	acc.duration += duration
	sw.uploadByPath[path] = acc
}

func (sw *SyncWorker) consumeUploadSamples(path string) (int64, time.Duration) {
	sw.uploadMu.Lock()
	defer sw.uploadMu.Unlock()

	acc := sw.uploadByPath[path]
	delete(sw.uploadByPath, path)
	return acc.bytes, acc.duration
}

func (sw *SyncWorker) recordSuccessfulSync(path string, uploadedBytes int64, dirtySince time.Time, uploadDuration time.Duration) {
	visibilityLatency := time.Since(dirtySince)
	throughputMiBps := float64(0)
	if uploadDuration > 0 {
		throughputMiBps = (float64(uploadedBytes) / (1024 * 1024)) / uploadDuration.Seconds()
	}

	metrics.RecordStagingUpload(uploadedBytes, uploadDuration, visibilityLatency)

	sw.uploadMu.Lock()
	sw.totalSyncedFiles++
	sw.totalUploadedBytes += uploadedBytes
	observation := syncObservation{
		Path:              path,
		SizeBytes:         uploadedBytes,
		UploadDuration:    uploadDuration,
		VisibilityLatency: visibilityLatency,
		ThroughputMiBps:   throughputMiBps,
		CompletedAt:       time.Now(),
	}
	sw.lastSync = observation
	sw.recentSyncs = append([]syncObservation{observation}, sw.recentSyncs...)
	if len(sw.recentSyncs) > 50 {
		sw.recentSyncs = sw.recentSyncs[:50]
	}
	sw.uploadMu.Unlock()

	logging.Info("Staging file visible in COS",
		zap.String("path", path),
		zap.Int64("bytes", uploadedBytes),
		zap.Duration("upload_duration", uploadDuration),
		zap.Float64("upload_mib_per_second", throughputMiBps),
		zap.Duration("cos_visibility_latency", visibilityLatency))
}

// uploadWithRetry uploads a file to COS with exponential backoff retry
func (sw *SyncWorker) uploadWithRetry(path string, data []byte, metadata map[string]string) error {
	var lastErr error

	retryBackoff, _ := sw.config.GetRetryBackoffInitial()
	maxRetryBackoff, _ := sw.config.GetRetryBackoffMax()

	for attempt := 0; attempt < sw.config.MaxSyncRetries; attempt++ {
		if attempt > 0 {
			// Calculate backoff delay
			backoff := retryBackoff * time.Duration(1<<uint(attempt-1))
			if backoff > maxRetryBackoff {
				backoff = maxRetryBackoff
			}

			logging.Debug("Retrying upload after backoff",
				zap.String("path", path),
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff))

			select {
			case <-time.After(backoff):
			case <-sw.ctx.Done():
				return sw.ctx.Err()
			}
		}

		// Attempt upload
		err := sw.cosClient.PutObject(sw.ctx, path, data, metadata)
		if err == nil {
			if attempt > 0 {
				logging.Info("Upload succeeded after retry",
					zap.String("path", path),
					zap.Int("attempts", attempt+1))
			}
			return nil
		}

		lastErr = err
		logging.Warn("Upload attempt failed",
			zap.String("path", path),
			zap.Int("attempt", attempt+1),
			zap.Error(err))
	}

	return fmt.Errorf("upload failed after %d attempts: %w", sw.config.MaxSyncRetries, lastErr)
}

// uploadWithRetryStream uploads a file object stream to COS with exponential backoff retry
func (sw *SyncWorker) uploadWithRetryStream(path string, body io.ReadSeeker, metadata map[string]string) error {
	var lastErr error

	retryBackoff, _ := sw.config.GetRetryBackoffInitial()
	maxRetryBackoff, _ := sw.config.GetRetryBackoffMax()

	for attempt := 0; attempt < sw.config.MaxSyncRetries; attempt++ {
		if attempt > 0 {
			backoff := retryBackoff * time.Duration(1<<uint(attempt-1))
			if backoff > maxRetryBackoff {
				backoff = maxRetryBackoff
			}

			logging.Debug("Retrying upload stream after backoff",
				zap.String("path", path),
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff))

			select {
			case <-time.After(backoff):
			case <-sw.ctx.Done():
				return sw.ctx.Err()
			}
		}

		// Ensure body is rewound before upload retry
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("failed to seek upload body: %w", err)
		}

		err := sw.cosClient.PutObjectStream(sw.ctx, path, body, metadata)
		if err == nil {
			if attempt > 0 {
				logging.Info("Upload stream succeeded after retry",
					zap.String("path", path),
					zap.Int("attempts", attempt+1))
			}
			return nil
		}

		lastErr = err
		logging.Warn("Upload stream attempt failed",
			zap.String("path", path),
			zap.Int("attempt", attempt+1),
			zap.Error(err))
	}

	return fmt.Errorf("upload stream failed after %d attempts: %w", sw.config.MaxSyncRetries, lastErr)
}

// TriggerSync manually triggers a sync for a specific file
func (sw *SyncWorker) TriggerSync(path string) error {
	unlockObject := sw.lockObject(path)
	defer unlockObject()

	if sw.manager.IsConflicted(path) {
		return fmt.Errorf("%w: %s", ErrPathConflicted, path)
	}

	if !sw.manager.IsDirty(path) {
		return nil // Nothing to sync
	}

	logging.Info("Manual sync triggered",
		zap.String("path", path))

	return sw.syncFileLocked(path, -1)
}

// Stats returns statistics about the sync worker
func (sw *SyncWorker) Stats() map[string]interface{} {
	sw.manager.updateSyncQueueMetrics()
	dirtyFiles := sw.manager.GetDirtyFiles()

	totalSize := int64(0)
	oldestDirty := time.Time{}
	now := time.Now()
	pendingFiles := make([]map[string]interface{}, 0, len(dirtyFiles))

	sort.Slice(dirtyFiles, func(i, j int) bool {
		return dirtyFiles[i].DirtySince.Before(dirtyFiles[j].DirtySince)
	})

	for _, metadata := range dirtyFiles {
		totalSize += metadata.Size
		if oldestDirty.IsZero() || metadata.DirtySince.Before(oldestDirty) {
			oldestDirty = metadata.DirtySince
		}
		syncing := sw.manager.dirtyIndex.IsSyncing(metadata.Path)
		status := "queued"
		progress := 0
		if syncing {
			status = "syncing"
			progress = 65
		}
		entry := map[string]interface{}{
			"path":             metadata.Path,
			"size_bytes":       metadata.Size,
			"dirty_since":      metadata.DirtySince.Format(time.RFC3339Nano),
			"age_seconds":      now.Sub(metadata.DirtySince).Seconds(),
			"sync_attempts":    metadata.SyncAttempts,
			"status":           status,
			"progress_percent": progress,
		}
		if metadata.LastSyncError != nil {
			entry["last_error"] = metadata.LastSyncError.Error()
		}
		pendingFiles = append(pendingFiles, entry)
	}

	depth, queueBytes, oldestAge := sw.manager.SyncQueueStats()
	pressure := sw.manager.CurrentPressure()
	conflictCount, conflictedPaths, lastConflict := sw.manager.ConflictStats()
	sw.uploadMu.Lock()
	lastSync := sw.lastSync
	recentSyncs := append([]syncObservation(nil), sw.recentSyncs...)
	totalSyncedFiles := sw.totalSyncedFiles
	totalUploadedBytes := sw.totalUploadedBytes
	sw.uploadMu.Unlock()

	stats := map[string]interface{}{
		"dirty_files":             len(dirtyFiles),
		"total_size":              totalSize,
		"worker_count":            sw.config.SyncWorkerCount,
		"sync_queue_depth":        depth,
		"sync_queue_bytes":        queueBytes,
		"syncing_files":           sw.manager.dirtyIndex.SyncingCount(),
		"pending_deletes":         sw.manager.PendingDeleteCount(),
		"conflict_count":          conflictCount,
		"conflicted_paths":        conflictedPaths,
		"total_synced_files":      totalSyncedFiles,
		"total_uploaded_bytes":    totalUploadedBytes,
		"staging_used_bytes":      pressure.UsedBytes,
		"staging_available_bytes": pressure.AvailableBytes,
		"staging_pressure_level":  pressure.Level,
		"pending_files":           pendingFiles,
		"recent_synced_files":     formatSyncObservations(recentSyncs),
	}

	if !oldestDirty.IsZero() {
		stats["oldest_dirty_age"] = time.Since(oldestDirty).String()
	}
	if oldestAge > 0 {
		stats["oldest_dirty_age_seconds"] = oldestAge.Seconds()
	}
	if !lastConflict.IsZero() {
		stats["last_conflict_time"] = lastConflict.Format(time.RFC3339Nano)
	}
	if !lastSync.CompletedAt.IsZero() {
		stats["last_sync"] = map[string]interface{}{
			"path":                           lastSync.Path,
			"bytes":                          lastSync.SizeBytes,
			"upload_duration_seconds":        lastSync.UploadDuration.Seconds(),
			"cos_visibility_latency_seconds": lastSync.VisibilityLatency.Seconds(),
			"upload_mib_per_second":          lastSync.ThroughputMiBps,
			"completed_at":                   lastSync.CompletedAt.Format(time.RFC3339Nano),
		}
	}

	return stats
}

func formatSyncObservations(observations []syncObservation) []map[string]interface{} {
	files := make([]map[string]interface{}, 0, len(observations))
	for _, observation := range observations {
		files = append(files, map[string]interface{}{
			"path":                           observation.Path,
			"bytes":                          observation.SizeBytes,
			"upload_duration_seconds":        observation.UploadDuration.Seconds(),
			"cos_visibility_latency_seconds": observation.VisibilityLatency.Seconds(),
			"upload_mib_per_second":          observation.ThroughputMiBps,
			"completed_at":                   observation.CompletedAt.Format(time.RFC3339Nano),
			"status":                         "synced",
			"progress_percent":               100,
		})
	}
	return files
}

// processMultipartFiles processes progressive multi-part chunk streaming
func (sw *SyncWorker) processMultipartFiles(workerID int) {
	logging.Debug("Progressive multipart pre-upload disabled; multipart lifecycle is owned by final sync",
		zap.Int("worker_id", workerID))
}

// Made with Bob

// UploadToCOS uploads data directly to COS (used for immediate sync before delete)
func (sw *SyncWorker) UploadToCOS(ctx context.Context, path string, data []byte, metadata map[string]string) error {
	if err := sw.uploadWithRetry(path, data, metadata); err != nil {
		return err
	}
	sw.notifyObjectSynced(path)
	return nil
}
