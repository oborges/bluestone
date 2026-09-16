package staging

import (
	"os"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/pkg/types"
	"go.uber.org/zap"
)

// StagedAttributes are the attributes a staged file syncs to COS with.
type StagedAttributes struct {
	Mode              os.FileMode `json:"mode"`
	UID               uint32      `json:"uid"`
	GID               uint32      `json:"gid"`
	Btime             time.Time   `json:"btime,omitempty"`
	WindowsAttributes uint32      `json:"windows_attributes,omitempty"`
	// Atime and Mtime are times a client set explicitly. They are zero when
	// no client has set them, and the staged file's own last write stands in.
	Atime time.Time `json:"atime,omitempty"`
	Mtime time.Time `json:"mtime,omitempty"`
}

// StagedAttributesFrom converts object attributes for staging.
func StagedAttributesFrom(attrs types.POSIXAttributes) StagedAttributes {
	return StagedAttributes{
		Mode:              attrs.Mode,
		UID:               uint32(attrs.UID),
		GID:               uint32(attrs.GID),
		Btime:             attrs.Btime,
		WindowsAttributes: attrs.WindowsAttributes,
		Atime:             attrs.Atime,
		Mtime:             attrs.Mtime,
	}
}

// POSIX returns the attributes in the form COS metadata is encoded from.
func (a StagedAttributes) POSIX() *types.POSIXAttributes {
	return &types.POSIXAttributes{
		Mode:              a.Mode,
		UID:               int(a.UID),
		GID:               int(a.GID),
		Btime:             a.Btime,
		WindowsAttributes: a.WindowsAttributes,
		Atime:             a.Atime,
		Mtime:             a.Mtime,
	}
}

// persistSessionAttributes records a session's attributes in its sidecar, so
// crash recovery uploads the file with them. A session without a sidecar has
// no staged state to recover and is skipped; MarkDirty writes the attributes
// when it creates one.
func (sm *StagingManager) persistSessionAttributes(ws *WriteSession) {
	attrs, stagingPath, set := ws.stagedAttributes()
	if !set {
		return
	}

	sm.sidecarMu.Lock()
	defer sm.sidecarMu.Unlock()

	metadataPath := sm.pathMetadataPath(stagingPath)
	state, err := readPathMetadataState(metadataPath)
	if err != nil {
		if !os.IsNotExist(err) {
			logging.Warn("Failed to read staging metadata for attribute update",
				zap.String("metadata_path", metadataPath), zap.Error(err))
		}
		return
	}
	state.Attributes = &attrs
	if err := writePathMetadataState(metadataPath, state); err != nil {
		logging.Warn("Failed to persist staged attributes",
			zap.String("metadata_path", metadataPath), zap.Error(err))
	}
}

// sessionAttributes returns the attributes of the session staging path, if
// they were seeded or set rather than new-session defaults.
func (sm *StagingManager) sessionAttributes(path string) *StagedAttributes {
	session, ok := sm.GetSession(path)
	if !ok {
		return nil
	}
	attrs, _, set := session.stagedAttributes()
	if !set {
		return nil
	}
	return &attrs
}

// renamedAttributes returns the attributes a renamed staged file keeps: the
// source session's when set, otherwise those persisted for the source.
func (sm *StagingManager) renamedAttributes(oldPath, oldStaging string) *StagedAttributes {
	if attrs := sm.sessionAttributes(oldPath); attrs != nil {
		return attrs
	}
	if state, err := readPathMetadataState(sm.pathMetadataPath(oldStaging)); err == nil {
		return state.Attributes
	}
	return nil
}
