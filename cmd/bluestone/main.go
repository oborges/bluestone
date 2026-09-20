package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
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
	"github.com/oborges/bluestone/internal/smb"
	"github.com/oborges/bluestone/internal/staging"
	"github.com/oborges/bluestone/internal/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/ntlmssp"
	nfshelper "github.com/willscott/go-nfs/helpers"
	"go.uber.org/zap"
)

var (
	// Version is set during build
	Version = "dev"

	// Command line flags
	configPath = flag.String("config", "", "Path to configuration file")
	version    = flag.Bool("version", false, "Print version and exit")
	smbHash    = flag.Bool("smb-hash", false, "Read a password from standard input and print its NT hash for smb.users[].ntlm_hash")
)

// printSMBHash reads a password from standard input and prints its NT hash,
// so an operator can put the hash in the configuration instead of the
// password. The password is read from stdin rather than taken as an
// argument, so it stays out of the process list and the shell history.
func printSMBHash() int {
	stat, err := os.Stdin.Stat()
	if err == nil && stat.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "Password: ")
	}
	reader := bufio.NewReader(os.Stdin)
	password, err := reader.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || password == "") {
		fmt.Fprintf(os.Stderr, "Failed to read the password: %v\n", err)
		return 1
	}
	password = strings.TrimRight(password, "\r\n")
	if password == "" {
		fmt.Fprintln(os.Stderr, "The password is empty.")
		return 1
	}
	fmt.Printf("%x\n", ntlmssp.NTHash(password))
	return 0
}

func main() {
	flag.Parse()

	// Print version and exit
	if *version {
		fmt.Printf("Bluestone v%s\n", Version)
		os.Exit(0)
	}

	if *smbHash {
		os.Exit(printSMBHash())
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

	// Byte-range lock table shared by every file protocol server
	locks := lock.NewManager(lock.Options{})

	// Open files and what each open lets others do, so an SMB client that
	// opens a file exclusively conflicts with every other open of it.
	opens := lock.NewShareTable(lock.ShareOptions{})

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
		// change the namespace and purge ancestor listings; uploads keep a
		// parent listing that already shows the file, so they use the
		// narrower invalidation.
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
	// The NFS server serves its own view of the shared filesystem, so its
	// requests are labelled protocol="nfs" in metrics.
	cosFilesystem := vfs.NewFilesystem(operations, nfsLogger, "/", &cfg.Performance, stagingManager, syncWorker, featureFlags).
		ForProtocol(metrics.ProtocolNFS)

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
		Locker:             nfs.NewLocker(locks),
	})
	if err != nil {
		logging.Fatal("Failed to create NFS server", zap.Error(err))
	}

	if err := nfsServer.Start(); err != nil {
		logging.Fatal("Failed to start NFS server", zap.Error(err))
	}
	defer nfsServer.Stop()

	// The SMB server serves its own view with Windows naming, labelled
	// protocol="smb" in metrics, over the same operations and staging.
	var smbServer *smb.Server
	if cfg.SMB.Enabled {
		smbFilesystem := vfs.NewFilesystem(operations, logging.NewKVLogger(zapLogger), "/", &cfg.Performance, stagingManager, syncWorker, featureFlags).
			WithWindowsNames().
			ForProtocol(metrics.ProtocolSMB)
		authWindow, err := cfg.SMB.Limits.GetAuthWindow()
		if err != nil {
			logging.Fatal("Invalid smb.limits.auth_window", zap.Error(err))
		}
		authBlock, err := cfg.SMB.Limits.GetAuthBlock()
		if err != nil {
			logging.Fatal("Invalid smb.limits.auth_block", zap.Error(err))
		}
		authMaxBlock, err := cfg.SMB.Limits.GetAuthMaxBlock()
		if err != nil {
			logging.Fatal("Invalid smb.limits.auth_max_block", zap.Error(err))
		}
		users := make([]smb.User, 0, len(cfg.SMB.Users))
		for _, user := range cfg.SMB.Users {
			hash, err := user.NTHashBytes()
			if err != nil {
				logging.Fatal("Invalid smb user", zap.String("username", user.Username), zap.Error(err))
			}
			users = append(users, smb.User{Name: user.Username, Password: user.Password, NTLMHash: hash})
		}
		if plaintext := cfg.SMB.UsesPlaintextPasswords(); len(plaintext) > 0 {
			logging.Warn("SMB accounts are configured with passwords in clear; store their NT hash in ntlm_hash instead (bluestone -smb-hash)",
				zap.Strings("users", plaintext))
		}
		smbServer, err = smb.NewServer(smbFilesystem, smb.ServerOptions{
			Address:            fmt.Sprintf(":%d", cfg.SMB.Port),
			ShareName:          cfg.SMB.ShareName,
			Domain:             cfg.SMB.Domain,
			Users:              users,
			AllowedClients:     cfg.Server.AllowedClients,
			EncryptionRequired: cfg.SMB.EncryptionRequired,
			Opens:              opens,
			Locks:              locks,
			ConcurrentRequests: cfg.SMB.ConcurrentRequests,
			Limits: smb.Limits{
				Connections:           cfg.SMB.Limits.MaxConnections,
				ConnectionsPerClient:  cfg.SMB.Limits.MaxConnectionsPerClient,
				SessionsPerConnection: cfg.SMB.Limits.MaxSessionsPerConnection,
				TreesPerSession:       cfg.SMB.Limits.MaxTreesPerSession,
				OpensPerSession:       cfg.SMB.Limits.MaxOpensPerSession,
			},
			AuthLimits: smb.AuthLimits{
				Failures: cfg.SMB.Limits.AuthFailures,
				Window:   authWindow,
				Block:    authBlock,
				MaxBlock: authMaxBlock,
			},
			Logger: zapLogger,
		})
		if err != nil {
			logging.Fatal("Failed to create SMB server", zap.Error(err))
		}
		if err := smbServer.Start(); err != nil {
			logging.Fatal("Failed to start SMB server", zap.Error(err))
		}
		defer smbServer.Stop()
		healthChecker.RegisterCheck("smb", health.SMBHealthCheck(func() health.SMBStats {
			stats := smbServer.Stats()
			return health.SMBStats{
				Running:     smbServer.Running(),
				Connections: stats.Connections,
				Sessions:    stats.Sessions,
				OpenFiles:   stats.OpenFiles,
			}
		}))
	} else {
		logging.Info("SMB server disabled by configuration")
		healthChecker.RegisterCheck("smb", health.SMBHealthCheck(nil))
	}

	logging.Info("Bluestone started successfully",
		zap.Int("nfs_port", cfg.Server.NFSPort),
		zap.String("nfs_version", cfg.Server.NFSVersion),
		zap.Bool("smb_enabled", cfg.SMB.Enabled),
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
	if smbServer != nil {
		if err := smbServer.Stop(); err != nil {
			logging.Error("Error stopping SMB server", zap.Error(err))
		}
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
