package staging

import (
	"sync"
	"time"
)

// DirtyFileIndex tracks which files are dirty (not yet synced to COS)
type DirtyFileIndex struct {
	dirty     map[string]*DirtyFileMetadata
	syncing   map[string]bool
	conflicts map[string]*ConflictMetadata
	// generation is the last LocalDirtyGeneration handed out.
	generation int64
	mu         sync.RWMutex
}

// DirtyFileMetadata contains metadata about a dirty file
type DirtyFileMetadata struct {
	Path                 string
	ObjectKey            string
	ObservedETag         string
	ObservedSize         int64
	ObservedLastModified time.Time
	// LocalDirtyGeneration identifies one state of the staged file: every
	// write, truncate and rename gives the entry a new one. It is kept only
	// here, in memory, and comes from a counter the whole index shares, so
	// no value is ever used twice, not even for a path dropped from the
	// index and marked dirty again. That is what lets an upload tell, when
	// it finishes, whether the file it read is still the file that is
	// staged (see MarkCleanIfUnchanged).
	LocalDirtyGeneration int64
	StagedPath           string
	ConflictStatus       string
	Size                 int64
	DirtySince           time.Time
	LastModified         time.Time
	SyncAttempts         int
	LastSyncError        error
	// recorded reports that the sidecar a restart recovers this file by
	// was on disk when the entry was last recorded.
	recorded bool
}

// ConflictMetadata records a dirty staged path whose COS object changed before
// the local staged bytes could be synced.
type ConflictMetadata struct {
	Path                  string
	ObjectKey             string
	Size                  int64
	PreservedPath         string
	PreservedMetadataPath string
	DetectedAt            time.Time
	Reason                string
	LocalDirtyGeneration  int64
	StagedPath            string
	ConflictStatus        string
	ObservedETag          string
	ObservedSize          int64
	ObservedLastModified  time.Time
	ExternalSize          int64
	ExternalETag          string
	ExternalLastModified  time.Time
	ExternalDeleted       bool
}

// NewDirtyFileIndex creates a new dirty file index
func NewDirtyFileIndex() *DirtyFileIndex {
	return &DirtyFileIndex{
		dirty:     make(map[string]*DirtyFileMetadata),
		syncing:   make(map[string]bool),
		conflicts: make(map[string]*ConflictMetadata),
	}
}

// MarkDirty marks a file as dirty (needs sync)
func (dfi *DirtyFileIndex) MarkDirty(path string, size int64) {
	dfi.MarkDirtyWithState(path, size, nil)
}

// MarkDirtyWithState marks a file as dirty and attaches the state of the
// sidecar recorded for the staged write. A nil state means the sidecar could
// not be written.
func (dfi *DirtyFileIndex) MarkDirtyWithState(path string, size int64, state *PathMetadataState) {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	if _, conflicted := dfi.conflicts[path]; conflicted {
		return
	}

	now := time.Now()
	meta, exists := dfi.dirty[path]
	if !exists {
		meta = &DirtyFileMetadata{
			Path:           path,
			ObjectKey:      objectKeyFromPath(path),
			ConflictStatus: ConflictStatusNone,
			DirtySince:     now,
			SyncAttempts:   0,
		}
		dfi.dirty[path] = meta
	}
	meta.Size = size
	meta.LastModified = now
	meta.LocalDirtyGeneration = dfi.nextGenerationLocked()
	meta.recorded = state != nil
	applyPathStateToDirtyMetadata(meta, state)
}

// MarkDirtyAgain records one more change to a file that is already dirty
// and whose sidecar is on disk, and reports whether it did. It touches
// nothing but memory. False means the path has to go through
// MarkDirtyWithState: it is clean, or its sidecar is yet to be written.
func (dfi *DirtyFileIndex) MarkDirtyAgain(path string, size int64) bool {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	meta, exists := dfi.dirty[path]
	if !exists || !meta.recorded {
		return false
	}
	meta.Size = size
	meta.LastModified = time.Now()
	meta.LocalDirtyGeneration = dfi.nextGenerationLocked()
	return true
}

// MarkUnrecorded records that the sidecar of a dirty path is not on disk, so
// its next change goes through MarkDirtyWithState and writes it.
func (dfi *DirtyFileIndex) MarkUnrecorded(path string) {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	if meta, exists := dfi.dirty[path]; exists {
		meta.recorded = false
	}
}

// nextGenerationLocked returns a LocalDirtyGeneration no entry has carried.
// Callers hold mu.
func (dfi *DirtyFileIndex) nextGenerationLocked() int64 {
	dfi.generation++
	return dfi.generation
}

// MarkClean marks a file as clean (synced to COS)
func (dfi *DirtyFileIndex) MarkClean(path string) {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	delete(dfi.dirty, path)
	delete(dfi.syncing, path)
}

// MarkCleanIfUnchanged marks a file clean, as MarkClean does, unless it has
// changed since it carried generation. clean reports whether it is now
// clean. False means a write, truncate or rename landed after generation was
// read: the entry stays as it is, dirty. Checking and cleaning are one step,
// so a change lands before it or after it, never in between, where it would
// be marked clean without having been uploaded. wasDirty reports whether the
// path had an entry at all; one without is clean already.
func (dfi *DirtyFileIndex) MarkCleanIfUnchanged(path string, generation int64) (wasDirty, clean bool) {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	meta, wasDirty := dfi.dirty[path]
	if wasDirty && meta.LocalDirtyGeneration != generation {
		return true, false
	}
	delete(dfi.dirty, path)
	delete(dfi.syncing, path)
	return wasDirty, true
}

// MarkConflicted records a conflict and removes the path from the upload queue.
func (dfi *DirtyFileIndex) MarkConflicted(meta *ConflictMetadata) {
	if meta == nil || meta.Path == "" {
		return
	}

	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	metaCopy := *meta
	delete(dfi.dirty, meta.Path)
	delete(dfi.syncing, meta.Path)
	dfi.conflicts[meta.Path] = &metaCopy
}

// RestoreDirty restores the dirty entry of a staged file found on disk with
// its sidecar.
func (dfi *DirtyFileIndex) RestoreDirty(meta DirtyFileMetadata) {
	if meta.Path == "" {
		return
	}

	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	if _, conflicted := dfi.conflicts[meta.Path]; conflicted {
		return
	}
	metaCopy := meta
	if metaCopy.ObjectKey == "" {
		metaCopy.ObjectKey = objectKeyFromPath(metaCopy.Path)
	}
	metaCopy.LocalDirtyGeneration = dfi.nextGenerationLocked()
	metaCopy.recorded = true
	if metaCopy.ConflictStatus == "" {
		metaCopy.ConflictStatus = ConflictStatusNone
	}
	dfi.dirty[meta.Path] = &metaCopy
}

// Invalidate gives the entry of path, if it has one, a new generation
// without otherwise changing it, so that an upload of the file in flight
// does not mark it clean when it finishes.
func (dfi *DirtyFileIndex) Invalidate(path string) {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	if meta, exists := dfi.dirty[path]; exists {
		meta.LocalDirtyGeneration = dfi.nextGenerationLocked()
	}
}

// DropEntry removes the dirty entry without touching sync claims, unlike
// MarkClean which also releases the path's syncing lock.
func (dfi *DirtyFileIndex) DropEntry(path string) {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	delete(dfi.dirty, path)
}

// Rekey moves the dirty entry for oldPath to newPath, replacing any existing
// destination entry (rename-over semantics). The syncing map is intentionally
// untouched: sync claims belong to the workers holding them, and the new
// generation invalidates any in-flight upload of either path. The caller has
// written the destination's sidecar.
func (dfi *DirtyFileIndex) Rekey(oldPath, newPath, stagedPath string) {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	now := time.Now()
	moved := DirtyFileMetadata{
		DirtySince:     now,
		ConflictStatus: ConflictStatusNone,
	}
	if meta, exists := dfi.dirty[oldPath]; exists {
		moved = *meta
		delete(dfi.dirty, oldPath)
	}
	moved.Path = newPath
	moved.ObjectKey = objectKeyFromPath(newPath)
	moved.StagedPath = stagedPath
	// The destination object's remote state is unknown; do not carry the
	// source's observed COS state across the rename.
	moved.ObservedETag = ""
	moved.ObservedSize = 0
	moved.ObservedLastModified = time.Time{}
	moved.LocalDirtyGeneration = dfi.nextGenerationLocked()
	moved.recorded = true
	moved.LastModified = now
	moved.SyncAttempts = 0
	moved.LastSyncError = nil
	dfi.dirty[newPath] = &moved
}

// LockFile securely claims the file for syncing by a background worker natively isolating multiple loops
func (dfi *DirtyFileIndex) LockFile(path string) bool {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	if dfi.syncing[path] {
		return false // Activity bound globally to another worker
	}
	dfi.syncing[path] = true
	return true
}

// UnlockFile securely releases the file sync bounds natively out of IBM pipelines
func (dfi *DirtyFileIndex) UnlockFile(path string) {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	delete(dfi.syncing, path)
}

// IsSyncing returns true if a file is currently claimed by a sync worker.
func (dfi *DirtyFileIndex) IsSyncing(path string) bool {
	dfi.mu.RLock()
	defer dfi.mu.RUnlock()

	return dfi.syncing[path]
}

// IsDirty returns true if the file is dirty
func (dfi *DirtyFileIndex) IsDirty(path string) bool {
	dfi.mu.RLock()
	defer dfi.mu.RUnlock()

	_, exists := dfi.dirty[path]
	return exists
}

// IsConflicted returns true when the path has an unresolved staging conflict.
func (dfi *DirtyFileIndex) IsConflicted(path string) bool {
	dfi.mu.RLock()
	defer dfi.mu.RUnlock()

	_, exists := dfi.conflicts[path]
	return exists
}

// GetDirtyFiles returns a list of all dirty files
func (dfi *DirtyFileIndex) GetDirtyFiles() []*DirtyFileMetadata {
	dfi.mu.RLock()
	defer dfi.mu.RUnlock()

	files := make([]*DirtyFileMetadata, 0, len(dfi.dirty))
	for _, meta := range dfi.dirty {
		// Create a copy to avoid race conditions
		metaCopy := *meta
		files = append(files, &metaCopy)
	}

	return files
}

// GetConflicts returns copies of all unresolved conflict records.
func (dfi *DirtyFileIndex) GetConflicts() []*ConflictMetadata {
	dfi.mu.RLock()
	defer dfi.mu.RUnlock()

	conflicts := make([]*ConflictMetadata, 0, len(dfi.conflicts))
	for _, meta := range dfi.conflicts {
		metaCopy := *meta
		conflicts = append(conflicts, &metaCopy)
	}

	return conflicts
}

// GetMetadata returns metadata for a specific file
func (dfi *DirtyFileIndex) GetMetadata(path string) *DirtyFileMetadata {
	dfi.mu.RLock()
	defer dfi.mu.RUnlock()

	if meta, exists := dfi.dirty[path]; exists {
		metaCopy := *meta
		return &metaCopy
	}

	return nil
}

func applyPathStateToDirtyMetadata(meta *DirtyFileMetadata, state *PathMetadataState) {
	if meta == nil || state == nil {
		return
	}
	if state.ObjectKey != "" {
		meta.ObjectKey = state.ObjectKey
	}
	meta.ObservedETag = state.ObservedETag
	meta.ObservedSize = state.ObservedSize
	meta.ObservedLastModified = state.ObservedLastModified
	if state.StagedFilePath != "" {
		meta.StagedPath = state.StagedFilePath
	}
	if state.ConflictStatus != "" {
		meta.ConflictStatus = state.ConflictStatus
	}
}

// IncrementSyncAttempts increments the sync attempt counter for a file
func (dfi *DirtyFileIndex) IncrementSyncAttempts(path string, err error) {
	dfi.mu.Lock()
	defer dfi.mu.Unlock()

	if meta, exists := dfi.dirty[path]; exists {
		meta.SyncAttempts++
		meta.LastSyncError = err
	}
}

// Count returns the number of dirty files
func (dfi *DirtyFileIndex) Count() int {
	dfi.mu.RLock()
	defer dfi.mu.RUnlock()

	return len(dfi.dirty)
}

// ConflictCount returns the number of unresolved conflict records.
func (dfi *DirtyFileIndex) ConflictCount() int {
	dfi.mu.RLock()
	defer dfi.mu.RUnlock()

	return len(dfi.conflicts)
}

// LastConflictTime returns the newest conflict timestamp.
func (dfi *DirtyFileIndex) LastConflictTime() time.Time {
	dfi.mu.RLock()
	defer dfi.mu.RUnlock()

	var last time.Time
	for _, meta := range dfi.conflicts {
		if meta.DetectedAt.After(last) {
			last = meta.DetectedAt
		}
	}
	return last
}

// SyncingCount returns the number of files currently claimed by sync workers.
func (dfi *DirtyFileIndex) SyncingCount() int {
	dfi.mu.RLock()
	defer dfi.mu.RUnlock()

	return len(dfi.syncing)
}

// Made with Bob
