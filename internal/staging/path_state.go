package staging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	pathMetadataVersion = 1

	ConflictStatusNone       = "none"
	ConflictStatusConflicted = "conflicted"
)

// PathMetadataState is the durable per-path write-back state kept next to a
// staged data file. It is intentionally small and local so restart recovery does
// not depend on any external database.
type PathMetadataState struct {
	Version              int       `json:"version"`
	OriginalPath         string    `json:"original_path"`
	ObjectKey            string    `json:"object_key"`
	ObservedETag         string    `json:"observed_etag,omitempty"`
	ObservedSize         int64     `json:"observed_size,omitempty"`
	ObservedLastModified time.Time `json:"observed_last_modified,omitempty"`
	LocalDirtyGeneration int64     `json:"local_dirty_generation"`
	StagedFilePath       string    `json:"staged_file_path"`
	ConflictStatus       string    `json:"conflict_status"`
	Size                 int64     `json:"size"`
	DirtySince           time.Time `json:"dirty_since,omitempty"`
	LastModified         time.Time `json:"last_modified,omitempty"`
	// Attributes are the POSIX attributes the staged file syncs with, kept
	// here so crash recovery uploads it with them.
	Attributes *StagedAttributes `json:"attributes,omitempty"`
}

func objectKeyFromPath(path string) string {
	return strings.TrimPrefix(path, "/")
}

func readPathMetadataState(metadataPath string) (*PathMetadataState, error) {
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, err
	}

	var state PathMetadataState
	if err := json.Unmarshal(metadataBytes, &state); err != nil {
		return nil, err
	}
	if state.OriginalPath == "" {
		return nil, fmt.Errorf("metadata missing original_path")
	}
	if state.Version == 0 {
		state.Version = pathMetadataVersion
	}
	if state.ObjectKey == "" {
		state.ObjectKey = objectKeyFromPath(state.OriginalPath)
	}
	if state.ConflictStatus == "" {
		state.ConflictStatus = ConflictStatusNone
	}
	return &state, nil
}

func writePathMetadataState(metadataPath string, state *PathMetadataState) error {
	if state == nil {
		return fmt.Errorf("metadata state is nil")
	}
	if state.Version == 0 {
		state.Version = pathMetadataVersion
	}
	if state.ObjectKey == "" {
		state.ObjectKey = objectKeyFromPath(state.OriginalPath)
	}
	if state.ConflictStatus == "" {
		state.ConflictStatus = ConflictStatusNone
	}

	if err := os.MkdirAll(filepath.Dir(metadataPath), 0700); err != nil {
		return err
	}
	metadataBytes, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := metadataPath + ".tmp"
	if err := os.WriteFile(tmpPath, metadataBytes, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, metadataPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func dirtyMetadataFromPathState(state *PathMetadataState, size int64, modTime time.Time) DirtyFileMetadata {
	dirtySince := state.DirtySince
	if dirtySince.IsZero() {
		dirtySince = modTime
	}
	lastModified := state.LastModified
	if lastModified.IsZero() {
		lastModified = modTime
	}
	generation := state.LocalDirtyGeneration
	if generation <= 0 {
		generation = 1
	}
	objectKey := state.ObjectKey
	if objectKey == "" {
		objectKey = objectKeyFromPath(state.OriginalPath)
	}
	return DirtyFileMetadata{
		Path:                 state.OriginalPath,
		ObjectKey:            objectKey,
		ObservedETag:         state.ObservedETag,
		ObservedSize:         state.ObservedSize,
		ObservedLastModified: state.ObservedLastModified,
		LocalDirtyGeneration: generation,
		StagedPath:           state.StagedFilePath,
		ConflictStatus:       state.ConflictStatus,
		Size:                 size,
		DirtySince:           dirtySince,
		LastModified:         lastModified,
	}
}
