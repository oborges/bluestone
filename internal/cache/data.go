package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// DataCache provides disk-based caching for file data
type DataCache struct {
	basePath    string
	maxSize     int64
	currentSize int64
	chunkSize   int64
	enabled     bool
	index       *LRUCache
	mu          sync.RWMutex
}

// CacheEntry represents a cached data entry
type CacheEntry struct {
	Key       string
	FilePath  string
	Size      int64
	CachedAt  time.Time
	AccessAt  time.Time
	ChunkSize int64
}

// NewDataCache creates a new data cache
func NewDataCache(cfg *config.DataCacheConfig) (*DataCache, error) {
	if cfg == nil || !cfg.Enabled {
		logging.Info("Data cache disabled")
		return &DataCache{enabled: false}, nil
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(cfg.Path, 0700); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	maxSize := int64(cfg.SizeGB) * 1024 * 1024 * 1024
	chunkSize := int64(cfg.ChunkSize) * 1024

	cache := &DataCache{
		basePath:  cfg.Path,
		maxSize:   maxSize,
		chunkSize: chunkSize,
		enabled:   true,
		index:     NewLRUCache(10000, 24*time.Hour), // Index with 24h TTL
	}

	// Set eviction callback to delete files
	cache.index.SetEvictCallback(func(key string, value interface{}) {
		if entry, ok := value.(*CacheEntry); ok {
			cache.deleteFile(entry.FilePath)
		}
	})

	// Calculate current cache size
	if err := cache.calculateSize(); err != nil {
		logging.Warn("Failed to calculate cache size", zap.Error(err))
	}

	logging.Info("Data cache initialized",
		zap.String("path", cfg.Path),
		zap.Int64("maxSize", maxSize),
		zap.Int64("chunkSize", chunkSize),
		zap.Int64("currentSize", cache.currentSize),
	)

	return cache, nil
}

// Read reads data from cache
func (c *DataCache) Read(key string, offset, length int64) ([]byte, error) {
	if !c.enabled {
		return nil, fmt.Errorf("cache disabled")
	}

	c.mu.RLock()
	value, ok := c.index.Get(key)
	c.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("cache miss")
	}

	entry, ok := value.(*CacheEntry)
	if !ok {
		return nil, fmt.Errorf("invalid cache entry")
	}

	// Update access time
	entry.AccessAt = time.Now()

	// Read from file
	file, err := os.Open(entry.FilePath)
	if err != nil {
		c.mu.Lock()
		c.index.Delete(key)
		c.mu.Unlock()
		return nil, fmt.Errorf("failed to open cache file: %w", err)
	}
	defer file.Close()

	if offset < 0 {
		return nil, fmt.Errorf("invalid cache offset: %d", offset)
	}

	// Seek to offset
	if _, err := file.Seek(offset, 0); err != nil {
		return nil, fmt.Errorf("failed to seek: %w", err)
	}

	if length <= 0 {
		// Size the buffer exactly from the file instead of io.ReadAll's
		// grow-and-copy loop; this path runs per cached chunk on hot reads.
		info, err := file.Stat()
		if err != nil {
			return nil, fmt.Errorf("failed to stat cache file: %w", err)
		}
		remaining := info.Size() - offset
		if remaining < 0 {
			remaining = 0
		}
		data := make([]byte, remaining)
		n, err := io.ReadFull(file, data)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("failed to read cache file: %w", err)
		}
		logging.Debug("Data cache hit",
			zap.String("key", key),
			zap.Int64("offset", offset),
			zap.Int("length", n),
		)
		return data[:n], nil
	}

	// Read data
	data := make([]byte, length)
	n, err := io.ReadFull(file, data)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("failed to read: %w", err)
	}

	logging.Debug("Data cache hit",
		zap.String("key", key),
		zap.Int64("offset", offset),
		zap.Int("length", n),
	)

	return data[:n], nil
}

// Write writes data to cache
func (c *DataCache) Write(key string, data []byte) error {
	if !c.enabled {
		return nil
	}

	size := int64(len(data))

	// Check if data fits in cache
	if size > c.maxSize {
		logging.Debug("Data too large for cache",
			zap.String("key", key),
			zap.Int64("size", size),
		)
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if value, ok := c.index.Get(key); ok {
		if entry, ok := value.(*CacheEntry); ok {
			c.deleteFile(entry.FilePath)
			c.index.Delete(key)
		}
	}

	// Evict entries if necessary
	for c.currentSize+size > c.maxSize {
		if !c.evictOldest() {
			break
		}
	}

	// Generate cache file path
	filePath := c.getCacheFilePath(key)
	if err := os.MkdirAll(filepath.Dir(filePath), 0700); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Write to file
	if err := os.WriteFile(filePath, data, 0600); err != nil {
		return fmt.Errorf("failed to write cache file: %w", err)
	}

	// Add to index
	entry := &CacheEntry{
		Key:       key,
		FilePath:  filePath,
		Size:      size,
		CachedAt:  time.Now(),
		AccessAt:  time.Now(),
		ChunkSize: c.chunkSize,
	}
	c.index.Set(key, entry)
	c.currentSize += size

	logging.Debug("Data cached",
		zap.String("key", key),
		zap.Int64("size", size),
		zap.Int64("currentSize", c.currentSize),
	)

	return nil
}

// ReadChunk reads an object chunk from cache.
func (c *DataCache) ReadChunk(objectKey string, chunkStart, chunkSize int64) ([]byte, error) {
	return c.Read(c.chunkKey(objectKey, chunkStart, chunkSize), 0, 0)
}

// ReadChunkRange reads only the requested byte range from a cached chunk, so
// small reads (e.g. 4k random I/O) cost one pread of the requested size
// instead of materializing the whole chunk.
func (c *DataCache) ReadChunkRange(objectKey string, chunkStart, chunkSize, offsetInChunk, length int64) ([]byte, error) {
	return c.Read(c.chunkKey(objectKey, chunkStart, chunkSize), offsetInChunk, length)
}

// ContainsChunk reports whether a chunk is present in the cache index without
// touching the chunk file. Used to decide read-ahead, where reading the bytes
// into memory would defeat the point.
func (c *DataCache) ContainsChunk(objectKey string, chunkStart, chunkSize int64) bool {
	if !c.enabled {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.index.Get(c.chunkKey(objectKey, chunkStart, chunkSize))
	return ok
}

// WriteChunk stores an object chunk in cache.
func (c *DataCache) WriteChunk(objectKey string, chunkStart, chunkSize int64, data []byte) error {
	return c.Write(c.chunkKey(objectKey, chunkStart, chunkSize), data)
}

// DeleteObject removes full-object and chunk cache entries for a logical object.
func (c *DataCache) DeleteObject(objectKey string) error {
	if !c.enabled {
		return nil
	}

	if err := c.Delete(objectKey); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.index.DeletePrefix(c.chunkPrefix(objectKey))
	return nil
}

// DeleteObjectPrefix removes full-object and chunk cache entries for all
// logical objects under an object path prefix.
func (c *DataCache) DeleteObjectPrefix(objectPrefix string) error {
	if !c.enabled {
		return nil
	}

	if objectPrefix != "/" && !strings.HasSuffix(objectPrefix, "/") {
		objectPrefix += "/"
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.index.DeletePrefix(objectPrefix)
	c.index.DeletePrefix(c.chunkPrefix(objectPrefix))
	return nil
}

// ChunkSize returns the configured cache chunk size.
func (c *DataCache) ChunkSize() int64 {
	if c == nil || !c.enabled || c.chunkSize <= 0 {
		return 0
	}
	return c.chunkSize
}

// Delete removes an entry from cache
func (c *DataCache) Delete(key string) error {
	if !c.enabled {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	value, ok := c.index.Get(key)
	if !ok {
		return nil
	}

	entry, ok := value.(*CacheEntry)
	if !ok {
		return nil
	}

	c.deleteFile(entry.FilePath)
	c.index.Delete(key)
	c.currentSize -= entry.Size

	logging.Debug("Data cache entry deleted",
		zap.String("key", key),
		zap.Int64("size", entry.Size),
	)

	return nil
}

// Clear removes all entries from cache
func (c *DataCache) Clear() error {
	if !c.enabled {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove all files
	if err := os.RemoveAll(c.basePath); err != nil {
		return fmt.Errorf("failed to clear cache: %w", err)
	}

	// Recreate directory
	if err := os.MkdirAll(c.basePath, 0700); err != nil {
		return fmt.Errorf("failed to recreate cache directory: %w", err)
	}

	c.index.Clear()
	c.currentSize = 0

	logging.Info("Data cache cleared")
	return nil
}

// Stats returns cache statistics
func (c *DataCache) Stats() DataCacheStats {
	if !c.enabled {
		return DataCacheStats{}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	indexStats := c.index.Stats()

	return DataCacheStats{
		CacheStats:   indexStats,
		CurrentSize:  c.currentSize,
		MaxSize:      c.maxSize,
		UsagePercent: float64(c.currentSize) / float64(c.maxSize) * 100,
	}
}

// IsEnabled returns whether the cache is enabled
func (c *DataCache) IsEnabled() bool {
	return c.enabled
}

// evictOldest evicts the oldest entry (must be called with lock held)
func (c *DataCache) evictOldest() bool {
	// Get oldest entry from LRU
	stats := c.index.Stats()
	if stats.Size == 0 {
		return false
	}

	if expired := c.index.CleanExpired(); expired > 0 {
		return true
	}

	return c.index.EvictOldest()
}

// deleteFile deletes a cache file
func (c *DataCache) deleteFile(filePath string) {
	info, err := os.Stat(filePath)
	if err == nil {
		c.currentSize -= info.Size()
	}

	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		logging.Warn("Failed to delete cache file",
			zap.String("path", filePath),
			zap.Error(err),
		)
	}
}

// getCacheFilePath generates a cache file path for a key
func (c *DataCache) getCacheFilePath(key string) string {
	// Hash the key to create a safe filename
	hash := sha256.Sum256([]byte(key))
	hashStr := hex.EncodeToString(hash[:])

	// Use first 2 chars for subdirectory to avoid too many files in one dir
	subdir := hashStr[:2]
	filename := hashStr[2:]

	return filepath.Join(c.basePath, subdir, filename)
}

func (c *DataCache) chunkPrefix(objectKey string) string {
	return "chunk:" + objectKey + ":"
}

func (c *DataCache) chunkKey(objectKey string, chunkStart, chunkSize int64) string {
	return fmt.Sprintf("%s%d:%d", c.chunkPrefix(objectKey), chunkStart, chunkSize)
}

// calculateSize calculates the current cache size
func (c *DataCache) calculateSize() error {
	var totalSize int64

	err := filepath.Walk(c.basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})

	if err != nil {
		return err
	}

	c.currentSize = totalSize
	return nil
}

// IsChunkKey reports whether a cache key belongs to a chunk entry.
func IsChunkKey(key string) bool {
	return strings.HasPrefix(key, "chunk:")
}

// DataCacheStats represents data cache statistics
type DataCacheStats struct {
	CacheStats
	CurrentSize  int64
	MaxSize      int64
	UsagePercent float64
}

// Made with Bob
