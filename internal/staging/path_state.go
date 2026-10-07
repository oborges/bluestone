package staging

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
//
// It holds what identifies the staged file and the attributes it syncs with,
// and is written only when one of those changes. What changes with every
// write is not in it: the size and the time of the last write are read back
// from the data file, and the dirty generation lives only in memory (see
// DirtyFileMetadata.LocalDirtyGeneration).
type PathMetadataState struct {
	Version              int       `json:"version"`
	OriginalPath         string    `json:"original_path"`
	ObjectKey            string    `json:"object_key"`
	ObservedETag         string    `json:"observed_etag,omitempty"`
	ObservedSize         int64     `json:"observed_size,omitempty"`
	ObservedLastModified time.Time `json:"observed_last_modified,omitempty"`
	StagedFilePath       string    `json:"staged_file_path"`
	ConflictStatus       string    `json:"conflict_status"`
	DirtySince           time.Time `json:"dirty_since,omitempty"`
	// Attributes are the POSIX attributes the staged file syncs with, kept
	// here so crash recovery uploads it with them.
	Attributes *StagedAttributes `json:"attributes,omitempty"`
	// Committed records that a client was told this staged file is on
	// stable storage, which put this sidecar on disk. From then on the
	// sidecar is only replaced by one already on disk, so a power loss
	// cannot leave the file without the metadata recovery needs to upload
	// it. A sidecar that takes the place of a committed one, as a rename
	// over the file writes, is committed for the same reason.
	Committed bool `json:"committed,omitempty"`
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

	// A committed sidecar is on disk, and may only be replaced by one that
	// is on disk before it takes its place. That holds whoever writes the
	// replacement and whatever it was built from, so it is settled here.
	if !state.Committed {
		if replaced, err := readPathMetadataState(metadataPath); err == nil && replaced.Committed {
			state.Committed = true
		}
	}

	if err := os.MkdirAll(filepath.Dir(metadataPath), 0700); err != nil {
		return err
	}
	metadataBytes, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := metadataPath + ".tmp"
	if err := writeSidecarFile(tmpPath, metadataBytes, state.Committed); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, metadataPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// writeSidecarFile writes a sidecar's bytes to path and, when durable,
// returns only once they are on disk.
func writeSidecarFile(path string, data []byte, durable bool) error {
	if !durable {
		return os.WriteFile(path, data, 0600)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// syncDir flushes a directory's entries to disk, so that files created in it
// or renamed into place are still there after a power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	err = d.Sync()
	// A platform that cannot sync a directory refuses with one of these
	// (AIX wants a descriptor open for writing, which a directory cannot
	// be); there is nothing more to do on it.
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}

// dirtyMetadataFromPathState builds the dirty entry of a staged file found on
// disk: its identity from the sidecar, its size and last write from the data
// file.
func dirtyMetadataFromPathState(state *PathMetadataState, size int64, modTime time.Time) DirtyFileMetadata {
	dirtySince := state.DirtySince
	if dirtySince.IsZero() {
		dirtySince = modTime
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
		StagedPath:           state.StagedFilePath,
		ConflictStatus:       state.ConflictStatus,
		Size:                 size,
		DirtySince:           dirtySince,
		LastModified:         modTime,
	}
}
