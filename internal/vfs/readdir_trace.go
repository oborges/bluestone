package vfs

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ReaddirTrace tracks detailed information about READDIR operations
type ReaddirTrace struct {
	mu              sync.Mutex
	traces          map[string]*PathTrace
	enabled         atomic.Bool
	maxTraceEntries int
	maxTracedPaths  int
}

// PathTrace tracks READDIR calls for a specific path
type PathTrace struct {
	Path            string
	FirstCall       time.Time
	LastCall        time.Time
	TotalCalls      int64
	Requests        []ReaddirRequest
	UniqueResults   map[int]int // map[entries_returned]count
	ResultSequence  []int       // sequence of entry counts returned
}

// ReaddirRequest represents a single READDIR request
type ReaddirRequest struct {
	Index           int
	Timestamp       time.Time
	EntriesReturned int
	Duration        time.Duration
	Error           error
}

var globalTrace = &ReaddirTrace{
	traces:          make(map[string]*PathTrace),
	maxTraceEntries: 200, // Only keep first 200 requests per path
	maxTracedPaths:  1000, // Max distinct paths to prevent memory-amplification OOM
}

// EnableTracing enables READDIR tracing
func EnableTracing() {
	globalTrace.enabled.Store(true)
}

// DisableTracing disables READDIR tracing
func DisableTracing() {
	globalTrace.enabled.Store(false)
}

// IsTracingEnabled returns whether tracing is enabled
func IsTracingEnabled() bool {
	return globalTrace.enabled.Load()
}

// RecordReaddirCall records a READDIR call
func RecordReaddirCall(path string, entriesReturned int, duration time.Duration, err error) {
	if !globalTrace.enabled.Load() {
		return
	}

	globalTrace.mu.Lock()
	defer globalTrace.mu.Unlock()

	trace, exists := globalTrace.traces[path]
	if !exists {
		// Evict random trace if at capacity to prevent memory-amplification OOM
		if len(globalTrace.traces) >= globalTrace.maxTracedPaths {
			for k := range globalTrace.traces {
				delete(globalTrace.traces, k)
				break
			}
		}

		trace = &PathTrace{
			Path:           path,
			FirstCall:      time.Now(),
			UniqueResults:  make(map[int]int),
			ResultSequence: make([]int, 0, globalTrace.maxTraceEntries),
		}
		globalTrace.traces[path] = trace
	}

	trace.LastCall = time.Now()
	trace.TotalCalls++

	// Only keep first N requests for detailed analysis
	if len(trace.Requests) < globalTrace.maxTraceEntries {
		trace.Requests = append(trace.Requests, ReaddirRequest{
			Index:           int(trace.TotalCalls),
			Timestamp:       time.Now(),
			EntriesReturned: entriesReturned,
			Duration:        duration,
			Error:           err,
		})
	}

	// Track unique result counts
	trace.UniqueResults[entriesReturned]++
	
	// Track result sequence (only first N)
	if len(trace.ResultSequence) < globalTrace.maxTraceEntries {
		trace.ResultSequence = append(trace.ResultSequence, entriesReturned)
	}
}

// GetTrace returns the trace for a specific path
func GetTrace(path string) *PathTrace {
	globalTrace.mu.Lock()
	defer globalTrace.mu.Unlock()
	return globalTrace.traces[path]
}

// GetAllTraces returns all traces
func GetAllTraces() map[string]*PathTrace {
	globalTrace.mu.Lock()
	defer globalTrace.mu.Unlock()
	
	// Return a copy to avoid race conditions
	result := make(map[string]*PathTrace)
	for k, v := range globalTrace.traces {
		result[k] = v
	}
	return result
}

// ClearTraces clears all traces
func ClearTraces() {
	globalTrace.mu.Lock()
	defer globalTrace.mu.Unlock()
	globalTrace.traces = make(map[string]*PathTrace)
}

// AnalyzeTrace analyzes a path trace and returns findings
func (pt *PathTrace) Analyze() map[string]interface{} {
	if pt == nil {
		return map[string]interface{}{"error": "no trace data"}
	}

	duration := pt.LastCall.Sub(pt.FirstCall)
	
	// Find most common result count
	var mostCommonCount, mostCommonFreq int
	for count, freq := range pt.UniqueResults {
		if freq > mostCommonFreq {
			mostCommonCount = count
			mostCommonFreq = freq
		}
	}

	// Detect loops: check if same count repeats consecutively
	var maxConsecutive int
	var currentConsecutive int
	var lastCount int = -1
	
	for _, count := range pt.ResultSequence {
		if count == lastCount {
			currentConsecutive++
			if currentConsecutive > maxConsecutive {
				maxConsecutive = currentConsecutive
			}
		} else {
			currentConsecutive = 1
			lastCount = count
		}
	}

	// Check if we're returning the same count repeatedly (potential pagination issue)
	isLikelyLoop := maxConsecutive > 10

	// Calculate average entries per call
	var totalEntries int64
	for count, freq := range pt.UniqueResults {
		totalEntries += int64(count * freq)
	}
	avgEntries := float64(totalEntries) / float64(pt.TotalCalls)

	analysis := map[string]interface{}{
		"path":                    pt.Path,
		"total_calls":             pt.TotalCalls,
		"duration_seconds":        duration.Seconds(),
		"calls_per_second":        float64(pt.TotalCalls) / duration.Seconds(),
		"unique_result_counts":    len(pt.UniqueResults),
		"most_common_count":       mostCommonCount,
		"most_common_frequency":   mostCommonFreq,
		"max_consecutive_same":    maxConsecutive,
		"likely_pagination_loop":  isLikelyLoop,
		"avg_entries_per_call":    avgEntries,
		"result_count_histogram":  pt.UniqueResults,
		"first_20_results":        pt.getFirstN(20),
	}

	// Add diagnosis
	if isLikelyLoop {
		analysis["diagnosis"] = fmt.Sprintf("PAGINATION LOOP DETECTED: Same count (%d) returned %d times consecutively", mostCommonCount, maxConsecutive)
	} else if len(pt.UniqueResults) == 1 && mostCommonCount > 0 {
		analysis["diagnosis"] = fmt.Sprintf("CONSISTENT RESULTS: Always returning %d entries (expected for complete directory reads)", mostCommonCount)
	} else if len(pt.UniqueResults) > 10 {
		analysis["diagnosis"] = "VARIABLE RESULTS: Entry count varies significantly (may indicate pagination or caching issues)"
	} else {
		analysis["diagnosis"] = "NORMAL: Result pattern appears reasonable"
	}

	return analysis
}

// getFirstN returns the first N results from the sequence
func (pt *PathTrace) getFirstN(n int) []int {
	if len(pt.ResultSequence) <= n {
		return pt.ResultSequence
	}
	return pt.ResultSequence[:n]
}

// PrintTrace prints a human-readable trace
func (pt *PathTrace) PrintTrace(w io.Writer) {
	if pt == nil {
		fmt.Fprintf(w, "No trace data\n")
		return
	}

	fmt.Fprintf(w, "\n=== READDIR TRACE: %s ===\n", pt.Path)
	fmt.Fprintf(w, "Total Calls: %d\n", pt.TotalCalls)
	fmt.Fprintf(w, "Duration: %.2fs\n", pt.LastCall.Sub(pt.FirstCall).Seconds())
	fmt.Fprintf(w, "Calls/sec: %.1f\n\n", float64(pt.TotalCalls)/pt.LastCall.Sub(pt.FirstCall).Seconds())

	fmt.Fprintf(w, "First 50 Requests:\n")
	fmt.Fprintf(w, "%-6s %-12s %-10s %-10s %s\n", "Index", "Time(ms)", "Entries", "Duration", "Error")
	fmt.Fprintf(w, "%s\n", "-------------------------------------------------------------------")
	
	limit := 50
	if len(pt.Requests) < limit {
		limit = len(pt.Requests)
	}
	
	for i := 0; i < limit; i++ {
		req := pt.Requests[i]
		relTime := req.Timestamp.Sub(pt.FirstCall).Milliseconds()
		errStr := "nil"
		if req.Error != nil {
			errStr = req.Error.Error()
		}
		fmt.Fprintf(w, "%-6d %-12d %-10d %-10s %s\n",
			req.Index,
			relTime,
			req.EntriesReturned,
			fmt.Sprintf("%.2fms", req.Duration.Seconds()*1000),
			errStr)
	}

	if len(pt.Requests) > limit {
		fmt.Fprintf(w, "... (%d more requests)\n", len(pt.Requests)-limit)
	}

	fmt.Fprintf(w, "\nResult Count Histogram:\n")
	for count, freq := range pt.UniqueResults {
		fmt.Fprintf(w, "  %d entries: %d times (%.1f%%)\n",
			count, freq, float64(freq)/float64(pt.TotalCalls)*100)
	}

	analysis := pt.Analyze()
	fmt.Fprintf(w, "\nDiagnosis: %s\n", analysis["diagnosis"])
}

// Made with Bob
