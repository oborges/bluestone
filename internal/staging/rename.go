package staging

import (
	"fmt"
	"os"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// moveStagedFile moves staged bytes to their new name on disk. It is a
// variable so a test can take the bytes away just before the move, as a sync
// worker finishing at that moment does.
var moveStagedFile = os.Rename

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
//   - an in-flight destination upload is invalidated by the destination
//     entry's new generation, so it cannot mark the destination clean and
//     the moved bytes re-sync over any stale put.
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
			// Stale dirty entry without staged bytes: drop it and let the
			// caller fall back to the object-store rename.
			sm.dropEntryWithoutStagedBytes(oldPath)
		}
		return err
	}

	// From here until the destination's session and dirty entry are
	// re-keyed, the staging file under the destination's name may be the
	// moved one while everything else there still describes the file it
	// replaces. The cleanup after a sync of that file must keep off it.
	sm.beginRenameOnto(newPath)
	defer sm.endRenameOnto(newPath)

	// The rename recreates the destination, so a pending delete there no
	// longer applies. Cancel before the new dirty entry appears; otherwise
	// tombstone processing could discard the moved bytes.
	sm.CancelPendingDelete(newPath)

	// Durable intent first: destination sidecar describing the moved bytes.
	// What it replaces is kept, to put back if the move does not happen.
	newSidecar := sm.pathMetadataPath(newStaging)
	state := &PathMetadataState{
		Version:        pathMetadataVersion,
		OriginalPath:   newPath,
		ObjectKey:      objectKeyFromPath(newPath),
		StagedFilePath: newStaging,
		ConflictStatus: ConflictStatusNone,
		DirtySince:     time.Now(),
	}
	if meta := sm.dirtyIndex.GetMetadata(oldPath); meta != nil && !meta.DirtySince.IsZero() {
		state.DirtySince = meta.DirtySince
	}
	state.Attributes = sm.renamedAttributes(oldPath, oldStaging)
	// Bytes a client was told are on stable storage stay so under the new
	// name.
	if old, err := readPathMetadataState(sm.pathMetadataPath(oldStaging)); err == nil {
		state.Committed = old.Committed
	}

	sm.sidecarMu.Lock()
	// An upload of the destination that finishes from here on must not mark
	// it clean: that would remove the sidecar written below along with the
	// entry, and leave the moved bytes with nothing to recover them by.
	sm.dirtyIndex.Invalidate(newPath)
	replacedSidecar, _ := readPathMetadataState(newSidecar)
	err := writePathMetadataState(newSidecar, state)
	sm.sidecarMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to persist renamed staging metadata: %w", err)
	}

	// Atomically move the staged bytes, replacing any destination bytes. Open
	// descriptors (source session, in-flight uploads) follow the inode and
	// stay valid. The source session's lock is held across the move, so a
	// write that moves the session to a new staging file (while an upload
	// reads the current one) cannot land at the old path after the move.
	sm.mu.RLock()
	source := sm.sessions[oldPath]
	sm.mu.RUnlock()
	moveStagedData := func() error { return moveStagedFile(oldStaging, newStaging) }
	if source != nil {
		moveStagedData = func() error {
			return source.moveStagingFile(newStaging, func() error { return moveStagedFile(oldStaging, newStaging) })
		}
	}
	if err := moveStagedData(); err != nil {
		// Nothing moved: take back the intent written above.
		sm.restorePathMetadata(newPath, newSidecar, replacedSidecar)
		if os.IsNotExist(err) {
			// The staged bytes went between the check above and the move:
			// a sync worker uploaded the file and cleaned up after itself.
			// That is a source without staged bytes like any other, and the
			// error goes back unwrapped so the caller can tell: wrapped, it
			// read as a failure, and a directory rename gave up over a file
			// that was safely in the bucket.
			sm.dropEntryWithoutStagedBytes(oldPath)
			return err
		}
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

// dropEntryWithoutStagedBytes forgets the dirty entry and sidecar of a path
// whose staged bytes are gone, without touching sync claims.
func (sm *StagingManager) dropEntryWithoutStagedBytes(path string) {
	sm.sidecarMu.Lock()
	sm.dirtyIndex.DropEntry(path)
	err := sm.removePathMetadata(path)
	sm.sidecarMu.Unlock()
	if err != nil {
		logging.Warn("Failed to remove stale sidecar for dirty entry without staged bytes",
			zap.String("path", path),
			zap.Error(err))
	}
	sm.updateSyncQueueMetrics()
}

// restorePathMetadata puts the sidecar of path back to what it was before a
// rename wrote its intent there: the state it replaced, or nothing. A path
// that is dirty is never left with nothing: if what it had could not be read
// before the rename replaced it, or cannot be written back, its sidecar is
// written again from what is in memory.
func (sm *StagingManager) restorePathMetadata(path, metadataPath string, replaced *PathMetadataState) {
	// Look the session up before taking the sidecar lock: the lookup takes
	// mu, which must be acquired first.
	session := sm.sessionAt(path)

	sm.sidecarMu.Lock()
	defer sm.sidecarMu.Unlock()

	var err error
	if replaced != nil {
		err = writePathMetadataState(metadataPath, replaced)
	} else if err = os.Remove(metadataPath); os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		logging.Warn("Failed to take back the staging metadata of a rename that did not happen",
			zap.String("metadata_path", metadataPath),
			zap.Error(err))
	}
	if (replaced == nil || err != nil) && sm.dirtyIndex.IsDirty(path) {
		if _, err := sm.recordDirtyPathLocked(path, sessionAttributesIfSet(session)); err != nil {
			// The next write of the file tries again, and fails until it
			// has a sidecar.
			sm.dirtyIndex.MarkUnrecorded(path)
			logging.Error("Dirty staged file is left without its sidecar after a rename that did not happen",
				zap.String("path", path),
				zap.String("metadata_path", metadataPath),
				zap.Error(err))
		}
	}
	if session != nil {
		session.metadataChanged()
	}
}

// Made with Bob
