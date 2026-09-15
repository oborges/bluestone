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
