package types

import (
	"io"
	"os"
	"time"
)

// FileSystem defines the interface for filesystem operations
type FileSystem interface {
	// File operations
	Open(path string, flags int) (File, error)
	Create(path string, mode os.FileMode) (File, error)
	Remove(path string) error
	Rename(oldPath, newPath string) error

	// Directory operations
	Mkdir(path string, mode os.FileMode) error
	Rmdir(path string) error
	ReadDir(path string) ([]os.FileInfo, error)

	// Metadata operations
	Stat(path string) (os.FileInfo, error)
	Chmod(path string, mode os.FileMode) error
	Chown(path string, uid, gid int) error
	Chtimes(path string, atime, mtime time.Time) error
}

// File represents an open file
type File interface {
	io.Reader
	io.Writer
	io.Closer
	io.Seeker
	Stat() (os.FileInfo, error)
	Truncate(size int64) error
	Sync() error
}

// ObjectMetadata represents COS object metadata
type ObjectMetadata struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	ContentType  string
	Metadata     map[string]string
}

// POSIXAttributes represents POSIX file attributes stored in COS metadata
type POSIXAttributes struct {
	Mode  os.FileMode
	UID   int
	GID   int
	Atime time.Time
	Mtime time.Time
	Ctime time.Time
	// Btime is the file creation (birth) time; zero when none is stored.
	Btime time.Time
	// WindowsAttributes holds Windows file attribute flags (read-only,
	// hidden, system, archive) for protocols that expose them.
	WindowsAttributes uint32
	// Streams holds a file's named data streams (alternate data streams),
	// by name, for protocols that expose them. Nil when there are none.
	Streams map[string][]byte
}

// CloneStreams copies a set of named streams, so attribute copies do not
// share one map.
func CloneStreams(streams map[string][]byte) map[string][]byte {
	if len(streams) == 0 {
		return nil
	}
	out := make(map[string][]byte, len(streams))
	for name, data := range streams {
		out[name] = append([]byte(nil), data...)
	}
	return out
}

// CacheEntry represents a cached item
type CacheEntry struct {
	Key       string
	Value     interface{}
	Size      int64
	ExpiresAt time.Time
}

// MetricType represents different metric types
type MetricType string

const (
	MetricTypeCounter   MetricType = "counter"
	MetricTypeGauge     MetricType = "gauge"
	MetricTypeHistogram MetricType = "histogram"
)

// Made with Bob
