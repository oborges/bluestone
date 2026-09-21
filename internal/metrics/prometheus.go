package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

var (
	// Filesystem request metrics, by the protocol that issued the request
	filesystemRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "filesystem_requests_total",
			Help: "Total filesystem requests by protocol, operation, and outcome",
		},
		[]string{"protocol", "operation", "status"},
	)

	filesystemRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "filesystem_request_duration_seconds",
			Help:    "Filesystem request duration in seconds by protocol and operation",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"protocol", "operation"},
	)

	// Deprecated: the same requests without a protocol label, kept so
	// existing dashboards keep working. Use filesystem_requests_total and
	// filesystem_request_duration_seconds.
	nfsRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nfs_requests_total",
			Help: "Deprecated: use filesystem_requests_total. Total filesystem requests by operation and outcome",
		},
		[]string{"operation", "status"},
	)

	nfsRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "nfs_request_duration_seconds",
			Help:    "Deprecated: use filesystem_request_duration_seconds. Filesystem request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"operation"},
	)

	// COS API metrics
	cosAPICallsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cos_api_calls_total",
			Help: "Total number of COS API calls",
		},
		[]string{"operation", "status"},
	)

	cosAPIDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cos_api_duration_seconds",
			Help:    "COS API call duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"operation"},
	)

	// Cache metrics
	cacheHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cache_hits_total",
			Help: "Total number of cache hits",
		},
		[]string{"cache_type"},
	)

	cacheMissesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cache_misses_total",
			Help: "Total number of cache misses",
		},
		[]string{"cache_type"},
	)

	cacheSizeBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cache_size_bytes",
			Help: "Current cache size in bytes",
		},
		[]string{"cache_type"},
	)

	cacheEvictionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cache_evictions_total",
			Help: "Total number of cache evictions",
		},
		[]string{"cache_type"},
	)

	// Data transfer metrics
	bytesReadTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "bytes_read_total",
			Help: "Total bytes read",
		},
	)

	bytesWrittenTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "bytes_written_total",
			Help: "Total bytes written",
		},
	)

	// Connection metrics
	activeConnections = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "active_connections",
			Help: "Number of active connections",
		},
	)

	// SMB metrics. Requests are counted per SMB2 command, so a compound
	// request counts once per command in it.
	smbRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "smb_requests_total",
			Help: "Total SMB requests by command and NT status",
		},
		[]string{"command", "status"},
	)

	// SMB client caching: leases and level II oplocks granted, and breaks
	// sent to take them back when a file changes.
	smbLeasesGranted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "smb_leases_granted_total",
			Help: "SMB leases and oplocks granted, by kind and state",
		},
		[]string{"kind", "state"},
	)
	smbLeaseBreaks = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "smb_lease_breaks_total",
			Help: "SMB lease and oplock breaks sent, by kind and the states broken from and to",
		},
		[]string{"kind", "from", "to"},
	)
	smbLeaseBreakTimeouts = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "smb_lease_break_timeouts_total",
			Help: "SMB lease breaks a client did not acknowledge in time",
		},
	)

	smbRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "smb_request_duration_seconds",
			Help:    "SMB request duration in seconds by command",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"command"},
	)

	smbConnections = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "smb_connections",
			Help: "Number of SMB client connections",
		},
	)

	smbSessions = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "smb_sessions",
			Help: "Number of authenticated SMB sessions",
		},
	)

	smbConnectionsRefusedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "smb_connections_refused_total",
			Help: "SMB connections refused, by which limit refused them",
		},
		[]string{"reason"},
	)

	smbAuthFailuresTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "smb_auth_failures_total",
			Help: "Failed SMB authentication attempts",
		},
	)

	smbAuthBlockedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "smb_auth_blocked_total",
			Help: "SMB authentication attempts refused because the client is blocked after repeated failures",
		},
	)

	smbAuthBlockedClients = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "smb_auth_blocked_clients",
			Help: "Client addresses currently blocked from authenticating",
		},
	)

	smbOpenFiles = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "smb_open_files",
			Help: "Number of files SMB clients hold open",
		},
	)

	// Lock metrics
	activeLocksTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "active_locks_total",
			Help: "Number of active file locks",
		},
	)

	// Staging sync metrics
	stagingSyncQueueDepth = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "staging_sync_queue_depth",
			Help: "Current number of dirty files waiting for staging sync",
		},
	)

	stagingSyncQueueBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "staging_sync_queue_bytes",
			Help: "Current total bytes waiting for staging sync",
		},
	)

	syncQueueBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "sync_queue_bytes",
			Help: "Current total bytes waiting for staging sync",
		},
	)

	stagingSyncOldestDirtyAge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "staging_sync_oldest_dirty_age_seconds",
			Help: "Age in seconds of the oldest dirty file waiting for staging sync",
		},
	)

	stagingCOSVisibilityLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "staging_cos_visibility_latency_seconds",
			Help:    "Seconds from first dirty mark until the synced file is visible in COS after upload completion",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
		},
	)

	stagingUploadDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "staging_upload_duration_seconds",
			Help:    "Wall-clock seconds spent uploading staged file bytes to COS",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		},
	)

	stagingUploadThroughputMiB = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "staging_upload_throughput_mib_per_second",
			Help:    "Observed staged upload throughput in MiB/s for successful COS uploads",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 500},
		},
	)

	stagingUsedBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "staging_used_bytes",
			Help: "Current bytes held by staging sessions",
		},
	)

	stagingAvailableBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "staging_available_bytes",
			Help: "Current bytes safely available for staging writes",
		},
	)

	stagingPressureLevel = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "staging_pressure_level",
			Help: "Current staging pressure level: 0=normal, 1=high, 2=critical",
		},
	)

	stagingConflictCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "staging_conflict_count",
			Help: "Current number of unresolved staging conflicts",
		},
	)

	stagingConflictsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "staging_conflicts_total",
			Help: "Total staging conflicts recorded after external object changes",
		},
	)

	stagingLastConflictTimestampSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "staging_last_conflict_timestamp_seconds",
			Help: "Unix timestamp for the most recent staging conflict, or 0 when none are recorded",
		},
	)

	writesBlockedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "writes_blocked_total",
			Help: "Total staging writes that entered backpressure blocking",
		},
	)

	writesRejectedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "writes_rejected_total",
			Help: "Total staging writes rejected by backpressure",
		},
	)

	backpressureWaitSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "backpressure_wait_seconds",
			Help:    "Seconds spent waiting for staging backpressure to clear",
			Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		},
	)

	objectRefreshScansTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "object_refresh_scans_total",
			Help: "Total number of object-side refresh scans",
		},
		[]string{"status"},
	)

	objectRefreshDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "object_refresh_duration_seconds",
			Help:    "Wall-clock seconds spent scanning COS for object-side changes",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		},
	)

	objectRefreshObjectsChangedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "object_refresh_objects_changed_total",
			Help: "Total number of created, updated, or deleted COS objects seen by refresh scans",
		},
	)

	objectRefreshCacheInvalidationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "object_refresh_cache_invalidations_total",
			Help: "Total number of cache invalidations caused by object-side refresh",
		},
		[]string{"cache_type"},
	)

	objectRefreshSkippedDirtyPathsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "object_refresh_skipped_dirty_paths_total",
			Help: "Total number of dirty staged paths skipped by object-side refresh",
		},
	)

	objectRefreshConflictsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "object_refresh_conflicts_total",
			Help: "Total number of object-side refresh changes that produced staging conflicts",
		},
	)
)

// Initialize registers all metrics with Prometheus
func Initialize() {
	prometheus.MustRegister(
		filesystemRequestsTotal,
		filesystemRequestDuration,
		nfsRequestsTotal,
		nfsRequestDuration,
		cosAPICallsTotal,
		cosAPIDuration,
		cacheHitsTotal,
		cacheMissesTotal,
		cacheSizeBytes,
		cacheEvictionsTotal,
		bytesReadTotal,
		bytesWrittenTotal,
		activeConnections,
		smbRequestsTotal,
		smbLeasesGranted,
		smbLeaseBreaks,
		smbLeaseBreakTimeouts,
		smbRequestDuration,
		smbConnections,
		smbSessions,
		smbOpenFiles,
		smbConnectionsRefusedTotal,
		smbAuthFailuresTotal,
		smbAuthBlockedTotal,
		smbAuthBlockedClients,
		activeLocksTotal,
		stagingSyncQueueDepth,
		stagingSyncQueueBytes,
		syncQueueBytes,
		stagingSyncOldestDirtyAge,
		stagingCOSVisibilityLatency,
		stagingUploadDuration,
		stagingUploadThroughputMiB,
		stagingUsedBytes,
		stagingAvailableBytes,
		stagingPressureLevel,
		stagingConflictCount,
		stagingConflictsTotal,
		stagingLastConflictTimestampSeconds,
		writesBlockedTotal,
		writesRejectedTotal,
		backpressureWaitSeconds,
		objectRefreshScansTotal,
		objectRefreshDuration,
		objectRefreshObjectsChangedTotal,
		objectRefreshCacheInvalidationsTotal,
		objectRefreshSkippedDirtyPathsTotal,
		objectRefreshConflictsTotal,
	)

	logging.Info("Metrics initialized")
}

// StartMetricsServer starts the Prometheus metrics HTTP server
func StartMetricsServer(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	logging.Info("Starting metrics server", zap.String("addr", addr))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       15 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil {
			logging.Error("Metrics server failed", zap.Error(err))
		}
	}()

	return nil
}

// Request outcome labels.
const (
	StatusSuccess  = "success"
	StatusNotFound = "not_found"
	StatusError    = "error"
)

// RecordRequest records a filesystem request under the protocol carried by
// ctx (see WithProtocol); err, the request's result, sets the status.
func RecordRequest(ctx context.Context, operation string, err error, duration time.Duration) {
	status := RequestStatus(err)
	protocol := ProtocolFrom(ctx)
	filesystemRequestsTotal.WithLabelValues(protocol, operation, status).Inc()
	filesystemRequestDuration.WithLabelValues(protocol, operation).Observe(duration.Seconds())
	nfsRequestsTotal.WithLabelValues(operation, status).Inc()
	nfsRequestDuration.WithLabelValues(operation).Observe(duration.Seconds())
}

// RequestStatus classifies a request result for the status label.
func RequestStatus(err error) string {
	switch {
	case err == nil:
		return StatusSuccess
	case errors.Is(err, os.ErrNotExist):
		return StatusNotFound
	default:
		return StatusError
	}
}

// RecordSMBRequest records one SMB request: command is the SMB2 command
// name, status the NT status returned, such as "STATUS_SUCCESS".
func RecordSMBRequest(command, status string, duration time.Duration) {
	smbRequestsTotal.WithLabelValues(command, status).Inc()
	smbRequestDuration.WithLabelValues(command).Observe(duration.Seconds())
}

// RecordSMBLeaseGranted counts caching granted to an SMB open.
func RecordSMBLeaseGranted(kind, state string) {
	smbLeasesGranted.WithLabelValues(kind, state).Inc()
}

// RecordSMBLeaseBreak counts a break sent to an SMB client.
func RecordSMBLeaseBreak(kind, from, to string) {
	smbLeaseBreaks.WithLabelValues(kind, from, to).Inc()
}

// RecordSMBLeaseBreakTimeout counts a break a client never acknowledged.
func RecordSMBLeaseBreakTimeout() {
	smbLeaseBreakTimeouts.Inc()
}

// SetSMBConnections records how many SMB connections are open.
func SetSMBConnections(count int) {
	smbConnections.Set(float64(count))
}

// SetSMBSessions records how many authenticated SMB sessions exist.
func SetSMBSessions(count int) {
	smbSessions.Set(float64(count))
}

// SetSMBOpenFiles records how many files SMB clients hold open.
func SetSMBOpenFiles(count int) {
	smbOpenFiles.Set(float64(count))
}

// RecordSMBConnectionRefused records a connection refused by a limit, named
// by the limit that refused it.
func RecordSMBConnectionRefused(reason string) {
	smbConnectionsRefusedTotal.WithLabelValues(reason).Inc()
}

// RecordSMBAuthFailure records a failed SMB authentication attempt.
func RecordSMBAuthFailure() {
	smbAuthFailuresTotal.Inc()
}

// RecordSMBAuthBlocked records an attempt refused because the client is
// blocked after repeated failures.
func RecordSMBAuthBlocked() {
	smbAuthBlockedTotal.Inc()
}

// SetSMBAuthBlockedClients records how many client addresses are blocked.
func SetSMBAuthBlockedClients(count int) {
	smbAuthBlockedClients.Set(float64(count))
}

// RecordCOSAPICall records a COS API call
func RecordCOSAPICall(operation, status string, duration time.Duration) {
	cosAPICallsTotal.WithLabelValues(operation, status).Inc()
	cosAPIDuration.WithLabelValues(operation).Observe(duration.Seconds())
}

// RecordCacheHit records a cache hit
func RecordCacheHit(cacheType string) {
	cacheHitsTotal.WithLabelValues(cacheType).Inc()
}

// RecordCacheMiss records a cache miss
func RecordCacheMiss(cacheType string) {
	cacheMissesTotal.WithLabelValues(cacheType).Inc()
}

// SetCacheSize sets the current cache size
func SetCacheSize(cacheType string, size int64) {
	cacheSizeBytes.WithLabelValues(cacheType).Set(float64(size))
}

// RecordCacheEviction records a cache eviction
func RecordCacheEviction(cacheType string) {
	cacheEvictionsTotal.WithLabelValues(cacheType).Inc()
}

// RecordObjectRefreshScanPrometheus records a refresh scan outcome and duration.
func RecordObjectRefreshScanPrometheus(status string, duration time.Duration) {
	objectRefreshScansTotal.WithLabelValues(status).Inc()
	objectRefreshDuration.Observe(duration.Seconds())
}

// RecordObjectRefreshObjectsChangedPrometheus records changed objects seen by refresh.
func RecordObjectRefreshObjectsChangedPrometheus(count int) {
	if count > 0 {
		objectRefreshObjectsChangedTotal.Add(float64(count))
	}
	RecordObjectRefreshObjectsChanged(count)
}

// RecordObjectRefreshCacheInvalidationsPrometheus records refresh cache invalidations.
func RecordObjectRefreshCacheInvalidationsPrometheus(cacheType string, count int) {
	if count > 0 {
		objectRefreshCacheInvalidationsTotal.WithLabelValues(cacheType).Add(float64(count))
	}
	RecordObjectRefreshCacheInvalidations(count)
}

// RecordObjectRefreshSkippedDirtyPathsPrometheus records dirty paths skipped by refresh.
func RecordObjectRefreshSkippedDirtyPathsPrometheus(count int) {
	if count > 0 {
		objectRefreshSkippedDirtyPathsTotal.Add(float64(count))
	}
	RecordObjectRefreshSkippedDirtyPaths(count)
}

// RecordObjectRefreshConflictsPrometheus records refresh-detected staging conflicts.
func RecordObjectRefreshConflictsPrometheus(count int) {
	if count > 0 {
		objectRefreshConflictsTotal.Add(float64(count))
	}
	RecordObjectRefreshConflicts(count)
}

// RecordBytesRead records bytes read
func RecordBytesRead(bytes int64) {
	bytesReadTotal.Add(float64(bytes))
	RecordBytesReadCounter(bytes)
}

// RecordBytesWritten records bytes written
func RecordBytesWritten(bytes int64) {
	bytesWrittenTotal.Add(float64(bytes))
	RecordBytesWrittenCounter(bytes)
}

// SetActiveConnections sets the number of active connections
func SetActiveConnections(count int) {
	activeConnections.Set(float64(count))
}

// SetActiveLocks sets the number of active locks
func SetActiveLocks(count int) {
	activeLocksTotal.Set(float64(count))
}

// SetStagingSyncQueue records the current staging sync queue state.
func SetStagingSyncQueue(depth int, bytes int64, oldestAge time.Duration) {
	stagingSyncQueueDepth.Set(float64(depth))
	stagingSyncQueueBytes.Set(float64(bytes))
	syncQueueBytes.Set(float64(bytes))
	if depth == 0 {
		stagingSyncOldestDirtyAge.Set(0)
		return
	}
	stagingSyncOldestDirtyAge.Set(oldestAge.Seconds())
}

// RecordStagingUpload records successful staged upload timing and throughput.
func RecordStagingUpload(sizeBytes int64, uploadDuration, visibilityLatency time.Duration) {
	if uploadDuration > 0 {
		stagingUploadDuration.Observe(uploadDuration.Seconds())
		mib := float64(sizeBytes) / (1024 * 1024)
		stagingUploadThroughputMiB.Observe(mib / uploadDuration.Seconds())
	}
	if visibilityLatency > 0 {
		stagingCOSVisibilityLatency.Observe(visibilityLatency.Seconds())
	}
}

// SetStagingPressure records current staging pressure gauges.
func SetStagingPressure(usedBytes, availableBytes int64, pressureLevel string) {
	stagingUsedBytes.Set(float64(usedBytes))
	stagingAvailableBytes.Set(float64(availableBytes))
	stagingPressureLevel.Set(float64(pressureLevelValue(pressureLevel)))
}

// RecordStagingConflict increments the total conflict counter.
func RecordStagingConflict() {
	stagingConflictsTotal.Inc()
}

// SetStagingConflicts records unresolved staging conflict state.
func SetStagingConflicts(count int, lastConflict time.Time) {
	stagingConflictCount.Set(float64(count))
	if lastConflict.IsZero() {
		stagingLastConflictTimestampSeconds.Set(0)
		return
	}
	stagingLastConflictTimestampSeconds.Set(float64(lastConflict.Unix()))
}

// RecordBackpressureBlocked records that a write entered backpressure wait.
func RecordBackpressureBlocked() {
	writesBlockedTotal.Inc()
}

// RecordBackpressureRejected records that a write was rejected by backpressure.
func RecordBackpressureRejected() {
	writesRejectedTotal.Inc()
}

// RecordBackpressureWait records time spent waiting for staging pressure relief.
func RecordBackpressureWait(wait time.Duration) {
	if wait > 0 {
		backpressureWaitSeconds.Observe(wait.Seconds())
	}
}

func pressureLevelValue(level string) int {
	switch level {
	case "critical":
		return 2
	case "high":
		return 1
	default:
		return 0
	}
}

// Made with Bob
