package nfs

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/metrics"
	gonfs "github.com/willscott/go-nfs"
)

// InstrumentedFilesystem wraps a billy.Filesystem to track NFS-level operations
type InstrumentedFilesystem struct {
	billy.Filesystem
	logger *logging.KVLogger

	// Per-path tracking for detecting loops
	pathCalls sync.Map // map[string]*PathCallTracker
}

// PathCallTracker tracks calls to a specific path
type PathCallTracker struct {
	callCount   atomic.Int64
	firstCall   time.Time
	lastCall    time.Time
	lastReturn  time.Time // When the last call returned
	mu          sync.Mutex
	callTimes   []time.Time     // Track timing of each call
	returnTimes []time.Time     // Track when calls returned
	gapTimes    []time.Duration // Track gaps between return and next call
}

// NewInstrumentedFilesystem wraps a filesystem with instrumentation
func NewInstrumentedFilesystem(fs billy.Filesystem, logger *logging.KVLogger) *InstrumentedFilesystem {
	return &InstrumentedFilesystem{
		Filesystem: fs,
		logger:     logger,
	}
}

// ReadDir wraps the ReadDir call with detailed instrumentation
func (ifs *InstrumentedFilesystem) ReadDir(path string) ([]os.FileInfo, error) {
	start := time.Now()

	// Get or create tracker for this path
	trackerVal, _ := ifs.pathCalls.LoadOrStore(path, &PathCallTracker{
		firstCall:   start,
		callTimes:   make([]time.Time, 0, 100),
		returnTimes: make([]time.Time, 0, 100),
		gapTimes:    make([]time.Duration, 0, 100),
	})
	tracker := trackerVal.(*PathCallTracker)

	// Calculate gap since last return (this is the REAL gap)
	tracker.mu.Lock()
	var gapSinceLastReturn time.Duration
	if !tracker.lastReturn.IsZero() {
		gapSinceLastReturn = start.Sub(tracker.lastReturn)
		if len(tracker.gapTimes) < 200 {
			tracker.gapTimes = append(tracker.gapTimes, gapSinceLastReturn)
		}
	}
	tracker.mu.Unlock()

	// Record this call
	callNum := tracker.callCount.Add(1)
	tracker.mu.Lock()
	tracker.lastCall = start
	if len(tracker.callTimes) < 200 {
		tracker.callTimes = append(tracker.callTimes, start)
	}
	tracker.mu.Unlock()

	// Log first few calls and detect rapid loops
	if callNum <= 10 {
		ifs.logger.Info("ReadDir call",
			"path", path,
			"call_number", callNum,
			"time_since_first_ms", start.Sub(tracker.firstCall).Milliseconds())
	}

	// Detect rapid repeated calls (potential infinite loop)
	if callNum > 100 && callNum%100 == 0 {
		tracker.mu.Lock()
		recentCalls := len(tracker.callTimes)
		var avgGap time.Duration
		if recentCalls > 1 {
			totalGap := tracker.callTimes[recentCalls-1].Sub(tracker.callTimes[0])
			avgGap = totalGap / time.Duration(recentCalls-1)
		}
		tracker.mu.Unlock()

		ifs.logger.Info("ReadDir loop detected",
			"path", path,
			"total_calls", callNum,
			"avg_gap_ms", avgGap.Milliseconds(),
			"duration_s", start.Sub(tracker.firstCall).Seconds())
	}

	// Call the underlying filesystem
	fsStart := time.Now()
	entries, err := ifs.Filesystem.ReadDir(path)
	fsDuration := time.Since(fsStart)

	totalDuration := time.Since(start)
	overheadDuration := totalDuration - fsDuration
	returnTime := time.Now()

	// Record return time
	tracker.mu.Lock()
	tracker.lastReturn = returnTime
	if len(tracker.returnTimes) < 200 {
		tracker.returnTimes = append(tracker.returnTimes, returnTime)
	}
	tracker.mu.Unlock()

	// Log timing breakdown for slow calls or first few
	if totalDuration > 10*time.Millisecond || callNum <= 5 {
		ifs.logger.Info("ReadDir timing",
			"path", path,
			"call_number", callNum,
			"total_ms", totalDuration.Milliseconds(),
			"fs_ms", fsDuration.Milliseconds(),
			"overhead_ms", overheadDuration.Milliseconds(),
			"gap_since_last_return_ms", gapSinceLastReturn.Milliseconds(),
			"entries", len(entries),
			"error", err != nil)
	}

	// Log if gap is suspiciously long
	if gapSinceLastReturn > 50*time.Millisecond {
		ifs.logger.Info("Long gap detected",
			"path", path,
			"call_number", callNum,
			"gap_ms", gapSinceLastReturn.Milliseconds())
	}

	// Record metrics
	metrics.GetGlobalCounters().RecordPathCall(path)

	return entries, err
}

// GetPathStats returns statistics for a specific path
func (ifs *InstrumentedFilesystem) GetPathStats(path string) map[string]interface{} {
	trackerVal, ok := ifs.pathCalls.Load(path)
	if !ok {
		return map[string]interface{}{
			"calls": 0,
		}
	}

	tracker := trackerVal.(*PathCallTracker)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	callCount := tracker.callCount.Load()
	duration := tracker.lastCall.Sub(tracker.firstCall)

	var avgCallGap time.Duration
	if len(tracker.callTimes) > 1 {
		totalGap := tracker.callTimes[len(tracker.callTimes)-1].Sub(tracker.callTimes[0])
		avgCallGap = totalGap / time.Duration(len(tracker.callTimes)-1)
	}

	var avgReturnGap time.Duration
	var maxReturnGap time.Duration
	if len(tracker.gapTimes) > 0 {
		var total time.Duration
		for _, gap := range tracker.gapTimes {
			total += gap
			if gap > maxReturnGap {
				maxReturnGap = gap
			}
		}
		avgReturnGap = total / time.Duration(len(tracker.gapTimes))
	}

	return map[string]interface{}{
		"total_calls":       callCount,
		"duration_seconds":  duration.Seconds(),
		"avg_call_gap_us":   avgCallGap.Microseconds(),
		"avg_return_gap_us": avgReturnGap.Microseconds(),
		"max_return_gap_us": maxReturnGap.Microseconds(),
		"calls_per_second":  float64(callCount) / duration.Seconds(),
		"gaps_measured":     len(tracker.gapTimes),
	}
}

// GetAllPathStats returns statistics for all paths
func (ifs *InstrumentedFilesystem) GetAllPathStats() map[string]interface{} {
	stats := make(map[string]interface{})

	ifs.pathCalls.Range(func(key, value interface{}) bool {
		path := key.(string)
		stats[path] = ifs.GetPathStats(path)
		return true
	})

	return stats
}

// FSStat forwards dynamic filesystem capacity data through instrumentation.
func (ifs *InstrumentedFilesystem) FSStat(ctx context.Context, stat *gonfs.FSStat) error {
	return fsStatFrom(ctx, ifs.Filesystem, stat)
}

// Chmod changes the mode of the named file (implements billy.Change)
func (ifs *InstrumentedFilesystem) Chmod(name string, mode os.FileMode) error {
	if c, ok := ifs.Filesystem.(billy.Change); ok {
		return c.Chmod(name, mode)
	}
	return nil
}

// Lchown changes the uid and gid of the named file (implements billy.Change)
func (ifs *InstrumentedFilesystem) Lchown(name string, uid, gid int) error {
	if c, ok := ifs.Filesystem.(billy.Change); ok {
		return c.Lchown(name, uid, gid)
	}
	return nil
}

// Chown changes the uid and gid of the named file (implements billy.Change)
func (ifs *InstrumentedFilesystem) Chown(name string, uid, gid int) error {
	if c, ok := ifs.Filesystem.(billy.Change); ok {
		return c.Chown(name, uid, gid)
	}
	return nil
}

// Chtimes changes the access and modification times (implements billy.Change)
func (ifs *InstrumentedFilesystem) Chtimes(name string, atime time.Time, mtime time.Time) error {
	if c, ok := ifs.Filesystem.(billy.Change); ok {
		return c.Chtimes(name, atime, mtime)
	}
	return nil
}

// Made with Bob
