package staging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// UploadSnapshot is a read-only view of a session's staged bytes for one sync
// attempt. Its File stays exactly as it was when the snapshot was taken:
// while the snapshot is open, a change to the session moves the session to a
// new staging file instead of changing this one. Close releases it.
type UploadSnapshot struct {
	File      *os.File
	Size      int64
	LastWrite time.Time
	PartSize  int64

	session    *WriteSession
	generation uint64
	closed     bool
}

// OpenUploadSnapshot opens the staged bytes for an upload. The size and last
// write it reports describe the file it returns.
func (ws *WriteSession) OpenUploadSnapshot() (*UploadSnapshot, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.File == nil {
		return nil, fmt.Errorf("staging session for %s is closed", ws.Path)
	}
	if err := ws.File.Sync(); err != nil {
		return nil, fmt.Errorf("failed to sync: %w", err)
	}
	// A descriptor of its own, so closing the session does not close the
	// upload's file.
	file, err := os.Open(ws.StagingPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open staging file: %w", err)
	}

	partSize := int64(0)
	if ws.Multipart != nil {
		partSize = ws.Multipart.PartSize
	}
	ws.uploadReaders++
	return &UploadSnapshot{
		File:       file,
		Size:       ws.Size,
		LastWrite:  ws.LastWrite,
		PartSize:   partSize,
		session:    ws,
		generation: ws.fileGeneration,
	}, nil
}

// Close releases the snapshot and its file.
func (s *UploadSnapshot) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true

	ws := s.session
	ws.mu.Lock()
	// A snapshot of a staging file the session has since moved away from no
	// longer holds the current one.
	if ws.fileGeneration == s.generation && ws.uploadReaders > 0 {
		ws.uploadReaders--
	}
	ws.mu.Unlock()

	return s.File.Close()
}

// detachFromUploadsLocked makes the session's staging file safe to change
// while uploads are reading it. With no upload snapshot open it does nothing.
// Otherwise it copies the first keepBytes bytes into a new file, renames that
// over the staging path and continues the session there, leaving the file the
// uploads read untouched until they close it. Only bytes the change keeps are
// copied, so truncating to zero, as an O_TRUNC open does, copies nothing.
// Callers hold ws.mu.
func (ws *WriteSession) detachFromUploadsLocked(keepBytes int64) error {
	if ws.uploadReaders == 0 {
		return nil
	}

	started := time.Now()
	tmp, err := os.CreateTemp(filepath.Dir(ws.StagingPath), filepath.Base(ws.StagingPath)+".detach-*")
	if err != nil {
		return fmt.Errorf("failed to create staging file for write during upload: %w", err)
	}
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if keepBytes > 0 {
		if _, err := ws.File.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("failed to rewind staging file: %w", err)
		}
		if _, err := io.CopyN(tmp, ws.File, keepBytes); err != nil {
			return fmt.Errorf("failed to copy staging file for write during upload: %w", err)
		}
		// The copied bytes were durable in the old file; make them durable
		// here before the rename replaces it, or a crash could leave the
		// rename without the data.
		if err := tmp.Sync(); err != nil {
			return fmt.Errorf("failed to sync staging file for write during upload: %w", err)
		}
	}
	if err := os.Rename(tmp.Name(), ws.StagingPath); err != nil {
		return fmt.Errorf("failed to replace staging file for write during upload: %w", err)
	}
	removeTmp = false

	// The uploads opened descriptors of their own, so the old file stays
	// readable for them after this close and the rename's unlink.
	_ = ws.File.Close()
	ws.File = tmp

	readers := ws.uploadReaders
	ws.uploadReaders = 0
	ws.fileGeneration++

	logging.Info("Moved staging session to a new file: upload in flight",
		zap.String("path", ws.Path),
		zap.String("staging_path", ws.StagingPath),
		zap.Int("upload_readers", readers),
		zap.Int64("copied_bytes", keepBytes),
		zap.Duration("duration", time.Since(started)))
	return nil
}
