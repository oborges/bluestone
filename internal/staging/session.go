package staging

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// WriteSession represents an active write session for a file path
// Sessions are path-scoped and survive file handle open/close cycles
type WriteSession struct {
	Manager     *StagingManager
	Path        string
	StagingPath string
	File        *os.File
	Size        int64
	Dirty       bool
	Prefetched  bool
	RefCount    int32
	LastWrite   time.Time
	LastAccess  time.Time
	CreatedAt   time.Time
	Multipart   *S3MultipartState
	Mode        os.FileMode
	UID         uint32
	GID         uint32
	mu          sync.Mutex
}

// NewWriteSession creates a new write session
func NewWriteSession(manager *StagingManager, path string, stagingPath string) (*WriteSession, error) {
	// Open or create staging file
	file, err := os.OpenFile(stagingPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open staging file: %w", err)
	}

	// Get current size
	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to stat staging file: %w", err)
	}

	if manager != nil {
		if err := manager.EnsurePathMetadata(path, stagingPath, stat.Size()); err != nil {
			fmt.Printf("Warning: Failed to persist staging metadata for %s: %v\n", path, err)
		}
	}

	now := time.Now()
	return &WriteSession{
		Manager:     manager,
		Path:        path,
		StagingPath: stagingPath,
		File:        file,
		Size:        stat.Size(),
		Dirty:       false,
		Prefetched:  false,
		RefCount:    1,
		LastWrite:   now,
		LastAccess:  now,
		CreatedAt:   now,
		Multipart:   NewS3MultipartState(20), // Default 20MB part chunks
		Mode:        0600,
		UID:         1000,
		GID:         1000,
	}, nil
}

// UpdateAttributes seamlessly mutates POSIX tracking bounds natively
func (ws *WriteSession) UpdateAttributes(mode os.FileMode, uid uint32, gid uint32) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if mode != 0 {
		ws.Mode = mode
	}
	// Avoid wiping 0 explicitly unless bounded but standard NFS propagates variables accurately
	ws.UID = uid
	ws.GID = gid
}

// Rekey points the session at a renamed path. The open descriptor still
// addresses the moved staging file, so only the naming changes.
func (ws *WriteSession) Rekey(newPath, newStagingPath string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.Path = newPath
	ws.StagingPath = newStagingPath
}

// Write writes data to the staging file at the specified offset
func (ws *WriteSession) Write(data []byte, offset int64) (int, error) {
	currentSize := ws.GetSize()
	growthBytes := offset + int64(len(data)) - currentSize
	releaseReservation := func() {}
	if ws.Manager != nil {
		var err error
		releaseReservation, err = ws.Manager.ReserveWrite(ws.Path, int64(len(data)), growthBytes)
		if err != nil {
			return 0, err
		}
	}
	defer releaseReservation()

	ws.mu.Lock()
	defer ws.mu.Unlock()

	// Seek to offset
	if _, err := ws.File.Seek(offset, 0); err != nil {
		return 0, fmt.Errorf("failed to seek: %w", err)
	}

	// Write data
	n, err := ws.File.Write(data)
	if err != nil {
		return n, fmt.Errorf("failed to write: %w", err)
	}

	// Update size
	newSize := offset + int64(n)
	if newSize > ws.Size {
		ws.Size = newSize
	}

	ws.Multipart.MarkModified(offset)

	now := time.Now()
	ws.Dirty = true
	ws.LastWrite = now
	ws.LastAccess = now

	return n, nil
}

// Read reads data from the staging file at the specified offset
func (ws *WriteSession) Read(buffer []byte, offset int64) (int, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	// Check bounds
	if offset >= ws.Size {
		return 0, io.EOF // Return EOF when at or past end of file
	}

	// Seek to offset
	if _, err := ws.File.Seek(offset, 0); err != nil {
		return 0, fmt.Errorf("failed to seek: %w", err)
	}

	// Read data
	n, err := ws.File.Read(buffer)
	if err != nil && err != io.EOF {
		return 0, fmt.Errorf("failed to read: %w", err)
	}

	ws.LastAccess = time.Now()

	// If we read some data but hit EOF, return the data with nil error
	// The next read will return 0, io.EOF
	if err == io.EOF && n > 0 {
		return n, nil
	}

	return n, err
}

// Sync flushes the staging file to disk
func (ws *WriteSession) Sync() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if err := ws.File.Sync(); err != nil {
		return fmt.Errorf("failed to sync: %w", err)
	}

	return nil
}

// Snapshot returns stable session metadata for a sync attempt.
func (ws *WriteSession) Snapshot() (stagingPath string, size int64, mode os.FileMode, uid uint32, gid uint32, refCount int32, lastWrite time.Time, multipartPartSize int64) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	partSize := int64(0)
	if ws.Multipart != nil {
		partSize = ws.Multipart.PartSize
	}
	return ws.StagingPath, ws.Size, ws.Mode, ws.UID, ws.GID, ws.RefCount, ws.LastWrite, partSize
}

// Close closes the staging file
func (ws *WriteSession) Close() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.File != nil {
		if err := ws.File.Close(); err != nil {
			return fmt.Errorf("failed to close: %w", err)
		}
		ws.File = nil
	}

	return nil
}

// GetSize returns the current file size
func (ws *WriteSession) GetSize() int64 {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	return ws.Size
}

// IncrementRefCount increments the reference count
func (ws *WriteSession) IncrementRefCount() {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	ws.RefCount++
	ws.LastAccess = time.Now()
}

// DecrementRefCount decrements the reference count
func (ws *WriteSession) DecrementRefCount() {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.RefCount > 0 {
		ws.RefCount--
	}
	ws.LastAccess = time.Now()
}

// GetRefCount returns the current reference count
func (ws *WriteSession) GetRefCount() int32 {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	return ws.RefCount
}

// Truncate truncates the staging file to the specified size
func (ws *WriteSession) Truncate(size int64) error {
	currentSize := ws.GetSize()
	releaseReservation := func() {}
	if ws.Manager != nil && size > currentSize {
		var err error
		releaseReservation, err = ws.Manager.ReserveWrite(ws.Path, size-currentSize, size-currentSize)
		if err != nil {
			return err
		}
	}
	defer releaseReservation()

	ws.mu.Lock()
	defer ws.mu.Unlock()

	// Truncate the file
	if err := ws.File.Truncate(size); err != nil {
		return fmt.Errorf("failed to truncate: %w", err)
	}

	// Bytes from the lower of the old and new end onward changed. Record it
	// like a write, so a progressive multipart upload that already covered
	// those bytes is not completed as if the file had only grown.
	firstChanged := size
	if ws.Size < firstChanged {
		firstChanged = ws.Size
	}
	ws.Multipart.MarkModified(firstChanged)

	// Update size and mark as dirty
	ws.Size = size
	ws.Dirty = true
	ws.LastWrite = time.Now()
	ws.LastAccess = time.Now()

	return nil
}

// Prefetch runs the provided fetch function exactly once.
func (ws *WriteSession) Prefetch(fetcher func() error) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.Prefetched {
		return nil
	}

	if err := fetcher(); err != nil {
		// Drop any partial download so the staged file never holds bytes the
		// session does not account for. The next open retries the fetch.
		if ws.File != nil {
			_ = ws.File.Truncate(ws.Size)
		}
		return err
	}

	if stat, err := ws.File.Stat(); err == nil {
		ws.Size = stat.Size()
	}

	ws.Prefetched = true
	return nil
}

// Made with Bob
