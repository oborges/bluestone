package cache

import (
	"os"
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/pkg/types"
	"go.uber.org/zap"
)

// MetadataCache caches file and directory metadata
type MetadataCache struct {
	cache   *LRUCache
	enabled bool
	mu      sync.RWMutex
}

// MetadataEntry represents cached metadata
type MetadataEntry struct {
	FileInfo     os.FileInfo
	Attributes   *types.POSIXAttributes
	IsDir        bool
	Children     []string      // DEPRECATED: Use ChildEntries instead
	ChildEntries []os.FileInfo // Full FileInfo for directory listings (O(1) cache hit)
	CachedAt     time.Time
	IsImplicit   bool // True if directory has no marker object (needs validation)
	// Negative marks a cached "does not exist" answer, valid only for
	// NegativeTTL after CachedAt. Prevents lookup/create storms from paying
	// repeated object-store probes for missing paths.
	Negative bool
}

// NegativeTTL bounds how long a cached "does not exist" answer is trusted.
// Short by design: it only needs to cover the lookup-then-create window of a
// single client operation.
const NegativeTTL = 5 * time.Second

// SetNegative caches a short-lived "does not exist" answer for path.
func (c *MetadataCache) SetNegative(path string) {
	c.Set(path, &MetadataEntry{Negative: true})
}

// GetNegative reports whether a fresh negative entry exists for path.
func (c *MetadataCache) GetNegative(path string) bool {
	entry, ok := c.Get(path)
	return ok && entry != nil && entry.Negative && time.Since(entry.CachedAt) < NegativeTTL
}

// NewMetadataCache creates a new metadata cache
func NewMetadataCache(cfg *config.MetadataCacheConfig) *MetadataCache {
	if cfg == nil || !cfg.Enabled {
		logging.Info("Metadata cache disabled")
		return &MetadataCache{enabled: false}
	}

	cache := NewLRUCache(cfg.MaxEntries, cfg.GetTTL())

	// Set eviction callback for logging
	cache.SetEvictCallback(func(key string, value interface{}) {
		logging.Debug("Metadata cache entry evicted", zap.String("key", key))
	})

	logging.Info("Metadata cache initialized",
		zap.Int("maxEntries", cfg.MaxEntries),
		zap.Duration("ttl", cfg.GetTTL()),
	)

	return &MetadataCache{
		cache:   cache,
		enabled: true,
	}
}

// Get retrieves metadata from cache
func (c *MetadataCache) Get(path string) (*MetadataEntry, bool) {
	if !c.enabled {
		return nil, false
	}

	value, ok := c.cache.Get(path)
	if !ok {
		return nil, false
	}

	entry, ok := value.(*MetadataEntry)
	if !ok {
		logging.Warn("Invalid metadata cache entry type", zap.String("path", path))
		c.cache.Delete(path)
		return nil, false
	}

	// Removed debug logging from hot path - was causing performance issues
	// with repeated directory listings (O(n) log calls per readdir)
	return entry, true
}

// GetStale returns a cached entry even after its TTL expired, for serving
// known-but-stale metadata while the object store is unreachable.
func (c *MetadataCache) GetStale(path string) (*MetadataEntry, bool) {
	if !c.enabled {
		return nil, false
	}

	value, ok := c.cache.GetStale(path)
	if !ok {
		return nil, false
	}
	entry, ok := value.(*MetadataEntry)
	if !ok {
		return nil, false
	}
	return entry, true
}

// Set stores metadata in cache
func (c *MetadataCache) Set(path string, entry *MetadataEntry) {
	if !c.enabled {
		return
	}

	entry.CachedAt = time.Now()
	c.cache.Set(path, entry)
	// Removed debug logging from hot path
}

// SetFileInfo stores file info in cache
func (c *MetadataCache) SetFileInfo(path string, info os.FileInfo, attrs *types.POSIXAttributes) {
	c.SetFileInfoWithFlags(path, info, attrs, false)
}

// SetFileInfoWithFlags stores file info in cache with additional flags
func (c *MetadataCache) SetFileInfoWithFlags(path string, info os.FileInfo, attrs *types.POSIXAttributes, isImplicit bool) {
	if !c.enabled {
		return
	}

	entry := &MetadataEntry{
		FileInfo:   info,
		Attributes: attrs,
		IsDir:      info.IsDir(),
		CachedAt:   time.Now(),
		IsImplicit: isImplicit,
	}
	c.cache.Set(path, entry)
	if isImplicit {
		logging.Debug("Implicit directory cached (will be validated on next access)", zap.String("path", path))
	} else {
		logging.Debug("File info cached", zap.String("path", path))
	}
}

// SetDirListing stores directory listing in cache (DEPRECATED: use SetDirEntries)
func (c *MetadataCache) SetDirListing(path string, children []string) {
	if !c.enabled {
		return
	}

	entry := &MetadataEntry{
		IsDir:    true,
		Children: children,
		CachedAt: time.Now(),
	}
	c.cache.Set(path, entry)
	// Removed debug logging from hot path
}

// SetDirEntries stores full directory entries in cache (O(1) retrieval)
func (c *MetadataCache) SetDirEntries(path string, entries []os.FileInfo) {
	if !c.enabled {
		return
	}

	entry := &MetadataEntry{
		IsDir:        true,
		ChildEntries: entries,
		CachedAt:     time.Now(),
	}
	c.cache.Set(path, entry)
	// Only log on initial cache, not on hot path
	if len(entries) > 50 {
		logging.Debug("Large directory cached",
			zap.String("path", path),
			zap.Int("entries", len(entries)),
		)
	}
}

// RemoveChildFromListing removes a child from a cached directory listing
func (c *MetadataCache) RemoveChildFromListing(dirPath string, childName string) bool {
	if !c.enabled {
		return false
	}

	entry, ok := c.Get(dirPath)
	if !ok || !entry.IsDir || entry.Children == nil {
		return false
	}

	// Find and remove the child
	newChildren := make([]string, 0, len(entry.Children)-1)
	found := false
	for _, child := range entry.Children {
		if child != childName {
			newChildren = append(newChildren, child)
		} else {
			found = true
		}
	}

	if found {
		// Update the cache with the new children list
		entry.Children = newChildren
		entry.CachedAt = time.Now()
		c.cache.Set(dirPath, entry)
		logging.Debug("Child removed from cached directory listing",
			zap.String("dir", dirPath),
			zap.String("child", childName),
			zap.Int("remaining", len(newChildren)))
		return true
	}

	return false
}

// Delete removes an entry from cache
func (c *MetadataCache) Delete(path string) {
	if !c.enabled {
		return
	}

	if c.cache.Delete(path) {
		logging.Debug("Metadata cache entry deleted", zap.String("path", path))
	}
}

// InvalidatePath invalidates cache for a specific path
func (c *MetadataCache) InvalidatePath(path string) {
	if !c.enabled {
		return
	}

	c.Delete(path)

	// Also invalidate parent directory listing
	if path != "/" && path != "" {
		parentPath := getParentPath(path)
		c.Delete(parentPath)
		logging.Debug("Parent directory cache invalidated", zap.String("parent", parentPath))
	}
}

// InvalidatePrefix invalidates all entries with the given prefix
func (c *MetadataCache) InvalidatePrefix(prefix string) int {
	if !c.enabled {
		return 0
	}

	count := c.cache.DeletePrefix(prefix)
	logging.Debug("Cache entries invalidated by prefix",
		zap.String("prefix", prefix),
		zap.Int("count", count),
	)
	return count
}

// InvalidateDirectory invalidates a directory and all its children
func (c *MetadataCache) InvalidateDirectory(dirPath string) int {
	if !c.enabled {
		return 0
	}

	originalPath := dirPath
	// Ensure path ends with /
	if dirPath != "/" && dirPath[len(dirPath)-1] != '/' {
		dirPath += "/"
	}

	count := c.InvalidatePrefix(dirPath)
	c.Delete(dirPath)
	c.Delete(originalPath)
	if originalPath != "/" && originalPath != "" {
		c.Delete(getParentPath(originalPath))
	}

	logging.Debug("Directory cache invalidated",
		zap.String("dir", originalPath),
		zap.Int("count", count),
	)
	return count
}

// Clear removes all entries from cache
func (c *MetadataCache) Clear() {
	if !c.enabled {
		return
	}

	c.cache.Clear()
	logging.Info("Metadata cache cleared")
}

// Stats returns cache statistics
func (c *MetadataCache) Stats() CacheStats {
	if !c.enabled {
		return CacheStats{}
	}

	return c.cache.Stats()
}

// CleanExpired removes expired entries
func (c *MetadataCache) CleanExpired() int {
	if !c.enabled {
		return 0
	}

	count := c.cache.CleanExpired()
	if count > 0 {
		logging.Debug("Expired metadata entries cleaned", zap.Int("count", count))
	}
	return count
}

// IsEnabled returns whether the cache is enabled
func (c *MetadataCache) IsEnabled() bool {
	return c.enabled
}

// getParentPath returns the parent directory path
func getParentPath(path string) string {
	if path == "/" || path == "" {
		return "/"
	}

	// Remove trailing slash if present
	if path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}

	// Find last slash
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}

	return "/"
}

// Made with Bob
