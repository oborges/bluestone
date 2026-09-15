package staging

import (
	"fmt"
	"os"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// RenameStagedPath applies POSIX write-back rename semantics to a dirty staged
// source file: the staged bytes and dirty bookkeeping move to the destination
// path, and a durable tombstone removes the source COS object once any
// in-flight upload finishes. Destination staged state (bytes, dirty entry,
// session, pending delete) is discarded, matching rename-over-replace.
//
// Ordering keeps every crash and concurrency window safe:
//   - the destination sidecar is persisted before the byte move, so a crash
//     in between leaves the source intact (rename simply did not happen);
//   - the source tombstone is persisted after the byte move, so the worst
//     crash window leaves both names visible, which matches the documented
//     non-atomic object-store rename semantics — never data loss;
//   - the syncing map is never mutated, so an in-flight source upload keeps
//     its claim and the tombstone (which must claim that lock) deletes the
//     old object strictly after the upload lands;
//   - an in-flight destination upload is invalidated by the re-keyed dirty
//     entry's newer LastModified, so the moved bytes re-sync over any stale
//     put.
//
// Returns an os.IsNotExist error when the source has no staged bytes; the
// caller should fall back to a plain object-store rename.
func (sm *StagingManager) RenameStagedPath(oldPath, newPath string) error {
	if sm.dirtyIndex.IsConflicted(oldPath) || sm.dirtyIndex.IsConflicted(newPath) {
		return fmt.Errorf("%w: %s -> %s", ErrPathConflicted, oldPath, newPath)
	}

	oldStaging := sm.stagingFilePath(oldPath)
	newStaging := sm.stagingFilePath(newPath)

	if _, err := os.Stat(oldStaging); err != nil {
		if os.IsNotExist(err) {
			// Stale dirty entry without staged bytes: drop it (without
			// touching sync claims) and let the caller fall back to the
			// object-store rename.
			sm.dirtyIndex.DropEntry(oldPath)
			if removeErr := sm.removePathMetadata(oldPath); removeErr != nil {
				logging.Warn("Failed to remove stale sidecar for dirty entry without staged bytes",
					zap.String("path", oldPath),
					zap.Error(removeErr))
			}
			sm.updateSyncQueueMetrics()
		}
		return err
	}

	// The rename recreates the destination, so a pending delete there no
	// longer applies. Cancel before the new dirty entry appears; otherwise
	// tombstone processing could discard the moved bytes.
	sm.CancelPendingDelete(newPath)

	// Durable intent first: destination sidecar describing the moved bytes.
	now := time.Now()
	state := &PathMetadataState{
		Version:              pathMetadataVersion,
		OriginalPath:         newPath,
		ObjectKey:            objectKeyFromPath(newPath),
		StagedFilePath:       newStaging,
		ConflictStatus:       ConflictStatusNone,
		DirtySince:           now,
		LastModified:         now,
		LocalDirtyGeneration: 1,
	}
	if meta := sm.dirtyIndex.GetMetadata(oldPath); meta != nil {
		state.Size = meta.Size
		if !meta.DirtySince.IsZero() {
			state.DirtySince = meta.DirtySince
		}
		state.LocalDirtyGeneration = meta.LocalDirtyGeneration + 1
	}
	if err := writePathMetadataState(sm.pathMetadataPath(newStaging), state); err != nil {
		return fmt.Errorf("failed to persist renamed staging metadata: %w", err)
	}

	// Atomically move the staged bytes, replacing any destination bytes. Open
	// descriptors (source session, in-flight uploads) follow the inode and
	// stay valid.
	if err := os.Rename(oldStaging, newStaging); err != nil {
		return fmt.Errorf("failed to move staged data: %w", err)
	}

	// Tombstone the source so its COS object cannot resurrect the old name.
	if _, err := sm.addTombstone(oldPath); err != nil {
		// The namespace move already happened; without the tombstone the old
		// object can linger, which matches the documented non-atomic rename
		// semantics. Never fail the rename here.
		logging.Error("Failed to persist rename tombstone; source object may linger in COS",
			zap.String("old_path", oldPath),
			zap.String("new_path", newPath),
			zap.Error(err))
	}

	// The source sidecar now describes bytes that moved away.
	if err := os.Remove(sm.pathMetadataPath(oldStaging)); err != nil && !os.IsNotExist(err) {
		logging.Warn("Failed to remove source sidecar after rename",
			zap.String("path", oldPath),
			zap.Error(err))
	}

	// Re-key in-memory state. The source session keeps its open descriptor,
	// which now addresses the moved file.
	sm.mu.Lock()
	if dest, exists := sm.sessions[newPath]; exists {
		delete(sm.sessions, newPath)
		if dest.GetRefCount() > 0 {
			logging.Warn("Discarding open destination session replaced by rename",
				zap.String("path", newPath),
				zap.Int32("ref_count", dest.GetRefCount()))
		}
		// The destination's staged bytes were already replaced on disk; close
		// the orphaned descriptor without deleting the moved file.
		dest.Dirty = false
		if err := dest.Close(); err != nil {
			logging.Warn("Failed to close destination session replaced by rename",
				zap.String("path", newPath),
				zap.Error(err))
		}
	}
	if session, exists := sm.sessions[oldPath]; exists {
		delete(sm.sessions, oldPath)
		session.Rekey(newPath, newStaging)
		sm.sessions[newPath] = session
	}
	sm.mu.Unlock()

	// Move the dirty entry last; sync claims are left untouched so in-flight
	// uploads of either path keep their ordering guarantees.
	sm.dirtyIndex.Rekey(oldPath, newPath, newStaging)

	sm.updateSyncQueueMetrics()
	sm.updatePressureMetrics()

	logging.Info("Renamed dirty staged path",
		zap.String("old_path", oldPath),
		zap.String("new_path", newPath),
		zap.String("staging_path", newStaging))

	return nil
}

// Made with Bob
