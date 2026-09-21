package staging

import (
	"os"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// hasNoStagedData reports whether the session holds no bytes of its own: it
// is not dirty, was never prefetched, and is empty.
func (ws *WriteSession) hasNoStagedData() bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return !ws.Dirty && !ws.Prefetched && ws.Size == 0
}

// ReleaseSessionDiscardingIfEmpty releases one handle's reference and removes
// the session when no handle still holds it and it carries no staged data,
// as after a failed prefetch. A kept empty session would answer Stat and
// ReadDir as a zero-byte file and hide the object's real content.
func (sm *StagingManager) ReleaseSessionDiscardingIfEmpty(path string) {
	sm.mu.Lock()
	session, exists := sm.sessions[path]
	if !exists {
		sm.mu.Unlock()
		return
	}
	session.DecrementRefCount()
	if session.GetRefCount() > 0 || sm.dirtyIndex.IsDirty(path) || !session.hasNoStagedData() {
		sm.mu.Unlock()
		return
	}
	delete(sm.sessions, path)
	sm.mu.Unlock()

	if err := session.Close(); err != nil {
		logging.Warn("Failed to close discarded empty session",
			zap.String("path", path), zap.Error(err))
	}
	for _, file := range []string{session.StagingPath, sm.pathMetadataPath(session.StagingPath)} {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			logging.Warn("Failed to remove discarded empty session file",
				zap.String("path", path), zap.String("file", file), zap.Error(err))
		}
	}
	logging.Debug("Discarded empty staging session", zap.String("path", path))
}

// holdsNoWrittenData reports whether every byte of the staged file is one no
// client wrote: the file is empty, or it was only created and grown by
// truncation, which fills it with zeros.
func (ws *WriteSession) holdsNoWrittenData() bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.Multipart != nil && ws.Multipart.Active {
		return false
	}
	return ws.Size == 0 || (!ws.written && !ws.Prefetched)
}

// DiscardUnwrittenSession drops the staged state of a path that holds no
// data of its own, as when a client creates a file and sets its length
// before a server-side copy fills it. Once dropped, the staged file can no
// longer sync over the object the copy writes.
//
// It returns what was discarded, nil when there was no session, so that
// RestoreUnwrittenSession can put it back if what replaces it fails. The
// caller must hold the path's sync claim
// (TryLockSync), so no upload of the staged state is in flight; the claim is
// kept. It returns false and changes nothing when the path is conflicted, has
// a session some handle still holds, or holds written bytes.
func (sm *StagingManager) DiscardUnwrittenSession(path string) (*DiscardedSession, bool) {
	if sm.dirtyIndex.IsConflicted(path) {
		return nil, false
	}

	// Holding the manager lock keeps a handle from taking the session
	// between the check and the removal.
	sm.mu.Lock()
	session, exists := sm.sessions[path]
	if !exists {
		sm.mu.Unlock()
		// A dirty entry without a session means staged bytes are on disk
		// that nothing has examined.
		return nil, !sm.dirtyIndex.IsDirty(path)
	}
	if session.GetRefCount() > 0 || !session.holdsNoWrittenData() {
		sm.mu.Unlock()
		return nil, false
	}
	delete(sm.sessions, path)
	sm.mu.Unlock()

	// Drop the entry but keep the caller's sync claim, which MarkClean
	// would release.
	sm.dirtyIndex.DropEntry(path)
	sm.updateSyncQueueMetrics()
	sm.updatePressureMetrics()

	if err := session.Close(); err != nil {
		logging.Warn("Failed to close discarded unwritten session",
			zap.String("path", path), zap.Error(err))
	}
	for _, file := range []string{session.StagingPath, sm.pathMetadataPath(session.StagingPath)} {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			logging.Warn("Failed to remove discarded unwritten session file",
				zap.String("path", path), zap.String("file", file), zap.Error(err))
		}
	}
	discarded := &DiscardedSession{Size: session.GetSize(), Attributes: session.Attributes()}
	logging.Debug("Discarded unwritten staging session", zap.String("path", path))
	return discarded, true
}

// DiscardedSession is what DiscardUnwrittenSession dropped: a staged file of
// Size zero bytes, and the attributes it would have synced with.
type DiscardedSession struct {
	Size       int64
	Attributes StagedAttributes
}

// RestoreUnwrittenSession stages path again as it was before
// DiscardUnwrittenSession, for when what was to replace it failed. A session
// some handle created since is newer than the discarded one and is kept.
func (sm *StagingManager) RestoreUnwrittenSession(path string, discarded *DiscardedSession) error {
	// The session is sized and given its attributes before any handle can
	// see it.
	sm.mu.Lock()
	if _, exists := sm.sessions[path]; exists {
		sm.mu.Unlock()
		return nil
	}
	session, err := NewWriteSession(sm, path, sm.stagingFilePath(path))
	if err != nil {
		sm.mu.Unlock()
		return err
	}
	// Growing the empty file only adds zeros, which take no staging space,
	// so no reservation is needed.
	if err := session.File.Truncate(discarded.Size); err != nil {
		sm.mu.Unlock()
		_ = session.Close()
		return err
	}
	session.Size = discarded.Size
	session.Dirty = true
	session.RefCount = 0
	session.restoreAttributes(discarded.Attributes)
	sm.sessions[path] = session
	sm.mu.Unlock()

	sm.MarkDirty(path, discarded.Size)
	session.persistAttributes()
	logging.Info("Restored unwritten staging session", zap.String("path", path), zap.Int64("size", discarded.Size))
	return nil
}
