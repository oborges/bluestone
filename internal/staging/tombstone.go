package staging

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

const tombstoneVersion = 1

// TombstoneState is the durable record of a delete accepted for a dirty staged
// path. It survives restarts so an accepted delete can never resurrect: the
// staged bytes are discarded and the COS object is removed by the sync worker
// before the tombstone is resolved.
type TombstoneState struct {
	Version   int       `json:"version"`
	Path      string    `json:"path"`
	ObjectKey string    `json:"object_key"`
	CreatedAt time.Time `json:"created_at"`
}

func (sm *StagingManager) tombstoneDir() string {
	return filepath.Join(sm.stagingRoot, "tombstones")
}

func (sm *StagingManager) tombstoneFilePath(path string) string {
	hash := sha256.Sum256([]byte(path))
	return filepath.Join(sm.tombstoneDir(), hex.EncodeToString(hash[:16])+".tombstone.json")
}

func writeTombstoneState(tombstonePath string, state *TombstoneState) error {
	if err := os.MkdirAll(filepath.Dir(tombstonePath), 0700); err != nil {
		return err
	}
	stateBytes, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := tombstonePath + ".tmp"
	if err := os.WriteFile(tmpPath, stateBytes, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, tombstonePath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// addTombstone durably records that the object for path must be removed from
// COS. It only persists intent; callers handle staged bytes and dirty state.
func (sm *StagingManager) addTombstone(path string) (*TombstoneState, error) {
	tomb := &TombstoneState{
		Version:   tombstoneVersion,
		Path:      path,
		ObjectKey: objectKeyFromPath(path),
		CreatedAt: time.Now(),
	}

	sm.tombstoneMu.Lock()
	if err := writeTombstoneState(sm.tombstoneFilePath(path), tomb); err != nil {
		sm.tombstoneMu.Unlock()
		return nil, fmt.Errorf("failed to persist delete tombstone: %w", err)
	}
	sm.tombstones[path] = tomb
	sm.tombstoneMu.Unlock()

	return tomb, nil
}

// TryLockSync attempts to claim the per-path sync lock, preventing sync
// workers (uploads and tombstone processing) from touching the path. Returns
// false when a worker already holds the claim. Callers must UnlockSync.
func (sm *StagingManager) TryLockSync(path string) bool {
	return sm.dirtyIndex.LockFile(path)
}

// UnlockSync releases a claim taken with TryLockSync.
func (sm *StagingManager) UnlockSync(path string) {
	sm.dirtyIndex.UnlockFile(path)
}

// RegisterPendingDelete accepts a delete of a dirty staged path with POSIX
// write-back semantics: the staged bytes are discarded rather than failing the
// unlink. A durable tombstone is persisted first so the delete survives a
// crash. Returns immediate=true when the caller can proceed to delete the COS
// object now; immediate=false means an upload for the path is in flight and
// the sync worker completes the delete after the upload finishes.
func (sm *StagingManager) RegisterPendingDelete(path string) (bool, error) {
	tomb, err := sm.addTombstone(path)
	if err != nil {
		return false, err
	}

	logging.Info("Registered pending delete for dirty staged path",
		zap.String("path", path),
		zap.String("object_key", tomb.ObjectKey))

	// Claim the sync lock before touching dirty state: ForgetDirty releases
	// the syncing claim as a side effect, so probing after it would steal an
	// active worker's lock. If a worker already claimed the path, its upload
	// is in flight and must complete before the object delete to preserve
	// ordering; the worker's tombstone pass finishes the delete.
	if !sm.dirtyIndex.LockFile(path) {
		return false, nil
	}
	defer sm.dirtyIndex.UnlockFile(path)

	// Drop the pending sync so workers stop considering this path.
	sm.ForgetDirty(path, "delete_pending")

	if err := sm.CleanupSession(path, true); err != nil {
		// Leftover staging data is reaped on restart because the tombstone
		// supersedes it during recovery.
		logging.Warn("Failed to cleanup staged data for pending delete",
			zap.String("path", path),
			zap.Error(err))
	}
	return true, nil
}

// HasPendingDelete returns true when a delete has been accepted for the path
// but the COS object has not been confirmed gone yet.
func (sm *StagingManager) HasPendingDelete(path string) bool {
	if sm == nil {
		return false
	}
	sm.tombstoneMu.RLock()
	defer sm.tombstoneMu.RUnlock()
	_, exists := sm.tombstones[path]
	return exists
}

// PendingDeleteCount returns the number of unresolved delete tombstones.
func (sm *StagingManager) PendingDeleteCount() int {
	if sm == nil {
		return 0
	}
	sm.tombstoneMu.RLock()
	defer sm.tombstoneMu.RUnlock()
	return len(sm.tombstones)
}

// PendingDeletes returns copies of all unresolved delete tombstones.
func (sm *StagingManager) PendingDeletes() []TombstoneState {
	sm.tombstoneMu.RLock()
	defer sm.tombstoneMu.RUnlock()

	tombs := make([]TombstoneState, 0, len(sm.tombstones))
	for _, tomb := range sm.tombstones {
		tombs = append(tombs, *tomb)
	}
	return tombs
}

// ResolvePendingDelete removes the tombstone after the COS object is
// confirmed deleted.
func (sm *StagingManager) ResolvePendingDelete(path string) {
	sm.tombstoneMu.Lock()
	defer sm.tombstoneMu.Unlock()

	if _, exists := sm.tombstones[path]; !exists {
		return
	}
	if err := os.Remove(sm.tombstoneFilePath(path)); err != nil && !os.IsNotExist(err) {
		// Keep the in-memory entry so the sync worker retries; a stale
		// tombstone must never outlive its in-memory tracking.
		logging.Warn("Failed to remove delete tombstone file",
			zap.String("path", path),
			zap.Error(err))
		return
	}
	delete(sm.tombstones, path)
}

// CancelPendingDelete withdraws a pending delete because the path was
// recreated with new data. Returns true when a tombstone was removed.
func (sm *StagingManager) CancelPendingDelete(path string) bool {
	sm.tombstoneMu.Lock()
	defer sm.tombstoneMu.Unlock()

	if _, exists := sm.tombstones[path]; !exists {
		return false
	}
	if err := os.Remove(sm.tombstoneFilePath(path)); err != nil && !os.IsNotExist(err) {
		logging.Warn("Failed to remove canceled delete tombstone file",
			zap.String("path", path),
			zap.Error(err))
		return false
	}
	delete(sm.tombstones, path)

	logging.Info("Canceled pending delete after path recreation",
		zap.String("path", path))
	return true
}

// recoverTombstonesFromDisk loads persisted delete tombstones so accepted
// deletes survive a restart. Loaded tombstones supersede any staged data for
// the same path during recovery.
func (sm *StagingManager) recoverTombstonesFromDisk() int {
	entries, err := os.ReadDir(sm.tombstoneDir())
	if err != nil {
		if !os.IsNotExist(err) {
			logging.Warn("Failed to read tombstone directory",
				zap.String("dir", sm.tombstoneDir()),
				zap.Error(err))
		}
		return 0
	}

	sm.tombstoneMu.Lock()
	defer sm.tombstoneMu.Unlock()

	recovered := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tombstone.json") {
			continue
		}
		tombstonePath := filepath.Join(sm.tombstoneDir(), entry.Name())
		stateBytes, err := os.ReadFile(tombstonePath)
		if err != nil {
			logging.Warn("Failed to read delete tombstone",
				zap.String("tombstone_path", tombstonePath),
				zap.Error(err))
			continue
		}
		var tomb TombstoneState
		if err := json.Unmarshal(stateBytes, &tomb); err != nil || tomb.Path == "" {
			logging.Warn("Removing invalid delete tombstone",
				zap.String("tombstone_path", tombstonePath),
				zap.Error(err))
			_ = os.Remove(tombstonePath)
			continue
		}
		if tomb.ObjectKey == "" {
			tomb.ObjectKey = objectKeyFromPath(tomb.Path)
		}
		sm.tombstones[tomb.Path] = &tomb
		recovered++
	}

	if recovered > 0 {
		logging.Info("Recovered pending delete tombstones",
			zap.Int("count", recovered))
	}
	return recovered
}

// Made with Bob
