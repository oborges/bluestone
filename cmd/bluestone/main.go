package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oborges/bluestone/internal/cache"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/cos"
	"github.com/oborges/bluestone/internal/ha"
	"github.com/oborges/bluestone/internal/dashboard"
	"github.com/oborges/bluestone/internal/feature"
	"github.com/oborges/bluestone/internal/health"
	"github.com/oborges/bluestone/internal/lock"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/metrics"
	"github.com/oborges/bluestone/internal/nfs"
	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/internal/staging"
	"github.com/oborges/bluestone/internal/vfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
	"go.uber.org/zap"
)

var (
	// Version is set during build
	Version = "dev"

	// Command line flags
	configPath = flag.String("config", "", "Path to configuration file")
	version    = flag.Bool("version", false, "Print version and exit")
)

func main() {
	flag.Parse()

	// Print version and exit
	if *version {
		fmt.Printf("Bluestone v%s\n", Version)
		os.Exit(0)
	}

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize logging
	logConfig := logging.Config{
		Level:  cfg.Logging.Level,
		Format: cfg.Logging.Format,
		Output: cfg.Logging.Output,
	}
	if err := logging.Initialize(logConfig); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logging: %v\n", err)
		os.Exit(1)
	}
	defer logging.Sync()

	logging.Info("Starting Bluestone",
		zap.String("version", Version),
	)
	for _, notice := range cfg.Notices {
		logging.Warn(notice)
	}

	// Log effective configuration
	logging.Info("Configuration loaded",
		zap.Int("write_buffer_kb", cfg.Performance.WriteBufferKB),
		zap.Int("write_buffer_bytes", cfg.Performance.WriteBufferKB*1024),
		zap.Int("multipart_threshold_mb", cfg.Performance.MultipartThresholdMB),
		zap.Int("multipart_chunk_mb", cfg.Performance.MultipartChunkMB),
		zap.Int("read_ahead_kb", cfg.Performance.ReadAheadKB),
		zap.Int("max_full_object_read_mb", cfg.Performance.MaxFullObjectReadMB),
		zap.Int("max_buffered_write_mb", cfg.Performance.MaxBufferedWriteMB),
		zap.Int("max_directory_entries", cfg.Performance.MaxDirectoryEntries),
		zap.String("nfs_version", cfg.Server.NFSVersion),
		zap.Bool("data_cache_enabled", cfg.Cache.Data.Enabled),
		zap.Bool("metadata_cache_enabled", cfg.Cache.Metadata.Enabled),
		zap.Int("data_cache_size_gb", cfg.Cache.Data.SizeGB),
		zap.Int("metadata_cache_size_mb", cfg.Cache.Metadata.SizeMB),
		zap.Bool("object_refresh_enabled", cfg.ObjectRefresh.Enabled),
		zap.String("object_refresh_interval", cfg.ObjectRefresh.Interval),
		zap.String("object_refresh_prefix", cfg.ObjectRefresh.Prefix),
		zap.String("log_level", cfg.Logging.Level),
	)

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx // Used for future context-aware operations

	// Initialize COS client
	cosClient, err := cos.NewClient(&cfg.COS)
	if err != nil {
		logging.Fatal("Failed to initialize COS client", zap.Error(err))
	}
	defer cosClient.Close()

	logging.Info("COS client initialized successfully")

	// HA fencing: exactly one gateway may serve this bucket.
	if cfg.HA.Enabled {
		heartbeat, _ := cfg.HA.GetHeartbeatInterval()
		leaseTimeout, _ := cfg.HA.GetLeaseTimeout()
		leaseManager, err := ha.Acquire(context.Background(), ha.Options{
			Store:             cosClient,
			HeartbeatInterval: heartbeat,
			LeaseTimeout:      leaseTimeout,
			HolderMarkerDir:   cfg.Staging.RootDir,
			ForceTakeover:     cfg.HA.ForceTakeover,
		})
		if err != nil {
			logging.Fatal("HA lease acquisition failed", zap.Error(err))
		}
		defer leaseManager.Release()
	}

	// Initialize caches
	metadataCache := cache.NewMetadataCache(&cfg.Cache.Metadata)
	dataCache, err := cache.NewDataCache(&cfg.Cache.Data)
	if err != nil {
		logging.Fatal("Failed to initialize data cache", zap.Error(err))
	}

	logging.Info("Caches initialized successfully")

	// Initialize POSIX operations handler
	operations := posix.NewOperationsHandler(cosClient, metadataCache, dataCache, &cfg.Performance)

	// Initialize lock manager
	lockManager := lock.NewManager(5 * time.Minute)
	defer lockManager.Close()

	// Initialize metrics
	metrics.Initialize()
	if cfg.Server.MetricsEnabled {
		if err := metrics.StartMetricsServer(cfg.Server.MetricsPort); err != nil {
			logging.Error("Failed to start metrics server", zap.Error(err))
		}
	} else {
		logging.Info("Metrics server disabled by configuration")
	}

	// Initialize health checks
	healthChecker := health.NewChecker()
	healthChecker.RegisterCheck("cos", health.COSHealthCheck(func(ctx context.Context) error {
		_, err := cosClient.ObjectExists(ctx, ".health")
		return err
	}))
	healthChecker.RegisterCheck("cache", health.CacheHealthCheck(
		metadataCache.IsEnabled,
		func() interface{} { return metadataCache.Stats() },
	))

	if cfg.Server.HealthEnabled {
		if err := health.StartHealthServer(cfg.Server.HealthPort, healthChecker); err != nil {
			logging.Error("Failed to start health server", zap.Error(err))
		}
	} else {
		logging.Info("Health server disabled by configuration")
	}

	// Initialize debug server
	if cfg.Server.DebugEnabled {
		debugAddr := fmt.Sprintf("127.0.0.1:%d", cfg.Server.DebugPort)
		logging.Info("Starting debug pprof server", zap.String("addr", debugAddr))
		srv := &http.Server{
			Addr:              debugAddr,
			Handler:           nil, // Uses DefaultServeMux internally cleanly mapping explicitly
			ReadTimeout:       5 * time.Second,
			ReadHeaderTimeout: 3 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       15 * time.Second,
		}

		go func() {
			if err := srv.ListenAndServe(); err != nil {
				logging.Error("Debug server failed", zap.Error(err))
			}
		}()
	} else {
		logging.Info("Debug server disabled by configuration")
	}

	// Initialize feature flags
	featureFlags := feature.LoadFeatureFlags(cfg)
	logging.Info("Feature flags loaded",
		zap.Bool("staging_enabled", featureFlags.IsStagingEnabled()))

	// Initialize staging components if enabled
	var stagingManager *staging.StagingManager
	var syncWorker *staging.SyncWorker

	if featureFlags.IsStagingEnabled() {
		logging.Info("Initializing staging architecture",
			zap.String("root_dir", cfg.Staging.RootDir),
			zap.Int64("sync_threshold_mb", cfg.Staging.SyncThresholdMB),
			zap.Int("sync_worker_count", cfg.Staging.SyncWorkerCount))

		// Create staging manager
		stagingManager, err = staging.NewStagingManager(&cfg.Staging)
		if err != nil {
			logging.Fatal("Failed to initialize staging manager", zap.Error(err))
		}
		defer stagingManager.Shutdown()

		// Create COS client adapter for sync worker
		cosClientAdapter := &staging.COSClientAdapter{Client: cosClient}

		// Create sync worker
		syncWorker = staging.NewSyncWorker(stagingManager, cosClientAdapter, &cfg.Staging)

		// Sync uploads and deferred deletes bypass the operations handler, so
		// invalidate its caches whenever the worker mutates a COS object;
		// otherwise state cached during the dirty window (e.g. a zero-byte
		// truncate object) outlives the sync and serves stale reads. Deletes
		// change the namespace and purge ancestor listings; uploads do not,
		// so they use the narrower invalidation.
		syncWorker.SetObjectMutatedCallback(operations.InvalidateFileMutation)
		syncWorker.SetObjectSyncedCallback(operations.InvalidateObjectAfterSync)

		// Start sync worker
		syncWorker.Start()
		defer syncWorker.Stop()

		logging.Info("Staging architecture initialized successfully")
	}

	if cfg.ObjectRefresh.Enabled {
		refreshInterval, err := cfg.ObjectRefresh.GetInterval()
		if err != nil {
			logging.Fatal("Invalid object refresh interval", zap.Error(err))
		}
		var dirtyChecker posix.DirtyPathChecker
		var conflictRecorder posix.ConflictRecorder
		if stagingManager != nil {
			dirtyChecker = stagingManager.IsDirty
			conflictRecorder = func(conflict posix.ObjectConflict) error {
				_, err := stagingManager.RecordConflict(conflict.Path, staging.ExternalChangeSnapshot{
					ObjectKey:    conflict.Key,
					Size:         conflict.Size,
					ETag:         conflict.ETag,
					LastModified: conflict.LastModified,
					Deleted:      conflict.Deleted,
					Reason:       "object_refresh_external_change",
				})
				return err
			}
		}
		refreshScanner := posix.NewObjectRefreshScanner(operations, &cfg.ObjectRefresh, dirtyChecker, conflictRecorder)
		refreshScanner.Start(ctx, refreshInterval)
		logging.Info("Object-side refresh scanner started",
			zap.Duration("interval", refreshInterval),
			zap.String("prefix", cfg.ObjectRefresh.Prefix))
	} else {
		logging.Info("Object-side refresh scanner disabled by configuration")
	}

	// Initialize NFS filesystem and server
	zapLogger := logging.GetLogger()
	nfsLogger := logging.NewKVLogger(zapLogger)

	// Create billy.Filesystem implementation with config
	cosFilesystem := vfs.NewFilesystem(operations, nfsLogger, "/", &cfg.Performance, stagingManager, syncWorker, featureFlags)

	// Wrap with directory caching to work around go-nfs library limitation
	// The go-nfs library doesn't use CachingHandler for READDIR, so we cache at filesystem level
	// Use 30-second TTL - long enough to handle pagination, short enough to see updates
	cachedFS := nfs.NewCachedFilesystem(cosFilesystem, nfsLogger, 30*time.Second)

	// Wrap with instrumentation to track NFS-level behavior
	instrumentedFS := nfs.NewInstrumentedFilesystem(cachedFS, nfsLogger)

	// Wrap with null auth handler, then caching handler
	// Use large cache to prevent verifier eviction during directory pagination
	// Handle cache: 10000 (file handles)
	// Verifier cache: 10000 (directory listings)
	authHandler := nfshelper.NewNullAuthHandler(instrumentedFS)
	cachedHandler := nfshelper.NewCachingHandlerWithVerifierLimit(authHandler, 10000, 10000)

	// Wrap with stable verifier handler to prevent BadCookie errors
	// This ensures the same verifier is returned for a directory across all pagination requests
	stableHandler := nfs.NewStableVerifierHandler(cachedHandler, nfsLogger)

	nfsAddress := fmt.Sprintf(":%d", cfg.Server.NFSPort)
	nfsVersions := cfg.Server.GetNFSVersions()
	nfsServer, err := nfs.NewServer(stableHandler, nfsAddress, nfsLogger, nfsVersions, nfs.ServerOptions{
		AllowedClients:     cfg.Server.AllowedClients,
		ConcurrentHandlers: cfg.Server.NFSConcurrentHandlers,
	})
	if err != nil {
		logging.Fatal("Failed to create NFS server", zap.Error(err))
	}

	if err := nfsServer.Start(); err != nil {
		logging.Fatal("Failed to start NFS server", zap.Error(err))
	}
	defer nfsServer.Stop()

	logging.Info("Bluestone started successfully",
		zap.Int("nfs_port", cfg.Server.NFSPort),
		zap.String("nfs_version", cfg.Server.NFSVersion),
		zap.Int("metrics_port", cfg.Server.MetricsPort),
		zap.Int("health_port", cfg.Server.HealthPort),
	)

	// Add performance metrics endpoint
	http.HandleFunc("/debug/perf", func(w http.ResponseWriter, r *http.Request) {
		counters := metrics.GetGlobalCounters()
		report := counters.GetReport()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(report)
	})

	// Add path-specific stats endpoint
	http.HandleFunc("/debug/perf/paths", func(w http.ResponseWriter, r *http.Request) {
		stats := instrumentedFS.GetAllPathStats()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	})

	// Add staging sync metrics endpoint
	http.HandleFunc("/debug/staging/sync", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if syncWorker == nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"enabled": false,
			})
			return
		}

		stats := syncWorker.Stats()
		stats["enabled"] = true
		json.NewEncoder(w).Encode(stats)
	})

	// Add consolidated dashboard endpoint
	http.HandleFunc("/debug/dashboard/data", func(w http.ResponseWriter, r *http.Request) {
		syncStats := map[string]interface{}{
			"enabled": false,
		}
		if syncWorker != nil {
			syncStats = syncWorker.Stats()
			syncStats["enabled"] = true
		}

		payload := map[string]interface{}{
			"sync":         syncStats,
			"performance":  metrics.GetGlobalCounters().GetReport(),
			"transfers":    metrics.GetTransferReport(),
			"generated_at": time.Now().Format(time.RFC3339Nano),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	})

	http.Handle("/dashboard/", http.StripPrefix("/dashboard/", dashboard.Handler()))
	http.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusTemporaryRedirect)
	})

	// Add reset endpoint
	http.HandleFunc("/debug/perf/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		metrics.ResetCounters()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Counters reset\n"))
	})

	// Add READDIR trace endpoints
	http.HandleFunc("/debug/readdir/enable", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		vfs.EnableTracing()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("READDIR tracing enabled\n"))
	})

	http.HandleFunc("/debug/readdir/disable", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		vfs.DisableTracing()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("READDIR tracing disabled\n"))
	})

	http.HandleFunc("/debug/readdir/traces", func(w http.ResponseWriter, r *http.Request) {
		traces := vfs.GetAllTraces()

		// Analyze each trace
		result := make(map[string]interface{})
		for path, trace := range traces {
			result[path] = trace.Analyze()
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})

	http.HandleFunc("/debug/readdir/trace", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			http.Error(w, "Missing 'path' query parameter", http.StatusBadRequest)
			return
		}

		trace := vfs.GetTrace(path)
		if trace == nil {
			http.Error(w, "No trace found for path", http.StatusNotFound)
			return
		}

		// Return detailed trace
		w.Header().Set("Content-Type", "text/plain")
		trace.PrintTrace(w)
	})

	http.HandleFunc("/debug/readdir/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		vfs.ClearTraces()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("READDIR traces cleared\n"))
	})

	logging.Info("Performance metrics available at:")
	logging.Info(fmt.Sprintf("  http://localhost:%d/debug/perf - Overall metrics", cfg.Server.DebugPort))
	logging.Info(fmt.Sprintf("  http://localhost:%d/debug/perf/paths - Per-path statistics", cfg.Server.DebugPort))
	logging.Info(fmt.Sprintf("  http://localhost:%d/debug/staging/sync - Staging sync queue and upload metrics", cfg.Server.DebugPort))
	logging.Info(fmt.Sprintf("  http://localhost:%d/dashboard/ - End-user sync dashboard", cfg.Server.DebugPort))

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	logging.Info("Received shutdown signal, shutting down gracefully...")

	// Shutdown NFS server
	if err := nfsServer.Stop(); err != nil {
		logging.Error("Error stopping NFS server", zap.Error(err))
	}

	// Close lock manager
	if err := lockManager.Close(); err != nil {
		logging.Error("Error closing lock manager", zap.Error(err))
	}

	// Clear caches
	metadataCache.Clear()
	if err := dataCache.Clear(); err != nil {
		logging.Error("Error clearing data cache", zap.Error(err))
	}

	// Close COS client
	if err := cosClient.Close(); err != nil {
		logging.Error("Error closing COS client", zap.Error(err))
	}

	logging.Info("Shutdown complete")
}

// Made with Bob
