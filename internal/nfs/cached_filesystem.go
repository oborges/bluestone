package nfs

import (
	"context"
	"os"
	pathpkg "path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/oborges/bluestone/internal/logging"
	gonfs "github.com/willscott/go-nfs"
)

// CachedFilesystem wraps a billy.Filesystem with directory listing cache
// This works around the go-nfs library's failure to use CachingHandler.
//
// Every change made through it drops the listings it affects: a listing
// served after a create but taken before it hides the new file, and rm -rf
// then leaves that file behind and fails to remove the directory.
type CachedFilesystem struct {
	billy.Filesystem
	cache    sync.Map // map[string]*cachedDir
	logger   *logging.KVLogger
	cacheTTL time.Duration
	// generation counts invalidations, so a listing read while a change was
	// made is not cached.
	generation atomic.Uint64
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
	key := cacheKey(path)

	// Check cache first
	if cached, ok := cfs.cache.Load(key); ok {
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

	generation := cfs.generation.Load()
	entries, err := cfs.Filesystem.ReadDir(path)
	if err != nil {
		return nil, err
	}
	if cfs.generation.Load() != generation {
		// Something changed while listing; the listing may predate it.
		return entries, nil
	}

	// Store in cache
	dir := &cachedDir{
		entries:   entries,
		timestamp: time.Now(),
	}
	cfs.cache.Store(key, dir)

	cfs.logger.Info("CACHE STORE: Cached directory listing",
		"path", path,
		"entries", len(entries))

	return entries, nil
}

// InvalidateCache clears the cache for a specific path
func (cfs *CachedFilesystem) InvalidateCache(path string) {
	cfs.generation.Add(1)
	cfs.cache.Delete(cacheKey(path))
	cfs.logger.Info("CACHE INVALIDATE: Cleared cache",
		"path", path)
}

// ClearCache clears all cached entries
func (cfs *CachedFilesystem) ClearCache() {
	cfs.generation.Add(1)
	cfs.cache.Range(func(key, _ any) bool {
		cfs.cache.Delete(key)
		return true
	})
	cfs.logger.Info("CACHE CLEAR: Cleared all cache entries")
}

// cacheKey normalizes a path to the form listings are cached under, so
// "a/b", "/a/b" and "a/b/" share an entry and the root is "".
func cacheKey(path string) string {
	return strings.Trim(pathpkg.Clean("/"+path), "/")
}

// changed drops the listing of the directory holding path, whose entry (name
// or attributes) changed.
func (cfs *CachedFilesystem) changed(path string) {
	cfs.generation.Add(1)
	key := cacheKey(path)
	if key == "" {
		return
	}
	cfs.cache.Delete(cacheKey(pathpkg.Dir(key)))
}

// changedTree drops the listings of path's parent, path itself and every
// directory below it, for a renamed directory.
func (cfs *CachedFilesystem) changedTree(path string) {
	cfs.changed(path)
	key := cacheKey(path)
	prefix := key + "/"
	cfs.cache.Range(func(k, _ any) bool {
		if name := k.(string); key == "" || name == key || strings.HasPrefix(name, prefix) {
			cfs.cache.Delete(k)
		}
		return true
	})
}

// changedAncestors drops the listings along path, for a MkdirAll that may
// have created any of its components.
func (cfs *CachedFilesystem) changedAncestors(path string) {
	for key := cacheKey(path); key != ""; key = cacheKey(pathpkg.Dir(key)) {
		cfs.changed(key)
	}
}

// Create creates a file, dropping its directory's listing.
func (cfs *CachedFilesystem) Create(filename string) (billy.File, error) {
	defer cfs.changed(filename)
	return cfs.Filesystem.Create(filename)
}

// OpenFile opens a file; one that may be created or truncated changes its
// directory's listing.
func (cfs *CachedFilesystem) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&(os.O_CREATE|os.O_TRUNC) != 0 {
		defer cfs.changed(filename)
	}
	return cfs.Filesystem.OpenFile(filename, flag, perm)
}

// TempFile creates a temporary file, dropping its directory's listing.
func (cfs *CachedFilesystem) TempFile(dir, prefix string) (billy.File, error) {
	defer cfs.changed(pathpkg.Join(dir, prefix))
	return cfs.Filesystem.TempFile(dir, prefix)
}

// Rename moves a file or directory, dropping the listings of both names.
func (cfs *CachedFilesystem) Rename(oldpath, newpath string) error {
	defer cfs.changedTree(oldpath)
	defer cfs.changedTree(newpath)
	return cfs.Filesystem.Rename(oldpath, newpath)
}

// Remove removes a file or empty directory, dropping the listing it was in
// and, for a directory, its own; an empty directory has no subdirectory
// listings left to drop.
func (cfs *CachedFilesystem) Remove(filename string) error {
	defer func() {
		cfs.changed(filename)
		cfs.cache.Delete(cacheKey(filename))
	}()
	return cfs.Filesystem.Remove(filename)
}

// MkdirAll creates a directory and any missing parents, dropping the
// listings they appear in.
func (cfs *CachedFilesystem) MkdirAll(filename string, perm os.FileMode) error {
	defer cfs.changedAncestors(filename)
	return cfs.Filesystem.MkdirAll(filename, perm)
}

// Symlink creates a symbolic link, dropping its directory's listing.
func (cfs *CachedFilesystem) Symlink(target, link string) error {
	defer cfs.changed(link)
	return cfs.Filesystem.Symlink(target, link)
}

// FSStat forwards dynamic filesystem capacity data through the cache wrapper.
func (cfs *CachedFilesystem) FSStat(ctx context.Context, stat *gonfs.FSStat) error {
	return fsStatFrom(ctx, cfs.Filesystem, stat)
}

// Chmod changes the mode of the named file (implements billy.Change)
func (cfs *CachedFilesystem) Chmod(name string, mode os.FileMode) error {
	if c, ok := cfs.Filesystem.(billy.Change); ok {
		defer cfs.changed(name)
		return c.Chmod(name, mode)
	}
	return nil
}

// Lchown changes the uid and gid of the named file (implements billy.Change)
func (cfs *CachedFilesystem) Lchown(name string, uid, gid int) error {
	if c, ok := cfs.Filesystem.(billy.Change); ok {
		defer cfs.changed(name)
		return c.Lchown(name, uid, gid)
	}
	return nil
}

// Chown changes the uid and gid of the named file (implements billy.Change)
func (cfs *CachedFilesystem) Chown(name string, uid, gid int) error {
	if c, ok := cfs.Filesystem.(billy.Change); ok {
		defer cfs.changed(name)
		return c.Chown(name, uid, gid)
	}
	return nil
}

// Chtimes changes the access and modification times (implements billy.Change)
func (cfs *CachedFilesystem) Chtimes(name string, atime time.Time, mtime time.Time) error {
	if c, ok := cfs.Filesystem.(billy.Change); ok {
		defer cfs.changed(name)
		return c.Chtimes(name, atime, mtime)
	}
	return nil
}

// Made with Bob
