package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// PerformanceCounters tracks detailed performance metrics
type PerformanceCounters struct {
	// ReadDir metrics
	ReadDirCalls     atomic.Int64
	ReadDirTotalTime atomic.Int64 // nanoseconds
	ReadDirMaxTime   atomic.Int64 // nanoseconds

	// ListDirectory metrics
	ListDirCalls       atomic.Int64
	ListDirCacheHits   atomic.Int64
	ListDirCacheMisses atomic.Int64
	ListDirTotalTime   atomic.Int64 // nanoseconds

	// COS operation metrics
	COSListObjects atomic.Int64
	COSHeadObject  atomic.Int64
	COSGetObject   atomic.Int64

	// Object-side refresh metrics
	RefreshScans              atomic.Int64
	RefreshObjectsChanged     atomic.Int64
	RefreshCacheInvalidations atomic.Int64
	RefreshSkippedDirtyPaths  atomic.Int64
	RefreshConflicts          atomic.Int64

	// Transfer metrics
	BytesRead    atomic.Int64
	BytesWritten atomic.Int64

	// Conversion metrics
	ConversionTime atomic.Int64 // nanoseconds

	mu        sync.Mutex
	startTime time.Time
}

var globalCounters = &PerformanceCounters{
	startTime: time.Now(),
}

// GetGlobalCounters returns the global performance counters
func GetGlobalCounters() *PerformanceCounters {
	return globalCounters
}

// ResetCounters resets all counters
func ResetCounters() {
	globalCounters = &PerformanceCounters{
		startTime: time.Now(),
	}
}

// RecordReadDir records a ReadDir call
func RecordReadDir(duration time.Duration) {
	globalCounters.ReadDirCalls.Add(1)
	nanos := duration.Nanoseconds()
	globalCounters.ReadDirTotalTime.Add(nanos)

	// Update max time
	for {
		oldMax := globalCounters.ReadDirMaxTime.Load()
		if nanos <= oldMax {
			break
		}
		if globalCounters.ReadDirMaxTime.CompareAndSwap(oldMax, nanos) {
			break
		}
	}
}

// RecordListDirectory records a ListDirectory call
func RecordListDirectory(duration time.Duration, cacheHit bool) {
	globalCounters.ListDirCalls.Add(1)
	if cacheHit {
		globalCounters.ListDirCacheHits.Add(1)
	} else {
		globalCounters.ListDirCacheMisses.Add(1)
	}
	globalCounters.ListDirTotalTime.Add(duration.Nanoseconds())
}

// RecordCOSListObjects records a COS ListObjects call
func RecordCOSListObjects() {
	globalCounters.COSListObjects.Add(1)
}

// RecordCOSHeadObject records a COS HeadObject call
func RecordCOSHeadObject() {
	globalCounters.COSHeadObject.Add(1)
}

// RecordCOSGetObject records a COS GetObject call
func RecordCOSGetObject() {
	globalCounters.COSGetObject.Add(1)
}

// RecordObjectRefreshScan records one completed object-side refresh scan.
func RecordObjectRefreshScan() {
	globalCounters.RefreshScans.Add(1)
}

// RecordObjectRefreshObjectsChanged records objects whose list signature changed.
func RecordObjectRefreshObjectsChanged(count int) {
	if count > 0 {
		globalCounters.RefreshObjectsChanged.Add(int64(count))
	}
}

// RecordObjectRefreshCacheInvalidations records cache invalidations caused by refresh.
func RecordObjectRefreshCacheInvalidations(count int) {
	if count > 0 {
		globalCounters.RefreshCacheInvalidations.Add(int64(count))
	}
}

// RecordObjectRefreshSkippedDirtyPaths records dirty staged paths skipped by refresh.
func RecordObjectRefreshSkippedDirtyPaths(count int) {
	if count > 0 {
		globalCounters.RefreshSkippedDirtyPaths.Add(int64(count))
	}
}

// RecordObjectRefreshConflicts records dirty local paths that became conflicts.
func RecordObjectRefreshConflicts(count int) {
	if count > 0 {
		globalCounters.RefreshConflicts.Add(int64(count))
	}
}

// RecordBytesReadCounter records bytes read in the debug counters.
func RecordBytesReadCounter(bytes int64) {
	if bytes > 0 {
		globalCounters.BytesRead.Add(bytes)
	}
}

// RecordBytesWrittenCounter records bytes written in the debug counters.
func RecordBytesWrittenCounter(bytes int64) {
	if bytes > 0 {
		globalCounters.BytesWritten.Add(bytes)
	}
}

// GetTransferReport returns aggregate transfer counters for debug consumers.
func GetTransferReport() map[string]interface{} {
	return map[string]interface{}{
		"bytes_read_total":    globalCounters.BytesRead.Load(),
		"bytes_written_total": globalCounters.BytesWritten.Load(),
	}
}

// RecordConversion records time spent in conversion
func RecordConversion(duration time.Duration) {
	globalCounters.ConversionTime.Add(duration.Nanoseconds())
}

// GetReport generates a performance report
func (pc *PerformanceCounters) GetReport() map[string]interface{} {
	readDirCalls := pc.ReadDirCalls.Load()
	listDirCalls := pc.ListDirCalls.Load()

	avgReadDir := int64(0)
	if readDirCalls > 0 {
		avgReadDir = pc.ReadDirTotalTime.Load() / readDirCalls
	}

	avgListDir := int64(0)
	if listDirCalls > 0 {
		avgListDir = pc.ListDirTotalTime.Load() / listDirCalls
	}

	return map[string]interface{}{
		"uptime_seconds": time.Since(pc.startTime).Seconds(),
		"readdir": map[string]interface{}{
			"total_calls":    readDirCalls,
			"avg_latency_ms": float64(avgReadDir) / 1e6,
			"max_latency_ms": float64(pc.ReadDirMaxTime.Load()) / 1e6,
			"total_time_ms":  float64(pc.ReadDirTotalTime.Load()) / 1e6,
		},
		"listdirectory": map[string]interface{}{
			"total_calls":    listDirCalls,
			"cache_hits":     pc.ListDirCacheHits.Load(),
			"cache_misses":   pc.ListDirCacheMisses.Load(),
			"avg_latency_ms": float64(avgListDir) / 1e6,
			"total_time_ms":  float64(pc.ListDirTotalTime.Load()) / 1e6,
		},
		"cos_operations": map[string]interface{}{
			"list_objects": pc.COSListObjects.Load(),
			"head_object":  pc.COSHeadObject.Load(),
			"get_object":   pc.COSGetObject.Load(),
		},
		"object_refresh": map[string]interface{}{
			"scans":               pc.RefreshScans.Load(),
			"objects_changed":     pc.RefreshObjectsChanged.Load(),
			"cache_invalidations": pc.RefreshCacheInvalidations.Load(),
			"skipped_dirty_paths": pc.RefreshSkippedDirtyPaths.Load(),
			"conflicts":           pc.RefreshConflicts.Load(),
		},
		"transfers": map[string]interface{}{
			"bytes_read_total":    pc.BytesRead.Load(),
			"bytes_written_total": pc.BytesWritten.Load(),
		},
		"conversion": map[string]interface{}{
			"total_time_ms": float64(pc.ConversionTime.Load()) / 1e6,
		},
	}
}

// Made with Bob
