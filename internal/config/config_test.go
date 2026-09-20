package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestLoadSMBDefaultsAndOverrides(t *testing.T) {
	setRequiredTestEnv(t)

	cfg, err := Load(writeTestConfig(t, "staging:\n  enabled: false\n"))
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.SMB.Enabled || cfg.SMB.Port != 445 || cfg.SMB.ShareName != "bluestone" || cfg.SMB.Domain != "BLUESTONE" {
		t.Fatalf("SMB defaults = %+v, want disabled on 445 sharing bluestone in domain BLUESTONE", cfg.SMB)
	}

	t.Setenv("BLUESTONE_SMB_PORT", "1445")
	cfg, err = Load(writeTestConfig(t, "staging:\n  enabled: false\nsmb:\n  enabled: true\n  share_name: data\n  users:\n    - username: alice\n      password: secret\n"))
	if err != nil {
		t.Fatalf("Load() with SMB enabled returned error: %v", err)
	}
	if !cfg.SMB.Enabled || cfg.SMB.Port != 1445 || cfg.SMB.ShareName != "data" {
		t.Fatalf("SMB = %+v, want enabled on 1445 sharing data", cfg.SMB)
	}
	if len(cfg.SMB.Users) != 1 || cfg.SMB.Users[0].Username != "alice" || cfg.SMB.Users[0].Password != "secret" {
		t.Fatalf("SMB.Users = %+v, want alice", cfg.SMB.Users)
	}

	if _, err := Load(writeTestConfig(t, "staging:\n  enabled: false\nsmb:\n  enabled: true\n")); err == nil || !strings.Contains(err.Error(), "smb config") {
		t.Fatalf("Load() with SMB enabled and no users error = %v, want an smb config error", err)
	}
}

func TestLoadSMBLimitDefaultsAndOverrides(t *testing.T) {
	setRequiredTestEnv(t)

	cfg, err := Load(writeTestConfig(t, "staging:\n  enabled: false\n"))
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	limits := cfg.SMB.Limits
	if limits.MaxConnections != 256 || limits.MaxConnectionsPerClient != 64 ||
		limits.MaxSessionsPerConnection != 32 || limits.MaxTreesPerSession != 64 ||
		limits.MaxOpensPerSession != 4096 || limits.AuthFailures != 5 {
		t.Fatalf("SMB limit defaults = %+v", limits)
	}
	for _, get := range []struct {
		name string
		fn   func() (time.Duration, error)
		want time.Duration
	}{
		{"auth_window", limits.GetAuthWindow, 5 * time.Minute},
		{"auth_block", limits.GetAuthBlock, 30 * time.Second},
		{"auth_max_block", limits.GetAuthMaxBlock, 15 * time.Minute},
	} {
		got, err := get.fn()
		if err != nil || got != get.want {
			t.Fatalf("%s = %s, %v; want %s", get.name, got, err, get.want)
		}
	}

	t.Setenv("BLUESTONE_SMB_LIMITS_MAX_CONNECTIONS", "12")
	cfg, err = Load(writeTestConfig(t, "staging:\n  enabled: false\nsmb:\n  limits:\n    max_opens_per_session: 7\n    auth_block: 2m\n"))
	if err != nil {
		t.Fatalf("Load() with limits returned error: %v", err)
	}
	if cfg.SMB.Limits.MaxConnections != 12 || cfg.SMB.Limits.MaxOpensPerSession != 7 {
		t.Fatalf("SMB limits = %+v, want 12 connections and 7 opens", cfg.SMB.Limits)
	}
	if block, err := cfg.SMB.Limits.GetAuthBlock(); err != nil || block != 2*time.Minute {
		t.Fatalf("auth_block = %s, %v; want 2m", block, err)
	}
}

func TestValidateSMB(t *testing.T) {
	valid := func() SMBConfig {
		return SMBConfig{
			Enabled:   true,
			Port:      445,
			ShareName: "bluestone",
			Domain:    "BLUESTONE",
			Users:     []SMBUser{{Username: "alice", Password: "secret"}},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*SMBConfig)
		wantErr bool
	}{
		{name: "valid", mutate: func(*SMBConfig) {}},
		{name: "disabled skips checks", mutate: func(c *SMBConfig) { *c = SMBConfig{} }},
		{name: "port out of range", mutate: func(c *SMBConfig) { c.Port = 0 }, wantErr: true},
		{name: "empty share name", mutate: func(c *SMBConfig) { c.ShareName = "" }, wantErr: true},
		{name: "share name with separator", mutate: func(c *SMBConfig) { c.ShareName = "a/b" }, wantErr: true},
		{name: "reserved share name", mutate: func(c *SMBConfig) { c.ShareName = "ipc$" }, wantErr: true},
		{name: "empty domain", mutate: func(c *SMBConfig) { c.Domain = " " }, wantErr: true},
		{name: "no users", mutate: func(c *SMBConfig) { c.Users = nil }, wantErr: true},
		{name: "empty username", mutate: func(c *SMBConfig) { c.Users[0].Username = "" }, wantErr: true},
		{name: "no password or hash", mutate: func(c *SMBConfig) { c.Users[0].Password = "" }, wantErr: true},
		{name: "hash instead of password", mutate: func(c *SMBConfig) {
			c.Users[0].Password = ""
			c.Users[0].NTLMHash = "8846f7eaee8fb117ad06bdd830b7586c"
		}},
		{name: "hash and password together", mutate: func(c *SMBConfig) {
			c.Users[0].NTLMHash = "8846f7eaee8fb117ad06bdd830b7586c"
		}, wantErr: true},
		{name: "hash that is not hex", mutate: func(c *SMBConfig) {
			c.Users[0].Password = ""
			c.Users[0].NTLMHash = "zzzz6f7eaee8fb117ad06bdd830b7586"
		}, wantErr: true},
		{name: "hash of the wrong length", mutate: func(c *SMBConfig) {
			c.Users[0].Password = ""
			c.Users[0].NTLMHash = "8846f7ea"
		}, wantErr: true},
		{name: "duplicate username", mutate: func(c *SMBConfig) {
			c.Users = append(c.Users, SMBUser{Username: "Alice", Password: "other"})
		}, wantErr: true},
		{name: "negative limit", mutate: func(c *SMBConfig) { c.Limits.MaxConnections = -1 }, wantErr: true},
		{name: "limits off", mutate: func(c *SMBConfig) { c.Limits = SMBLimits{} }},
		{name: "per-client over total", mutate: func(c *SMBConfig) {
			c.Limits.MaxConnections = 4
			c.Limits.MaxConnectionsPerClient = 8
		}, wantErr: true},
		{name: "per-client under total", mutate: func(c *SMBConfig) {
			c.Limits.MaxConnections = 8
			c.Limits.MaxConnectionsPerClient = 4
		}},
		{name: "per-client without total", mutate: func(c *SMBConfig) { c.Limits.MaxConnectionsPerClient = 8 }},
		{name: "unparsable auth window", mutate: func(c *SMBConfig) { c.Limits.AuthWindow = "soon" }, wantErr: true},
		{name: "zero auth block", mutate: func(c *SMBConfig) { c.Limits.AuthBlock = "0s" }, wantErr: true},
		{name: "max block under block", mutate: func(c *SMBConfig) {
			c.Limits.AuthBlock = "10m"
			c.Limits.AuthMaxBlock = "1m"
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(&cfg)
			if err := validateSMB(&cfg); (err != nil) != tt.wantErr {
				t.Fatalf("validateSMB() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSMBUserHashAndPlaintextReporting(t *testing.T) {
	cfg := SMBConfig{Users: []SMBUser{
		{Username: "hashed", NTLMHash: " 8846F7EAEE8FB117AD06BDD830B7586C "},
		{Username: "plain", Password: "secret"},
		{Username: "also-plain", Password: "secret"},
	}}

	hash, err := cfg.Users[0].NTHashBytes()
	if err != nil || len(hash) != 16 {
		t.Fatalf("NTHashBytes() = %x, %v; want 16 bytes from a padded, upper-case hash", hash, err)
	}
	if hash, err := cfg.Users[1].NTHashBytes(); hash != nil || err != nil {
		t.Fatalf("NTHashBytes() for a password account = %x, %v; want nil", hash, err)
	}
	if got := cfg.UsesPlaintextPasswords(); !slices.Equal(got, []string{"plain", "also-plain"}) {
		t.Fatalf("UsesPlaintextPasswords() = %v, want the two password accounts", got)
	}
	if got := (&SMBConfig{Users: []SMBUser{{Username: "hashed", NTLMHash: "8846f7eaee8fb117ad06bdd830b7586c"}}}).UsesPlaintextPasswords(); got != nil {
		t.Fatalf("UsesPlaintextPasswords() with only hashes = %v, want none", got)
	}
}
