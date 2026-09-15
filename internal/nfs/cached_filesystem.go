package nfs

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/oborges/bluestone/internal/logging"
	gonfs "github.com/willscott/go-nfs"
)

// CachedFilesystem wraps a billy.Filesystem with directory listing cache
// This works around the go-nfs library's failure to use CachingHandler
type CachedFilesystem struct {
	billy.Filesystem
	cache    sync.Map // map[string]*cachedDir
	logger   *logging.KVLogger
	cacheTTL time.Duration
}

type cachedDir struct {
	entries   []os.FileInfo
	timestamp time.Time
	mu        sync.RWMutex
}

// NewCachedFilesystem creates a filesystem with directory caching
func NewCachedFilesystem(fs billy.Filesystem, logger *logging.KVLogger, cacheTTL time.Duration) *CachedFilesystem {
	return &CachedFilesystem{
		Filesystem: fs,
		logger:     logger,
		cacheTTL:   cacheTTL,
	}
}

// ReadDir implements cached directory reading
func (cfs *CachedFilesystem) ReadDir(path string) ([]os.FileInfo, error) {
	// Check cache first
	if cached, ok := cfs.cache.Load(path); ok {
		dir := cached.(*cachedDir)
		dir.mu.RLock()
		age := time.Since(dir.timestamp)
		if age < cfs.cacheTTL {
			entries := dir.entries
			dir.mu.RUnlock()

			cfs.logger.Info("CACHE HIT: ReadDir from cache",
				"path", path,
				"entries", len(entries),
				"age_ms", age.Milliseconds())

			return entries, nil
		}
		dir.mu.RUnlock()
	}

	// Cache miss - read from underlying filesystem
	cfs.logger.Info("CACHE MISS: Reading from filesystem",
		"path", path)

	entries, err := cfs.Filesystem.ReadDir(path)
	if err != nil {
		return nil, err
	}

	// Store in cache
	dir := &cachedDir{
		entries:   entries,
		timestamp: time.Now(),
	}
	cfs.cache.Store(path, dir)

	cfs.logger.Info("CACHE STORE: Cached directory listing",
		"path", path,
		"entries", len(entries))

	return entries, nil
}

// InvalidateCache clears the cache for a specific path
func (cfs *CachedFilesystem) InvalidateCache(path string) {
	cfs.cache.Delete(path)
	cfs.logger.Info("CACHE INVALIDATE: Cleared cache",
		"path", path)
}

// ClearCache clears all cached entries
func (cfs *CachedFilesystem) ClearCache() {
	cfs.cache = sync.Map{}
	cfs.logger.Info("CACHE CLEAR: Cleared all cache entries")
}

// FSStat forwards dynamic filesystem capacity data through the cache wrapper.
func (cfs *CachedFilesystem) FSStat(ctx context.Context, stat *gonfs.FSStat) error {
	return fsStatFrom(ctx, cfs.Filesystem, stat)
}

// Chmod changes the mode of the named file (implements billy.Change)
func (cfs *CachedFilesystem) Chmod(name string, mode os.FileMode) error {
	if c, ok := cfs.Filesystem.(billy.Change); ok {
		return c.Chmod(name, mode)
	}
	return nil
}

// Lchown changes the uid and gid of the named file (implements billy.Change)
func (cfs *CachedFilesystem) Lchown(name string, uid, gid int) error {
	if c, ok := cfs.Filesystem.(billy.Change); ok {
		return c.Lchown(name, uid, gid)
	}
	return nil
}

// Chown changes the uid and gid of the named file (implements billy.Change)
func (cfs *CachedFilesystem) Chown(name string, uid, gid int) error {
	if c, ok := cfs.Filesystem.(billy.Change); ok {
		return c.Chown(name, uid, gid)
	}
	return nil
}

// Chtimes changes the access and modification times (implements billy.Change)
func (cfs *CachedFilesystem) Chtimes(name string, atime time.Time, mtime time.Time) error {
	if c, ok := cfs.Filesystem.(billy.Change); ok {
		return c.Chtimes(name, atime, mtime)
	}
	return nil
}

// Made with Bob
