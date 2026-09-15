package posix

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/metrics"
	"go.uber.org/zap"
)

// DirtyPathChecker reports whether a logical filesystem path has local staged
// writes that must remain authoritative over object-side changes.
type DirtyPathChecker func(path string) bool

// ObjectConflict describes an object-side change that collided with local
// dirty staged data for the same logical path.
type ObjectConflict struct {
	Path         string
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	Deleted      bool
	DetectedAt   time.Time
}

// ConflictRecorder records a dirty-path object-side conflict.
type ConflictRecorder func(ObjectConflict) error

// ObjectRefreshScanner periodically lists a COS prefix and invalidates local
// caches when object list signatures change.
type ObjectRefreshScanner struct {
	ops              *OperationsHandler
	prefix           string
	dirty            DirtyPathChecker
	conflictRecorder ConflictRecorder
	mu               sync.Mutex
	objects          map[string]objectSignature
	observed         bool
}

type objectSignature struct {
	size         int64
	etag         string
	lastModified int64
}

// NewObjectRefreshScanner creates a scanner for object-side changes.
func NewObjectRefreshScanner(ops *OperationsHandler, cfg *config.ObjectRefreshConfig, dirty DirtyPathChecker, conflictRecorders ...ConflictRecorder) *ObjectRefreshScanner {
	prefix := ""
	if cfg != nil {
		prefix = strings.TrimPrefix(cfg.Prefix, "/")
	}

	var conflictRecorder ConflictRecorder
	if len(conflictRecorders) > 0 {
		conflictRecorder = conflictRecorders[0]
	}

	return &ObjectRefreshScanner{
		ops:              ops,
		prefix:           prefix,
		dirty:            dirty,
		conflictRecorder: conflictRecorder,
		objects:          make(map[string]objectSignature),
	}
}

// Start runs refresh scans until ctx is canceled.
func (s *ObjectRefreshScanner) Start(ctx context.Context, interval time.Duration) {
	if s == nil || interval <= 0 {
		return
	}

	go func() {
		s.runOnce(ctx)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runOnce(ctx)
			}
		}
	}()
}

// RunOnce performs a single refresh scan. It is exported for focused tests and
// manual debug hooks.
func (s *ObjectRefreshScanner) RunOnce(ctx context.Context) {
	s.runOnce(ctx)
}

func (s *ObjectRefreshScanner) runOnce(ctx context.Context) {
	if s == nil || s.ops == nil || s.ops.cosClient == nil {
		return
	}

	start := time.Now()
	status := "success"
	changed := 0
	metadataInvalidations := 0
	dataInvalidations := 0
	skippedDirty := 0
	conflicts := 0

	defer func() {
		duration := time.Since(start)
		metrics.RecordObjectRefreshScan()
		metrics.RecordObjectRefreshScanPrometheus(status, duration)
		metrics.RecordObjectRefreshObjectsChangedPrometheus(changed)
		metrics.RecordObjectRefreshCacheInvalidationsPrometheus("metadata", metadataInvalidations)
		metrics.RecordObjectRefreshCacheInvalidationsPrometheus("data", dataInvalidations)
		metrics.RecordObjectRefreshSkippedDirtyPathsPrometheus(skippedDirty)
		metrics.RecordObjectRefreshConflictsPrometheus(conflicts)
		logging.Info("Object refresh scan completed",
			zap.String("prefix", s.prefix),
			zap.String("status", status),
			zap.Int("objects_changed", changed),
			zap.Int("metadata_invalidations", metadataInvalidations),
			zap.Int("data_invalidations", dataInvalidations),
			zap.Int("skipped_dirty_paths", skippedDirty),
			zap.Int("conflicts", conflicts),
			zap.Duration("duration", duration))
	}()

	metrics.RecordCOSListObjects()
	objects, err := s.ops.cosClient.ListObjects(ctx, s.prefix, 0)
	if err != nil {
		status = "error"
		logging.Warn("Object refresh scan failed",
			zap.String("prefix", s.prefix),
			zap.Error(err))
		return
	}

	current := make(map[string]objectSignature, len(objects))
	for _, obj := range objects {
		if obj == nil || obj.Key == "" {
			continue
		}
		current[obj.Key] = objectSignature{
			size:         obj.Size,
			etag:         obj.ETag,
			lastModified: obj.LastModified.UnixNano(),
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	changedKeys := make(map[string]struct{})
	if s.observed {
		for key, sig := range current {
			if previous, ok := s.objects[key]; !ok || previous != sig {
				changedKeys[key] = struct{}{}
			}
		}
		for key := range s.objects {
			if _, ok := current[key]; !ok {
				changedKeys[key] = struct{}{}
			}
		}
	}

	for key := range changedKeys {
		path := s.ops.translator.ToFSPath(key)
		if path == "/" || path == "." {
			continue
		}

		changed++
		if s.isDirty(path) {
			if s.conflictRecorder != nil {
				conflict := s.objectConflict(path, key, current)
				if err := s.conflictRecorder(conflict); err == nil {
					conflicts++
					metadataInvalidations += s.ops.invalidateObjectPath(path)
					if s.ops.invalidateDataPath(path) {
						dataInvalidations++
					}
					continue
				} else {
					logging.Warn("Failed to record dirty-path object refresh conflict",
						zap.String("path", path),
						zap.String("key", key),
						zap.Error(err))
				}
			}
			skippedDirty++
			metadataInvalidations += s.ops.invalidateParentListing(path)
			continue
		}

		metadataInvalidations += s.ops.invalidateObjectPath(path)
		if s.ops.invalidateDataPath(path) {
			dataInvalidations++
		}
	}

	s.objects = current
	s.observed = true
}

func (s *ObjectRefreshScanner) isDirty(path string) bool {
	return s.dirty != nil && s.dirty(path)
}

func (s *ObjectRefreshScanner) objectConflict(path, key string, current map[string]objectSignature) ObjectConflict {
	conflict := ObjectConflict{
		Path:       path,
		Key:        key,
		Deleted:    true,
		DetectedAt: time.Now(),
	}
	if sig, ok := current[key]; ok {
		conflict.Size = sig.size
		conflict.ETag = sig.etag
		conflict.LastModified = time.Unix(0, sig.lastModified)
		conflict.Deleted = false
	}
	return conflict
}

func (h *OperationsHandler) invalidateObjectPath(path string) int {
	if h == nil || h.metadataCache == nil {
		return 0
	}
	h.metadataCache.Delete(path)
	return 1 + h.invalidateAncestorListings(GetParentPath(path))
}

func (h *OperationsHandler) invalidateParentListing(path string) int {
	if h == nil || h.metadataCache == nil {
		return 0
	}
	return h.invalidateAncestorListings(GetParentPath(path))
}

func (h *OperationsHandler) invalidateAncestorListings(dir string) int {
	logging.Debug("Invalidating ancestor listings", zap.String("from", dir))
	count := 0
	for {
		h.metadataCache.Delete(dir)
		count++
		if dir == "/" || dir == "" {
			break
		}
		dir = GetParentPath(dir)
	}
	return count
}

func (h *OperationsHandler) invalidateDataPath(path string) bool {
	if h == nil || !h.dataCacheEnabled() {
		return false
	}
	if err := h.dataCache.DeleteObject(path); err != nil {
		logging.Warn("Failed to invalidate data cache during object refresh",
			zap.String("path", path),
			zap.Error(err))
	}
	return true
}
