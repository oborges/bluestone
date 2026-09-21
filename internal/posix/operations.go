package posix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/cache"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/metrics"
	"github.com/oborges/bluestone/pkg/types"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// OperationsHandler handles POSIX filesystem operations
type OperationsHandler struct {
	cosClient     ObjectStore
	metadataCache *cache.MetadataCache
	dataCache     *cache.DataCache
	translator    *PathTranslator
	perfConfig    *config.PerformanceConfig
	readGroup     singleflight.Group
	listings      listingFence
}

// ObjectStore is the COS API surface used by POSIX operations and refresh scans.
type ObjectStore interface {
	GetObject(ctx context.Context, key string) ([]byte, error)
	GetObjectRange(ctx context.Context, key string, offset, length int64) ([]byte, error)
	GetObjectStream(ctx context.Context, key string) (io.ReadCloser, error)
	PutObject(ctx context.Context, key string, data []byte, metadata map[string]string) error
	DeleteObject(ctx context.Context, key string) error
	HeadObject(ctx context.Context, key string) (*types.ObjectMetadata, error)
	ListObjects(ctx context.Context, prefix string, maxKeys int) ([]*types.ObjectMetadata, error)
	CopyObject(ctx context.Context, sourceKey, destKey string) error
	CopyObjectWithMetadata(ctx context.Context, sourceKey, destKey string, metadata map[string]string) error
	UpdateObjectMetadata(ctx context.Context, key string, metadata map[string]string) error
}

// NewOperationsHandler creates a new operations handler
func NewOperationsHandler(
	cosClient ObjectStore,
	metadataCache *cache.MetadataCache,
	dataCache *cache.DataCache,
	perfConfig *config.PerformanceConfig,
) *OperationsHandler {
	if perfConfig == nil {
		perfConfig = &config.PerformanceConfig{
			MaxFullObjectReadMB: config.DefaultMaxFullObjectReadMB,
			MaxDirectoryEntries: config.DefaultMaxDirectoryEntries,
		}
	}

	return &OperationsHandler{
		cosClient:     cosClient,
		metadataCache: metadataCache,
		dataCache:     dataCache,
		translator:    NewPathTranslator(""),
		perfConfig:    perfConfig,
	}
}

func (h *OperationsHandler) maxFullObjectReadBytes() int64 {
	limitMB := config.DefaultMaxFullObjectReadMB
	if h.perfConfig != nil && h.perfConfig.MaxFullObjectReadMB > 0 {
		limitMB = h.perfConfig.MaxFullObjectReadMB
	}
	return int64(limitMB) * 1024 * 1024
}

func (h *OperationsHandler) maxDirectoryEntries() int {
	if h.perfConfig != nil && h.perfConfig.MaxDirectoryEntries > 0 {
		return h.perfConfig.MaxDirectoryEntries
	}
	return config.DefaultMaxDirectoryEntries
}

func (h *OperationsHandler) maxConcurrentReadFetches() int {
	if h.perfConfig != nil && h.perfConfig.MaxConcurrentReads > 0 {
		return h.perfConfig.MaxConcurrentReads
	}
	return 8
}

func (h *OperationsHandler) readAheadBytes() int64 {
	if h.perfConfig != nil && h.perfConfig.ReadAheadKB > 0 {
		return int64(h.perfConfig.ReadAheadKB) * 1024
	}
	return int64(config.DefaultReadAheadKB) * 1024
}

func (h *OperationsHandler) dataCacheEnabled() bool {
	return h.dataCache != nil && h.dataCache.IsEnabled()
}

func (h *OperationsHandler) invalidateFileMutation(path string) {
	h.invalidateObjectPath(path)
	h.invalidateDataPath(path)
}

// InvalidateFileMutation exposes cache invalidation for components that mutate
// objects outside this handler (e.g. deferred staged deletes).
func (h *OperationsHandler) InvalidateFileMutation(path string) {
	h.invalidateFileMutation(path)
}

// InvalidateObjectAfterSync purges caches that observed the pre-sync object.
// A completed upload of a file the parent listing already shows does not
// change the namespace, so that listing is kept: invalidating listings after
// every sync forced parent re-probes against COS under write churn. A file
// the cached listing lacks was visible only through its staging session,
// which may now be cleaned up, so that listing is dropped or the file would
// vanish from listings until it expired.
func (h *OperationsHandler) InvalidateObjectAfterSync(path string) {
	if h == nil {
		return
	}
	if h.metadataCache != nil {
		normalized := NormalizePath(path)
		parent := GetParentPath(normalized)
		h.metadataCache.Delete(normalized)
		// A listing of the parent in flight may predate the upload.
		h.listings.invalidate(parent)
		if !h.cachedListingHas(parent, GetBaseName(normalized)) {
			h.metadataCache.Delete(parent)
		}
	}
	h.invalidateDataPath(path)
}

// cachedListingHas reports whether dir has a cached listing naming name.
func (h *OperationsHandler) cachedListingHas(dir, name string) bool {
	entry, ok := h.metadataCache.Get(dir)
	if !ok || entry.ChildEntries == nil {
		return false
	}
	for _, child := range entry.ChildEntries {
		if child.Name() == name {
			return true
		}
	}
	return false
}

func (h *OperationsHandler) invalidateDirectoryMutation(path string) {
	if h == nil {
		return
	}

	if h.metadataCache != nil {
		normalized := NormalizePath(path)
		prefix := normalized
		if prefix != "/" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}

		h.metadataCache.Delete(normalized)
		h.metadataCache.InvalidatePrefix(prefix)
		h.invalidateAncestorListings(GetParentPath(normalized))
	}

	if h.dataCacheEnabled() {
		if err := h.dataCache.DeleteObjectPrefix(NormalizePath(path)); err != nil {
			logging.Warn("Failed to invalidate data cache prefix",
				zap.String("path", path),
				zap.Error(err))
		}
	}
}

func (h *OperationsHandler) invalidateRenameMutation(oldPath, newPath string, isDir bool) {
	if isDir {
		h.invalidateDirectoryMutation(oldPath)
		h.invalidateDirectoryMutation(newPath)
		return
	}

	h.invalidateFileMutation(oldPath)
	h.invalidateFileMutation(newPath)
}

func (h *OperationsHandler) ensureFullObjectReadAllowed(ctx context.Context, path string) error {
	info, err := h.Stat(ctx, path)
	if err != nil {
		return err
	}

	limitBytes := h.maxFullObjectReadBytes()
	if info.Size() > limitBytes {
		return fmt.Errorf("full-object read for %s would allocate %d bytes, exceeding max_full_object_read_mb=%d; use ranged reads or raise the limit",
			path, info.Size(), limitBytes/(1024*1024))
	}

	return nil
}

// Stat retrieves file/directory metadata
func (h *OperationsHandler) Stat(ctx context.Context, path string) (_ *FileInfo, err error) {
	log := logging.WithOperation("Stat").With(zap.String("path", path))
	start := time.Now()
	defer func() {
		metrics.RecordRequest(ctx, "stat", err, time.Since(start))
	}()

	// Check cache first, but skip if it's an implicit directory (needs validation)
	if entry, ok := h.metadataCache.Get(path); ok {
		// A fresh negative answers ENOENT without touching the object
		// store; an expired negative falls through to a fresh probe.
		if entry.Negative {
			if time.Since(entry.CachedAt) < cache.NegativeTTL {
				metrics.RecordCacheHit("metadata")
				return nil, os.ErrNotExist
			}
		} else if !entry.IsImplicit {
			// If it's an implicit directory, don't trust the cache - validate it exists
			metrics.RecordCacheHit("metadata")
			log.Debug("Metadata cache hit")

			// If we have FileInfo, use it directly
			if entry.FileInfo != nil {
				return entry.FileInfo.(*FileInfo), nil
			}

			// Fallback: construct from attributes
			mode := os.FileMode(0644)
			modTime := DefaultAttributes(entry.IsDir).Mtime
			size := int64(0)
			if entry.Attributes != nil {
				mode = entry.Attributes.Mode
				modTime = entry.Attributes.Mtime
			}
			if entry.IsDir {
				mode = mode | os.ModeDir
			}

			return &FileInfo{
				name:    GetBaseName(path),
				size:    size,
				mode:    mode,
				modTime: modTime,
				isDir:   entry.IsDir,
				attrs:   entry.Attributes,
			}, nil
		}
		// Implicit directory - fall through to validate
		log.Info("Implicit directory detected, validating existence", zap.String("path", path))
	}
	metrics.RecordCacheMiss("metadata")
	log.Debug("Stat cache miss", zap.String("path", path))

	// The export root exists by definition; never depend on the object store
	// to answer for it. This keeps the mount usable (and creates at the root
	// working) during a backend outage.
	if NormalizePath(path) == "/" {
		attrs := DefaultAttributes(true)
		info := &FileInfo{
			name:    "/",
			size:    0,
			mode:    attrs.Mode | os.ModeDir,
			modTime: attrs.Mtime,
			isDir:   true,
			attrs:   attrs,
		}
		h.metadataCache.SetFileInfo(path, info, attrs)
		return info, nil
	}

	// Translate path to object key
	objectKey := h.translator.ToObjectKey(path)

	// A backend failure that is not a definite "not found" must not be
	// reported as nonexistence: false ENOENT during an object-store outage
	// corrupts application behavior. Track it and surface an I/O error.
	var backendErr error
	trackBackendErr := func(err error) {
		if err != nil && !errors.Is(err, os.ErrNotExist) && backendErr == nil {
			backendErr = err
		}
	}

	// Probe file and directory-marker existence concurrently: lookups of
	// missing names (the prelude to every file create) previously paid these
	// round trips sequentially.
	dirKey := ToDirectoryKey(objectKey)
	type headResult struct {
		metadata *types.ObjectMetadata
		err      error
	}
	dirCh := make(chan headResult, 1)
	metrics.RecordCOSHeadObject()
	metrics.RecordCOSHeadObject()
	go func() {
		m, err := h.cosClient.HeadObject(ctx, dirKey)
		dirCh <- headResult{metadata: m, err: err}
	}()

	metadata, err := h.cosClient.HeadObject(ctx, objectKey)
	trackBackendErr(err)
	if err == nil {
		// It's a file
		attrs := DecodePOSIXAttributes(metadata.Metadata, false)
		info := &FileInfo{
			name:          GetBaseName(path),
			size:          metadata.Size,
			mode:          attrs.Mode,
			modTime:       reportedModTime(metadata.Metadata, metadata.LastModified),
			isDir:         false,
			attrs:         attrs,
			objectModTime: metadata.LastModified,
		}

		// Cache the result
		h.metadataCache.SetFileInfo(path, info, attrs)

		// Removed debug logging from hot path
		return info, nil
	}

	// Try as directory
	dirRes := <-dirCh
	metadata, err = dirRes.metadata, dirRes.err
	trackBackendErr(err)
	if err == nil {
		// It's a directory
		attrs := DecodePOSIXAttributes(metadata.Metadata, true)
		info := &FileInfo{
			name:          GetBaseName(path),
			size:          0,
			mode:          attrs.Mode | os.ModeDir,
			modTime:       reportedModTime(metadata.Metadata, metadata.LastModified),
			isDir:         true,
			attrs:         attrs,
			objectModTime: metadata.LastModified,
		}

		// Cache the result
		h.metadataCache.SetFileInfo(path, info, attrs)

		// Removed debug logging from hot path
		return info, nil
	}

	// Check if it's an implicit directory (has children but no marker object)
	// This happens when directories are created implicitly by uploading files
	prefix := objectKey
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	metrics.RecordCOSListObjects()
	objects, err := h.cosClient.ListObjects(ctx, prefix, 1)
	trackBackendErr(err)
	if err == nil && len(objects) > 0 {
		// It's an implicit directory - has children
		log.Debug("Implicit directory detected", zap.String("prefix", prefix))

		// Use default directory attributes
		attrs := DefaultAttributes(true)

		info := &FileInfo{
			name:    GetBaseName(path),
			size:    0,
			mode:    attrs.Mode,
			modTime: attrs.Mtime,
			isDir:   true,
			attrs:   attrs,
		}

		// Cache the result as a normal directory (NOT implicit)
		// Once validated, we don't need to re-validate on every Stat() call
		h.metadataCache.SetFileInfo(path, info, attrs)

		log.Debug("Implicit directory stat successful")
		return info, nil
	}

	if backendErr != nil {
		// Serve stale metadata rather than failing: an expired cache entry
		// is a better answer during an object-store outage than an error,
		// and keeps the namespace usable until the backend recovers.
		if entry, ok := h.metadataCache.GetStale(path); ok && entry != nil {
			if entry.Negative {
				log.Warn("Serving stale negative: object store unreachable",
					zap.Error(backendErr))
				return nil, os.ErrNotExist
			}
			if info := fileInfoFromCacheEntry(path, entry); info != nil {
				log.Warn("Serving stale metadata: object store unreachable",
					zap.Error(backendErr))
				return info, nil
			}
		}
		// A stale listing of the parent can still answer the lookup: if the
		// name is absent there, report ENOENT (a stale negative). This is
		// what lets file creation keep working during an outage — clients
		// revalidate the name with a lookup before sending OPEN(create).
		if parentEntry, ok := h.metadataCache.GetStale(GetParentPath(NormalizePath(path))); ok &&
			parentEntry != nil && parentEntry.ChildEntries != nil {
			name := GetBaseName(path)
			for _, child := range parentEntry.ChildEntries {
				if child.Name() == name {
					log.Warn("Serving stale metadata from parent listing: object store unreachable",
						zap.Error(backendErr))
					if fi, isFileInfo := child.(*FileInfo); isFileInfo {
						return fi, nil
					}
					break
				}
			}
			log.Warn("Serving stale negative from parent listing: object store unreachable",
				zap.Error(backendErr))
			return nil, os.ErrNotExist
		}
		log.Error("Stat could not determine existence: object store unreachable",
			zap.Error(backendErr))
		return nil, fmt.Errorf("object store unreachable for %s: %w", path, backendErr)
	}

	// Cache the miss briefly so lookup/create storms do not pay repeated
	// object-store probes for the same missing path. Local creates overwrite
	// the entry through the staging layer and normal invalidation.
	h.metadataCache.SetNegative(path)

	log.Debug("Path not found")
	return nil, os.ErrNotExist
}

// fileInfoFromCacheEntry reconstructs a FileInfo from a metadata cache entry.
func fileInfoFromCacheEntry(path string, entry *cache.MetadataEntry) *FileInfo {
	if entry.FileInfo != nil {
		if info, ok := entry.FileInfo.(*FileInfo); ok {
			return info
		}
	}
	mode := os.FileMode(0644)
	modTime := DefaultAttributes(entry.IsDir).Mtime
	if entry.Attributes != nil {
		mode = entry.Attributes.Mode
		modTime = entry.Attributes.Mtime
	}
	if entry.IsDir {
		mode = mode | os.ModeDir
	}
	return &FileInfo{
		name:    GetBaseName(path),
		size:    0,
		mode:    mode,
		modTime: modTime,
		isDir:   entry.IsDir,
		attrs:   entry.Attributes,
	}
}

// reportedModTime is the modification time to report for an object: the one a
// client set, when the object's metadata carries it, and otherwise when the
// object itself last changed. Clients expect a time they set to survive, and
// COS rewrites an object's own last-modified whenever the gateway uploads or
// updates it.
func reportedModTime(metadata map[string]string, objectModTime time.Time) time.Time {
	if stored, ok := StoredMtime(metadata); ok {
		return stored
	}
	return objectModTime
}

// objectModTime reports when the object behind info last changed in COS,
// falling back to its reported modification time for entries that do not
// track the two separately.
func objectModTime(info os.FileInfo) time.Time {
	if fi, ok := info.(*FileInfo); ok {
		return fi.ObjectModTime()
	}
	return info.ModTime()
}

// refreshCachedEntries replaces listing entries with what an earlier stat
// cached for the same file, so a listing reports the attributes and
// modification time a stat would report.
func (h *OperationsHandler) refreshCachedEntries(dirPath string, entries []*FileInfo) []*FileInfo {
	parent := NormalizePath(dirPath)
	for i, entry := range entries {
		cached, ok := h.metadataCache.Get(JoinPath(parent, entry.Name()))
		if !ok || cached.Negative || cached.Attributes == nil || cached.FileInfo == nil {
			continue
		}
		stated, ok := cached.FileInfo.(*FileInfo)
		if !ok || stated.IsDir() != entry.IsDir() {
			continue
		}
		// Only when it still describes the same object.
		if !entry.IsDir() && (stated.Size() != entry.Size() ||
			!objectModTime(stated).Truncate(time.Second).Equal(objectModTime(entry).Truncate(time.Second))) {
			continue
		}
		entries[i] = stated
	}
	return entries
}

// cachedEntry returns the attributes and reported modification time an earlier
// Stat cached for path, when they still describe the listed object (same kind,
// and for files the same size and object modification time). COS listings
// carry no object metadata, so this lets listings report real attributes
// without a HEAD request per entry.
func (h *OperationsHandler) cachedEntry(path string, obj *types.ObjectMetadata, isDir bool) (*types.POSIXAttributes, time.Time) {
	entry, ok := h.metadataCache.Get(path)
	if !ok || entry.Negative || entry.Attributes == nil || entry.FileInfo == nil || entry.IsDir != isDir {
		return nil, time.Time{}
	}
	// COS reports fractional seconds when listing but whole seconds from a
	// HEAD, so compare at second resolution or the cache never matches.
	if !isDir && (entry.FileInfo.Size() != obj.Size ||
		!objectModTime(entry.FileInfo).Truncate(time.Second).Equal(obj.LastModified.Truncate(time.Second))) {
		return nil, time.Time{}
	}
	return entry.Attributes, entry.FileInfo.ModTime()
}

// DownloadToFile streams the object from COS into a local file path
func (h *OperationsHandler) DownloadToFile(ctx context.Context, path string, localPath string) error {
	log := logging.WithOperation("DownloadToFile").With(
		zap.String("path", path),
		zap.String("local_path", localPath),
	)

	objectKey := h.translator.ToObjectKey(path)
	metrics.RecordCOSGetObject()

	stream, err := h.cosClient.GetObjectStream(ctx, objectKey)
	if err != nil {
		return err
	}
	defer stream.Close()

	file, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open local file for prefetch: %w", err)
	}
	defer file.Close()

	if _, err := io.Copy(file, stream); err != nil {
		return fmt.Errorf("failed to copy stream body to file: %w", err)
	}

	log.Debug("Stream download to file complete")
	return nil
}

// ReadFile reads file content
func (h *OperationsHandler) ReadFile(ctx context.Context, path string, offset, length int64) (_ []byte, err error) {
	log := logging.WithOperation("ReadFile").With(
		zap.String("path", path),
		zap.Int64("offset", offset),
		zap.Int64("length", length),
	)
	start := time.Now()
	defer func() {
		metrics.RecordRequest(ctx, "read", err, time.Since(start))
	}()

	// Try cache first - but only for full file reads
	// The cache is designed for complete files, not partial ranges
	fullObjectRead := length <= 0
	if fullObjectRead {
		if err := h.ensureFullObjectReadAllowed(ctx, path); err != nil {
			log.Error("Full-object read rejected", zap.Error(err))
			return nil, err
		}
	}

	if h.dataCacheEnabled() && offset == 0 && length == 0 {
		if data, err := h.dataCache.Read(path, offset, length); err == nil {
			metrics.RecordCacheHit("data")
			metrics.RecordBytesRead(int64(len(data)))
			log.Debug("Data cache hit", zap.Int("bytes", len(data)))
			return data, nil
		}
		metrics.RecordCacheMiss("data")
	}

	// Read from COS
	objectKey := h.translator.ToObjectKey(path)

	var data []byte

	if length > 0 {
		if h.dataCacheEnabled() {
			data, err = h.readRangeWithCache(ctx, path, objectKey, offset, length)
		} else {
			data, err = h.cosClient.GetObjectRange(ctx, objectKey, offset, length)
		}
	} else {
		// Full read - can be cached
		data, err = h.cosClient.GetObject(ctx, objectKey)
	}

	if err != nil {
		log.Error("Failed to read file", zap.Error(err))
		return nil, err
	}

	// Cache the data - but only for full file reads
	if h.dataCacheEnabled() && len(data) > 0 && offset == 0 && length == 0 {
		if err := h.dataCache.Write(path, data); err != nil {
			log.Warn("Failed to cache data", zap.Error(err))
		}
	}

	metrics.RecordBytesRead(int64(len(data)))
	log.Debug("File read successful", zap.Int("bytes", len(data)))
	return data, nil
}

func (h *OperationsHandler) readRangeWithCache(ctx context.Context, path, objectKey string, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("invalid read range offset=%d length=%d", offset, length)
	}

	requestEnd := offset + length

	chunkSize := h.dataCache.ChunkSize()
	if chunkSize <= 0 {
		chunkSize = 1024 * 1024
	}

	firstChunk := (offset / chunkSize) * chunkSize
	lastRequiredChunk := ((requestEnd - 1) / chunkSize) * chunkSize

	// Fast path: when every required chunk is cached, serve the request with
	// one ranged read per chunk — exactly the requested bytes, no whole-chunk
	// materialization. This is the hot path for warm sequential reads (NFS
	// clients issue large reads at arbitrary alignment, often spanning two
	// chunks) and for small random reads.
	if data, ok := h.readWarmChunks(path, offset, length, chunkSize, firstChunk, lastRequiredChunk); ok {
		return data, nil
	}

	// Gather the chunks the request actually needs.
	chunks := make(map[int64][]byte)
	var missing []int64
	for chunkStart := firstChunk; chunkStart <= lastRequiredChunk; chunkStart += chunkSize {
		if cached, err := h.dataCache.ReadChunk(path, chunkStart, chunkSize); err == nil {
			metrics.RecordCacheHit("data")
			chunks[chunkStart] = cached
			continue
		}
		metrics.RecordCacheMiss("data")
		missing = append(missing, chunkStart)
	}

	// Read-ahead engages only when the request itself missed: fully warm
	// reads must not re-touch chunks beyond what was asked for. Read-ahead
	// candidates use an index-only existence probe so already-cached chunks
	// cost nothing, and the window is clamped to the known object size so
	// small files do not trigger doomed past-EOF range requests.
	if len(missing) > 0 {
		fetchLength := h.readAheadBytes()
		if fetchLength < length {
			fetchLength = length
		}
		fetchEnd := offset + fetchLength
		if size, ok := h.cachedObjectSize(path); ok && fetchEnd > size {
			fetchEnd = size
		}
		if fetchEnd < requestEnd {
			fetchEnd = requestEnd
		}
		readAheadLast := ((fetchEnd - 1) / chunkSize) * chunkSize
		for chunkStart := lastRequiredChunk + chunkSize; chunkStart <= readAheadLast; chunkStart += chunkSize {
			if !h.dataCache.ContainsChunk(path, chunkStart, chunkSize) {
				missing = append(missing, chunkStart)
			}
		}

		fetched, err := h.fetchMissingChunks(ctx, path, objectKey, missing, chunkSize, lastRequiredChunk)
		if err != nil {
			return nil, err
		}
		for chunkStart, data := range fetched {
			chunks[chunkStart] = data
		}
	}

	out := make([]byte, 0, requestEnd-offset)
	for chunkStart := firstChunk; chunkStart <= lastRequiredChunk && int64(len(out)) < requestEnd-offset; chunkStart += chunkSize {
		chunk := chunks[chunkStart]
		if len(chunk) == 0 {
			continue
		}

		startInChunk := int64(0)
		if offset > chunkStart {
			startInChunk = offset - chunkStart
		}
		endInChunk := int64(len(chunk))
		if requestEnd < chunkStart+endInChunk {
			endInChunk = requestEnd - chunkStart
		}
		if startInChunk < 0 {
			startInChunk = 0
		}
		if endInChunk > int64(len(chunk)) {
			endInChunk = int64(len(chunk))
		}
		if endInChunk > startInChunk {
			out = append(out, chunk[startInChunk:endInChunk]...)
		}
	}

	return out, nil
}

// readWarmChunks serves a read purely from cached chunks with ranged reads,
// or reports ok=false if any required chunk is missing. A short chunk means
// object EOF, which ends the read.
func (h *OperationsHandler) readWarmChunks(path string, offset, length, chunkSize, firstChunk, lastRequiredChunk int64) ([]byte, bool) {
	requestEnd := offset + length
	out := make([]byte, 0, length)
	for chunkStart := firstChunk; chunkStart <= lastRequiredChunk; chunkStart += chunkSize {
		readStart := int64(0)
		if offset > chunkStart {
			readStart = offset - chunkStart
		}
		readEnd := chunkSize
		if requestEnd < chunkStart+chunkSize {
			readEnd = requestEnd - chunkStart
		}
		data, err := h.dataCache.ReadChunkRange(path, chunkStart, chunkSize, readStart, readEnd-readStart)
		if err != nil {
			return nil, false
		}
		metrics.RecordCacheHit("data")
		out = append(out, data...)
		if int64(len(data)) < readEnd-readStart {
			// Short chunk: object EOF.
			break
		}
	}
	return out, true
}

// cachedObjectSize returns the object size from the metadata cache when a
// fresh file entry exists. Used to bound read-ahead; callers must tolerate
// absence.
func (h *OperationsHandler) cachedObjectSize(path string) (int64, bool) {
	if h == nil || h.metadataCache == nil {
		return 0, false
	}
	entry, ok := h.metadataCache.Get(path)
	if !ok || entry == nil || entry.IsDir || entry.FileInfo == nil {
		return 0, false
	}
	info, ok := entry.FileInfo.(os.FileInfo)
	if !ok {
		return 0, false
	}
	return info.Size(), true
}

func (h *OperationsHandler) fetchMissingChunks(ctx context.Context, path, objectKey string, missing []int64, chunkSize, lastRequiredChunk int64) (map[int64][]byte, error) {
	type result struct {
		start int64
		data  []byte
		err   error
	}

	parallelism := h.maxConcurrentReadFetches()
	if parallelism < 1 {
		parallelism = 1
	}
	if parallelism > len(missing) {
		parallelism = len(missing)
	}

	jobs := make(chan int64)
	results := make(chan result, len(missing))
	var wg sync.WaitGroup

	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkStart := range jobs {
				data, err := h.fetchChunkSingleflight(ctx, path, objectKey, chunkStart, chunkSize)
				if err != nil {
					results <- result{start: chunkStart, err: err}
					continue
				}
				results <- result{start: chunkStart, data: data}
			}
		}()
	}

	for _, chunkStart := range missing {
		jobs <- chunkStart
	}
	close(jobs)
	wg.Wait()
	close(results)

	fetched := make(map[int64][]byte, len(missing))
	for res := range results {
		if res.err != nil {
			if res.start <= lastRequiredChunk {
				return nil, res.err
			}
			logging.Warn("Read-ahead chunk fetch failed",
				zap.String("path", path),
				zap.Int64("chunk_start", res.start),
				zap.Error(res.err))
			continue
		}
		fetched[res.start] = res.data
	}

	return fetched, nil
}

func (h *OperationsHandler) fetchChunkSingleflight(ctx context.Context, path, objectKey string, chunkStart, chunkSize int64) ([]byte, error) {
	groupKey := fmt.Sprintf("%s:%d:%d", path, chunkStart, chunkSize)
	value, err, _ := h.readGroup.Do(groupKey, func() (interface{}, error) {
		if cached, cacheErr := h.dataCache.ReadChunk(path, chunkStart, chunkSize); cacheErr == nil {
			metrics.RecordCacheHit("data")
			return cached, nil
		}

		metrics.RecordCOSGetObject()
		data, fetchErr := h.cosClient.GetObjectRange(ctx, objectKey, chunkStart, chunkSize)
		if fetchErr != nil {
			return nil, fetchErr
		}
		if len(data) > 0 {
			if cacheErr := h.dataCache.WriteChunk(path, chunkStart, chunkSize, data); cacheErr != nil {
				logging.Warn("Failed to cache read chunk",
					zap.String("path", path),
					zap.Int64("chunk_start", chunkStart),
					zap.Error(cacheErr))
			}
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	data, ok := value.([]byte)
	if !ok {
		return nil, fmt.Errorf("invalid read chunk type %T", value)
	}
	return data, nil
}

// WriteFile writes file content
func (h *OperationsHandler) WriteFile(ctx context.Context, path string, data []byte, attrs *types.POSIXAttributes) (err error) {
	log := logging.WithOperation("WriteFile").With(
		zap.String("path", path),
		zap.Int("bytes", len(data)),
	)
	start := time.Now()
	defer func() {
		metrics.RecordRequest(ctx, "write", err, time.Since(start))
	}()

	objectKey := h.translator.ToObjectKey(path)

	// Encode attributes
	metadata := EncodePOSIXAttributes(attrs)

	// Write to COS
	err = h.cosClient.PutObject(ctx, objectKey, data, metadata)
	if err != nil {
		log.Error("Failed to write file", zap.Error(err))
		return err
	}

	// Invalidate caches
	h.invalidateFileMutation(path)

	metrics.RecordBytesWritten(int64(len(data)))
	log.Debug("File write successful")
	return nil
}

// DeleteFile deletes a file
func (h *OperationsHandler) DeleteFile(ctx context.Context, path string) (err error) {
	log := logging.WithOperation("DeleteFile").With(zap.String("path", path))
	start := time.Now()
	defer func() {
		metrics.RecordRequest(ctx, "delete", err, time.Since(start))
	}()

	objectKey := h.translator.ToObjectKey(path)

	// Delete from COS
	err = h.cosClient.DeleteObject(ctx, objectKey)
	if err != nil {
		log.Error("Failed to delete file", zap.Error(err))
		return err
	}

	// Invalidate caches
	h.invalidateFileMutation(path)

	log.Debug("File deleted successfully")
	return nil
}

// CreateDirectory creates a directory
func (h *OperationsHandler) CreateDirectory(ctx context.Context, path string, attrs *types.POSIXAttributes) (err error) {
	log := logging.WithOperation("CreateDirectory").With(zap.String("path", path))
	start := time.Now()
	defer func() {
		metrics.RecordRequest(ctx, "mkdir", err, time.Since(start))
	}()

	objectKey := ToDirectoryKey(h.translator.ToObjectKey(path))

	// Encode attributes
	if attrs == nil {
		attrs = DefaultAttributes(true)
	}
	metadata := EncodePOSIXAttributes(attrs)

	// Create directory marker in COS
	err = h.cosClient.PutObject(ctx, objectKey, []byte{}, metadata)
	if err != nil {
		log.Error("Failed to create directory", zap.Error(err))
		return err
	}

	// Invalidate the new directory metadata and all parent listings.
	h.invalidateDirectoryMutation(path)

	log.Debug("Directory created successfully")
	return nil
}

// DeleteDirectory deletes a directory
func (h *OperationsHandler) DeleteDirectory(ctx context.Context, path string) (err error) {
	log := logging.WithOperation("DeleteDirectory").With(zap.String("path", path))
	start := time.Now()
	defer func() {
		metrics.RecordRequest(ctx, "rmdir", err, time.Since(start))
	}()

	// Check if directory is empty
	entries, err := h.ListDirectory(ctx, path)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		log.Warn("Directory not empty", zap.Int("entries", len(entries)))
		return fmt.Errorf("directory not empty")
	}

	objectKey := ToDirectoryKey(h.translator.ToObjectKey(path))

	// Delete directory marker
	err = h.cosClient.DeleteObject(ctx, objectKey)
	if err != nil {
		log.Error("Failed to delete directory", zap.Error(err))
		return err
	}

	// Invalidate caches
	h.invalidateDirectoryMutation(path)

	log.Debug("Directory deleted successfully")
	return nil
}

// ListDirectory lists directory contents
func (h *OperationsHandler) ListDirectory(ctx context.Context, path string) (_ []*FileInfo, err error) {
	log := logging.WithOperation("ListDirectory").With(zap.String("path", path))
	start := time.Now()
	cacheHit := false

	defer func() {
		duration := time.Since(start)
		metrics.RecordRequest(ctx, "readdir", err, duration)
		metrics.RecordListDirectory(duration, cacheHit)

		// Log first call, cache misses, or slow operations
		counters := metrics.GetGlobalCounters()
		callCount := counters.ListDirCalls.Load()
		if callCount == 1 || !cacheHit || duration > 10*time.Millisecond {
			log.Info("ListDirectory",
				zap.Bool("cache_hit", cacheHit),
				zap.Int64("duration_ms", duration.Milliseconds()),
				zap.Int64("call_number", callCount))
		}
	}()

	// Check cache first - NEW: Return full FileInfo entries directly (O(1) cache hit)
	if entry, ok := h.metadataCache.Get(path); ok && entry.ChildEntries != nil {
		maxEntries := h.maxDirectoryEntries()
		if len(entry.ChildEntries) > maxEntries {
			return nil, fmt.Errorf("cached directory listing for %s exceeds max_directory_entries=%d", path, maxEntries)
		}
		cacheHit = true
		metrics.RecordCacheHit("metadata")

		// Convert []os.FileInfo to []*FileInfo (just type assertion, no COS calls)
		entries := make([]*FileInfo, len(entry.ChildEntries))
		for i, info := range entry.ChildEntries {
			if fi, ok := info.(*FileInfo); ok {
				entries[i] = fi
			} else {
				// Shouldn't happen, but handle gracefully
				log.Warn("Invalid cached entry type, refetching from COS")
				h.metadataCache.InvalidatePath(path)
				goto fetchFromCOS
			}
		}

		// A listing is often cached before anything stats the files in it, and
		// COS listings carry no user metadata, so pick up attributes a later
		// stat recorded. These are cache lookups, not COS calls.
		return h.refreshCachedEntries(path, entries), nil
	}

	// DEPRECATED: Old cache format with just names (fallback for compatibility)
	if entry, ok := h.metadataCache.Get(path); ok && entry.Children != nil {
		log.Warn("Using deprecated cache format, will upgrade on next fetch")
		h.metadataCache.InvalidatePath(path)
		// Fall through to fetch fresh listing
	}

fetchFromCOS:
	cacheHit = false
	metrics.RecordCacheMiss("metadata")

	// List from COS
	prefix := ListPrefix(path)
	log.Info("ListDirectory cache miss, fetching from COS", zap.String("prefix", prefix))

	fenceDir := NormalizePath(path)
	fenceGeneration := h.listings.begin(fenceDir)
	fenceEnded := false
	defer func() {
		if !fenceEnded {
			h.listings.end(fenceDir, fenceGeneration)
		}
	}()

	cosStart := time.Now()
	metrics.RecordCOSListObjects()
	maxEntries := h.maxDirectoryEntries()
	objects, err := h.cosClient.ListObjects(ctx, prefix, maxEntries+1)
	if err != nil {
		// Serve a stale listing during an object-store outage rather than
		// failing the readdir; local staged entries are merged on top by the
		// NFS layer either way.
		if entry, ok := h.metadataCache.GetStale(path); ok && entry != nil && entry.ChildEntries != nil {
			stale := make([]*FileInfo, 0, len(entry.ChildEntries))
			valid := true
			for _, child := range entry.ChildEntries {
				fi, isFileInfo := child.(*FileInfo)
				if !isFileInfo {
					valid = false
					break
				}
				stale = append(stale, fi)
			}
			if valid {
				log.Warn("Serving stale directory listing: object store unreachable",
					zap.Error(err))
				return stale, nil
			}
		}
		log.Error("Failed to list directory", zap.Error(err))
		return nil, err
	}
	if len(objects) > maxEntries {
		return nil, fmt.Errorf("directory listing for %s exceeds max_directory_entries=%d", path, maxEntries)
	}

	log.Info("Got objects from COS",
		zap.Int("count", len(objects)),
		zap.Duration("cos_duration", time.Since(cosStart)))

	// Convert to FileInfo
	entries := make([]*FileInfo, 0, len(objects))
	seen := make(map[string]bool)

	for _, obj := range objects {
		log.Debug("Processing object", zap.String("key", obj.Key))

		// Remove prefix to get relative path
		relPath := obj.Key
		if prefix != "" {
			relPath = strings.TrimPrefix(relPath, prefix)
		}

		// Skip if empty
		if relPath == "" {
			log.Debug("Skipping empty relPath")
			continue
		}

		// For root directory, get the first path component
		// For subdirectories, get the immediate child
		parts := strings.Split(strings.Trim(relPath, "/"), "/")
		if len(parts) == 0 {
			log.Debug("Skipping - no parts")
			continue
		}

		name := parts[0]
		isDir := len(parts) > 1 || strings.HasSuffix(obj.Key, "/")

		// Skip if already seen
		if seen[name] {
			log.Debug("Skipping duplicate", zap.String("name", name))
			continue
		}
		seen[name] = true

		attrs := DecodePOSIXAttributes(obj.Metadata, isDir)
		modTime := reportedModTime(obj.Metadata, obj.LastModified)
		if cached, cachedModTime := h.cachedEntry(JoinPath(NormalizePath(path), name), obj, isDir); cached != nil {
			attrs = cached
			modTime = cachedModTime
		}
		mode := attrs.Mode
		if isDir && (mode&os.ModeDir) == 0 {
			mode = mode | os.ModeDir
		}

		info := &FileInfo{
			name:          name,
			size:          obj.Size,
			mode:          mode,
			modTime:       modTime,
			isDir:         isDir,
			attrs:         attrs,
			objectModTime: obj.LastModified,
		}

		log.Debug("Adding entry",
			zap.String("name", name),
			zap.Bool("isDir", isDir),
			zap.Int64("size", obj.Size))

		entries = append(entries, info)
	}

	// Cache the full FileInfo entries (NEW: O(1) retrieval on cache hit)
	osEntries := make([]os.FileInfo, len(entries))
	for i, entry := range entries {
		osEntries[i] = entry
	}
	fenceEnded = true
	if !h.listings.end(fenceDir, fenceGeneration) {
		// The directory changed while this listing was in flight; return it
		// but do not let it outlive the change.
		log.Info("Directory listed, not cached: changed during listing", zap.Int("entries", len(entries)))
		return entries, nil
	}
	h.metadataCache.SetDirEntries(path, osEntries)

	log.Info("Directory listed and cached", zap.Int("entries", len(entries)))
	return entries, nil
}

// RenameFile renames/moves a file or directory
// CopyFile copies one object to another inside the bucket, without the bytes
// passing through the gateway. It is what a server-side copy (SMB's
// FSCTL_SRV_COPYCHUNK) becomes when the whole file is being copied and the
// source is clean in COS.
//
// The copy keeps the source's metadata, or carries attrs instead when they
// are given.
func (h *OperationsHandler) CopyFile(ctx context.Context, srcPath, dstPath string, attrs *types.POSIXAttributes) (err error) {
	log := logging.WithOperation("CopyFile").With(
		zap.String("srcPath", srcPath),
		zap.String("dstPath", dstPath),
	)
	start := time.Now()
	defer func() {
		metrics.RecordRequest(ctx, "copy", err, time.Since(start))
	}()

	srcPath = NormalizePath(srcPath)
	dstPath = NormalizePath(dstPath)
	if srcPath == dstPath {
		return nil
	}

	info, err := h.Stat(ctx, srcPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return &os.PathError{Op: "copy", Path: srcPath, Err: errors.ErrUnsupported}
	}

	// The destination's cached metadata and any cached listing of its
	// directory describe a file that is about to change.
	defer h.invalidateFileMutation(dstPath)

	srcKey, dstKey := h.translator.ToObjectKey(srcPath), h.translator.ToObjectKey(dstPath)
	if attrs != nil {
		err = h.cosClient.CopyObjectWithMetadata(ctx, srcKey, dstKey, EncodePOSIXAttributes(attrs))
	} else {
		err = h.cosClient.CopyObject(ctx, srcKey, dstKey)
	}
	if err != nil {
		log.Error("Failed to copy object", zap.Error(err))
		return err
	}
	log.Debug("File copied inside the bucket", zap.Int64("size", info.Size()))
	return nil
}

func (h *OperationsHandler) RenameFile(ctx context.Context, oldPath, newPath string) (err error) {
	log := logging.WithOperation("RenameFile").With(
		zap.String("oldPath", oldPath),
		zap.String("newPath", newPath),
	)
	start := time.Now()
	defer func() {
		metrics.RecordRequest(ctx, "rename", err, time.Since(start))
	}()

	oldPath = NormalizePath(oldPath)
	newPath = NormalizePath(newPath)
	if oldPath == newPath {
		return nil
	}

	info, err := h.Stat(ctx, oldPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return h.renameDirectory(ctx, oldPath, newPath)
	}

	oldKey := h.translator.ToObjectKey(oldPath)
	newKey := h.translator.ToObjectKey(newPath)

	defer h.invalidateRenameMutation(oldPath, newPath, false)

	// Copy to new location
	err = h.cosClient.CopyObject(ctx, oldKey, newKey)
	if err != nil {
		log.Error("Failed to copy object", zap.Error(err))
		return err
	}

	// Delete old location
	err = h.cosClient.DeleteObject(ctx, oldKey)
	if err != nil {
		log.Error("Failed to delete old object", zap.Error(err))
		return fmt.Errorf("rename copied %s to %s but failed to delete source; both objects may exist: %w", oldPath, newPath, err)
	}

	log.Debug("File renamed successfully")
	return nil
}

func (h *OperationsHandler) renameDirectory(ctx context.Context, oldPath, newPath string) error {
	log := logging.WithOperation("RenameDirectory").With(
		zap.String("oldPath", oldPath),
		zap.String("newPath", newPath),
	)

	if oldPath == "/" {
		return fmt.Errorf("cannot rename root directory")
	}
	if IsDescendant(oldPath, newPath) {
		return fmt.Errorf("cannot rename directory %s into its own subtree %s", oldPath, newPath)
	}

	if existing, err := h.Stat(ctx, newPath); err == nil {
		return fmt.Errorf("cannot rename directory %s to existing path %s (is_dir=%t)", oldPath, newPath, existing.IsDir())
	} else if !os.IsNotExist(err) {
		return err
	}

	oldPrefix := ListPrefix(oldPath)
	newPrefix := ListPrefix(newPath)

	defer h.invalidateRenameMutation(oldPath, newPath, true)

	metrics.RecordCOSListObjects()
	objects, err := h.cosClient.ListObjects(ctx, oldPrefix, 0)
	if err != nil {
		log.Error("Failed to list directory objects for rename", zap.Error(err))
		return err
	}
	if len(objects) == 0 {
		return os.ErrNotExist
	}

	copied := 0
	for _, obj := range objects {
		if obj == nil || !strings.HasPrefix(obj.Key, oldPrefix) {
			continue
		}
		destKey := newPrefix + strings.TrimPrefix(obj.Key, oldPrefix)
		if err := h.cosClient.CopyObject(ctx, obj.Key, destKey); err != nil {
			log.Error("Failed to copy directory object",
				zap.String("source_key", obj.Key),
				zap.String("dest_key", destKey),
				zap.Error(err))
			return fmt.Errorf("directory rename copied %d of %d objects before copy failed at %s -> %s; source objects were not deleted and copied destination objects were left in place: %w",
				copied, len(objects), obj.Key, destKey, err)
		}
		copied++
	}

	deleted := 0
	for _, obj := range objects {
		if obj == nil || !strings.HasPrefix(obj.Key, oldPrefix) {
			continue
		}
		if err := h.cosClient.DeleteObject(ctx, obj.Key); err != nil {
			log.Error("Failed to delete source directory object after copy",
				zap.String("source_key", obj.Key),
				zap.Error(err))
			return fmt.Errorf("directory rename copied %d objects but deleted only %d source objects before delete failed at %s; source and destination may both contain objects: %w",
				copied, deleted, obj.Key, err)
		}
		deleted++
	}

	log.Debug("Directory renamed successfully",
		zap.Int("objects_copied", copied),
		zap.Int("objects_deleted", deleted))
	return nil
}

// UpdateAttributes applies a metadata-only attribute change without rewriting
// content. The update merges into the object's current metadata, read fresh,
// so other attributes and unrelated user metadata are kept.
func (h *OperationsHandler) UpdateAttributes(ctx context.Context, path string, update AttributeUpdate) (err error) {
	log := logging.WithOperation("UpdateAttributes").With(zap.String("path", path))
	start := time.Now()
	defer func() {
		metrics.RecordRequest(ctx, "setattr", err, time.Since(start))
	}()

	info, err := h.Stat(ctx, path)
	if err != nil {
		return err
	}
	objectKey := h.translator.ToObjectKey(path)
	if info.IsDir() {
		objectKey = ToDirectoryKey(objectKey)
	}

	// A cached Stat may predate another client's change, so merge into the
	// metadata the object has now.
	existing := map[string]string{}
	markerMissing := false
	head, headErr := h.cosClient.HeadObject(ctx, objectKey)
	switch {
	case headErr == nil:
		existing = head.Metadata
	case info.IsDir() && errors.Is(headErr, os.ErrNotExist):
		// An implicit directory has no marker object to update.
		markerMissing = true
	default:
		return headErr
	}

	attrs := DecodePOSIXAttributes(existing, info.IsDir())
	update.Apply(attrs, time.Now())
	metadata := MergePOSIXMetadata(existing, attrs)

	if markerMissing {
		err = h.cosClient.PutObject(ctx, objectKey, []byte{}, metadata)
	} else {
		// Copy-to-self with replaced metadata: the bytes are not rewritten.
		err = h.cosClient.UpdateObjectMetadata(ctx, objectKey, metadata)
	}
	if err != nil {
		log.Error("Failed to update attributes", zap.Error(err))
		return err
	}

	h.metadataCache.InvalidatePath(path)

	log.Debug("Attributes updated successfully")
	return nil
}

// BucketFull reports whether the bucket is refusing writes for its hard
// quota, for object stores that can tell.
func (h *OperationsHandler) BucketFull() bool {
	full, ok := h.cosClient.(interface{ BucketFull() bool })
	return ok && full.BucketFull()
}

// FileInfo represents file information
type FileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
	attrs   *types.POSIXAttributes
	// objectModTime is when the object itself last changed in COS, which is
	// not the reported modification time once a client has set one. Listings
	// compare it to decide whether cached attributes still apply.
	objectModTime time.Time
}

// Implement os.FileInfo interface
func (f *FileInfo) Name() string       { return f.name }
func (f *FileInfo) Size() int64        { return f.size }
func (f *FileInfo) Mode() os.FileMode  { return f.mode }
func (f *FileInfo) ModTime() time.Time { return f.modTime }
func (f *FileInfo) IsDir() bool        { return f.isDir }
func (f *FileInfo) Sys() interface{}   { return nil }

// ObjectModTime reports when the object last changed in COS. It matches
// ModTime unless a client set a modification time of its own.
func (f *FileInfo) ObjectModTime() time.Time {
	if f.objectModTime.IsZero() {
		return f.modTime
	}
	return f.objectModTime
}

// Attributes returns the entry's attributes: decoded from object metadata, or
// defaults when its source (such as a listing) carried none. Without a stored
// creation time, the modification time is reported as the best estimate.
func (f *FileInfo) Attributes() types.POSIXAttributes {
	attrs := *DefaultAttributes(f.isDir)
	if f.attrs != nil {
		attrs = *f.attrs
	}
	if attrs.Btime.IsZero() {
		attrs.Btime = f.modTime
	}
	return attrs
}

// NFSOwner reports the entry's stored owner to the NFS server, which would
// otherwise take it from Sys() and show every file as root's.
func (f *FileInfo) NFSOwner() (uid, gid uint32) {
	return OwnerIDs(f.Attributes())
}

// OwnerIDs returns attrs' uid and gid as NFS carries them. Ids outside the
// 32-bit range cannot be stored, so they report as 0.
func OwnerIDs(attrs types.POSIXAttributes) (uid, gid uint32) {
	return clampID(attrs.UID), clampID(attrs.GID)
}

func clampID(id int) uint32 {
	if id < 0 || id > math.MaxUint32 {
		return 0
	}
	return uint32(id)
}

var _ os.FileInfo = (*FileInfo)(nil)
var _ io.Closer = (*OperationsHandler)(nil)

// Close closes the operations handler
func (h *OperationsHandler) Close() error {
	return nil
}

// Made with Bob
