package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadUsesNestedEnvironmentOverrides(t *testing.T) {
	t.Setenv("BLUESTONE_COS_API_KEY", "env-api-key")
	t.Setenv("BLUESTONE_COS_BUCKET", "env-bucket")
	t.Setenv("BLUESTONE_SERVER_NFS_VERSION", "dual")
	t.Setenv("BLUESTONE_CACHE_DATA_ENABLED", "false")
	t.Setenv("BLUESTONE_PERFORMANCE_MAX_FULL_OBJECT_READ_MB", "64")
	t.Setenv("BLUESTONE_PERFORMANCE_MAX_BUFFERED_WRITE_MB", "128")
	t.Setenv("BLUESTONE_PERFORMANCE_MAX_DIRECTORY_ENTRIES", "500")

	cfg, err := Load(writeTestConfig(t, "staging:\n  enabled: false\n"))
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.COS.APIKey != "env-api-key" {
		t.Fatalf("COS.APIKey = %q, want env-api-key", cfg.COS.APIKey)
	}
	if cfg.COS.Bucket != "env-bucket" {
		t.Fatalf("COS.Bucket = %q, want env-bucket", cfg.COS.Bucket)
	}
	if cfg.COS.CircuitBreakerEnabled == nil || !*cfg.COS.CircuitBreakerEnabled {
		t.Fatal("COS.CircuitBreakerEnabled should default to true")
	}
	if cfg.Server.NFSVersion != "dual" {
		t.Fatalf("Server.NFSVersion = %q, want dual", cfg.Server.NFSVersion)
	}
	versions := cfg.Server.GetNFSVersions()
	if len(versions) != 2 || versions[0] != 3 || versions[1] != 4 {
		t.Fatalf("Server.GetNFSVersions() = %v, want [3 4]", versions)
	}
	if cfg.Cache.Data.Enabled {
		t.Fatalf("Cache.Data.Enabled = true, want false from env override")
	}
	if cfg.Performance.MaxFullObjectReadMB != 64 {
		t.Fatalf("Performance.MaxFullObjectReadMB = %d, want 64", cfg.Performance.MaxFullObjectReadMB)
	}
	if cfg.Performance.MaxBufferedWriteMB != 128 {
		t.Fatalf("Performance.MaxBufferedWriteMB = %d, want 128", cfg.Performance.MaxBufferedWriteMB)
	}
	if cfg.Performance.MaxDirectoryEntries != 500 {
		t.Fatalf("Performance.MaxDirectoryEntries = %d, want 500", cfg.Performance.MaxDirectoryEntries)
	}
	if len(cfg.Notices) != 0 {
		t.Fatalf("Notices = %v, want none for new-prefix overrides", cfg.Notices)
	}
}

func TestLoadHonorsLegacyEnvironmentPrefix(t *testing.T) {
	setRequiredTestEnv(t)
	t.Setenv("NFS_GATEWAY_COS_BUCKET", "legacy-bucket")
	t.Setenv("NFS_GATEWAY_COS_SECRET_KEY", "legacy-secret")

	cfg, err := Load(writeTestConfig(t, "staging:\n  enabled: false\n"))
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.COS.Bucket != "legacy-bucket" {
		t.Fatalf("COS.Bucket = %q, want legacy-bucket", cfg.COS.Bucket)
	}
	want := "environment variable NFS_GATEWAY_COS_BUCKET is deprecated; rename it to BLUESTONE_COS_BUCKET"
	if !slices.Contains(cfg.Notices, want) {
		t.Fatalf("Notices = %v, want %q", cfg.Notices, want)
	}
	for _, notice := range cfg.Notices {
		if strings.Contains(notice, "legacy-secret") {
			t.Fatalf("notice leaks an environment value: %q", notice)
		}
	}
}

func TestLoadPrefersNewEnvironmentPrefix(t *testing.T) {
	setRequiredTestEnv(t)
	t.Setenv("NFS_GATEWAY_COS_BUCKET", "legacy-bucket")
	t.Setenv("BLUESTONE_COS_BUCKET", "new-bucket")

	cfg, err := Load(writeTestConfig(t, "staging:\n  enabled: false\n"))
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.COS.Bucket != "new-bucket" {
		t.Fatalf("COS.Bucket = %q, want new-bucket", cfg.COS.Bucket)
	}
}

func TestLoadStagingRootDirLegacyFallback(t *testing.T) {
	tests := []struct {
		name          string
		createDefault bool
		createLegacy  bool
		explicitRoot  bool
		wantLegacy    bool
	}{
		{name: "fresh install uses new default", wantLegacy: false},
		{name: "legacy data only falls back", createLegacy: true, wantLegacy: true},
		{name: "new directory present wins", createDefault: true, createLegacy: true, wantLegacy: false},
		{name: "explicit root_dir is kept", createLegacy: true, explicitRoot: true, wantLegacy: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredTestEnv(t)
			base := t.TempDir()
			defaultDir := filepath.Join(base, "bluestone")
			legacyDir := filepath.Join(base, "nfs-gateway")
			explicitDir := filepath.Join(base, "explicit")
			if tt.createDefault {
				mustMkdir(t, defaultDir)
			}
			if tt.createLegacy {
				mustMkdir(t, legacyDir)
			}
			oldDefault, oldLegacy := defaultStagingRootDir, legacyStagingRootDir
			defaultStagingRootDir, legacyStagingRootDir = defaultDir, legacyDir
			t.Cleanup(func() { defaultStagingRootDir, legacyStagingRootDir = oldDefault, oldLegacy })

			staging := "staging:\n  enabled: false\n"
			if tt.explicitRoot {
				staging += "  root_dir: \"" + explicitDir + "\"\n"
			}
			cfg, err := Load(writeTestConfig(t, staging))
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}

			want := defaultDir
			switch {
			case tt.explicitRoot:
				want = explicitDir
			case tt.wantLegacy:
				want = legacyDir
			}
			if cfg.Staging.RootDir != want {
				t.Fatalf("Staging.RootDir = %q, want %q", cfg.Staging.RootDir, want)
			}
			if gotNotice := len(cfg.Notices) > 0; gotNotice != tt.wantLegacy {
				t.Fatalf("Notices = %v, want notice: %v", cfg.Notices, tt.wantLegacy)
			}
		})
	}
}

// setRequiredTestEnv supplies the credential the base config omits and keeps
// the data cache from creating its directory.
func setRequiredTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BLUESTONE_COS_API_KEY", "env-api-key")
	t.Setenv("BLUESTONE_CACHE_DATA_ENABLED", "false")
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// writeTestConfig writes a valid base config with the given staging section.
func writeTestConfig(t *testing.T, stagingYAML string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configData := []byte(`
server:
  nfs_port: 2049
  nfs_version: "4"
  metrics_port: 8080
  health_port: 8081
  max_connections: 1000
  read_timeout: "30s"
  write_timeout: "30s"
cos:
  endpoint: "s3.us-south.cloud-object-storage.appdomain.cloud"
  bucket: "file-bucket"
  region: "us-south"
  auth_type: "iam"
  max_retries: 3
  timeout: "30s"
cache:
  metadata:
    enabled: false
  data:
    enabled: true
    size_gb: 1
    path: "/should-not-be-created"
    chunk_size_kb: 1024
performance:
  read_ahead_kb: 1024
  write_buffer_kb: 4096
  multipart_threshold_mb: 100
  multipart_chunk_mb: 10
  worker_pool_size: 100
  max_concurrent_reads: 50
  max_concurrent_writes: 25
  max_full_object_read_mb: 512
  max_buffered_write_mb: 512
  max_directory_entries: 100000
logging:
  level: "info"
  format: "json"
  output: "stdout"
` + stagingYAML)

	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	return configPath
}
