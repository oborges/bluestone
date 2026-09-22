package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// EnvPrefix is the prefix for environment variable overrides.
const EnvPrefix = "BLUESTONE"

// LegacyEnvPrefix is the pre-rename prefix. It is still honored, with a
// deprecation notice, so existing deployments keep their overrides.
const LegacyEnvPrefix = "NFS_GATEWAY"

// Default and pre-rename staging roots; variables so tests can redirect them.
var (
	defaultStagingRootDir = "/var/staging/bluestone"
	legacyStagingRootDir  = "/var/staging/nfs-gateway"
)

// Config represents the application configuration
type Config struct {
	// Notices are deprecation warnings found while loading, for the caller
	// to log once logging is initialized.
	Notices []string `mapstructure:"-"`

	Server        ServerConfig        `mapstructure:"server"`
	COS           COSConfig           `mapstructure:"cos"`
	Cache         CacheConfig         `mapstructure:"cache"`
	Performance   PerformanceConfig   `mapstructure:"performance"`
	ObjectRefresh ObjectRefreshConfig `mapstructure:"object_refresh"`
	Logging       LoggingConfig       `mapstructure:"logging"`
	Staging       StagingConfig       `mapstructure:"staging"`
	HA            HAConfig            `mapstructure:"ha"`
	SMB           SMBConfig           `mapstructure:"smb"`
}

// SMBConfig controls the SMB server, which serves the same bucket as NFS with
// Windows naming (case-insensitive names, mapped reserved characters).
type SMBConfig struct {
	Enabled bool `mapstructure:"enabled"`
	Port    int  `mapstructure:"port"`
	// ShareName is the share clients connect to, as in \\host\bluestone.
	ShareName string `mapstructure:"share_name"`
	// Domain is the NTLM domain and server name the gateway advertises.
	Domain string `mapstructure:"domain"`
	// EncryptionRequired rejects sessions that do not encrypt traffic.
	EncryptionRequired bool `mapstructure:"encryption_required"`
	// ConcurrentRequests bounds how many reads and writes one connection
	// handles at once. 0 selects the built-in default (64); 1 handles every
	// request in turn.
	ConcurrentRequests int `mapstructure:"concurrent_requests"`
	// DrainTimeout is how long a shutdown waits for requests in flight
	// before closing the connections carrying them.
	DrainTimeout string `mapstructure:"drain_timeout"`
	// MaxStreamBytes caps a file's named data streams (alternate data
	// streams), names and contents together. They are kept in the object's
	// metadata, which IBM COS caps at about 4 KB per object with the
	// gateway's own attributes, so the limit is at most 2560; set it to
	// 1024 for object stores that follow Amazon S3's 2 KB metadata limit.
	// 0 selects the default.
	MaxStreamBytes int `mapstructure:"max_stream_bytes"`
	// Leases lets clients cache files they read: read and read-handle
	// leases, and level II oplocks. Changes made over NFS break them, and so
	// do changes the object refresh scanner finds made directly in the
	// bucket; without the scanner, a client may keep serving its cached
	// copy of a file changed behind the gateway's back until it closes it.
	Leases bool `mapstructure:"leases"`
	// DurableHandles keeps a client's open files for up to five minutes
	// when its connection drops, so it can reconnect and carry on. It
	// applies to files opened with a read-handle lease, so it needs Leases.
	DurableHandles bool `mapstructure:"durable_handles"`
	// MaxDialect is the highest SMB dialect offered: "3.1.1" (the default,
	// with pre-authentication integrity and AES-GCM encryption) or "3.0.2",
	// for a client that does not get on with 3.1.1.
	MaxDialect string `mapstructure:"max_dialect"`
	// Limits bound what clients can make the server hold. 0 means no limit.
	Limits SMBLimits `mapstructure:"limits"`
	// Users are local accounts, authenticated with NTLM.
	Users []SMBUser `mapstructure:"users"`
	// Kerberos lets Active Directory users sign in with their own
	// credentials, with no local account.
	Kerberos SMBKerberos `mapstructure:"kerberos"`
	// IDMap maps the users who create files to the uid and gid the gateway
	// stores for them, which NFS clients see.
	IDMap SMBIDMap `mapstructure:"id_map"`
	// Shares are the shares served, each a directory of the bucket with
	// its own access rules. Without any, one share named ShareName serves
	// the whole bucket to every user.
	Shares []SMBShare `mapstructure:"shares"`
}

// SMBKerberos configures Kerberos sign-in for Active Directory users.
type SMBKerberos struct {
	// Keytab is the keytab holding the key of the gateway's service
	// principal, cifs/<host name> for every name clients use, as ktpass or
	// "net ads keytab" writes it. Setting it enables Kerberos.
	Keytab string `mapstructure:"keytab"`
	// MaxClockSkew is how far a client's clock may be from the gateway's.
	MaxClockSkew string `mapstructure:"max_clock_skew"`
}

// Enabled reports whether Kerberos sign-in is configured.
func (k *SMBKerberos) Enabled() bool { return strings.TrimSpace(k.Keytab) != "" }

// GetMaxClockSkew returns the parsed clock skew allowance.
func (k *SMBKerberos) GetMaxClockSkew() (time.Duration, error) {
	if strings.TrimSpace(k.MaxClockSkew) == "" {
		return 5 * time.Minute, nil
	}
	return time.ParseDuration(k.MaxClockSkew)
}

// SMBIDMap maps domain accounts to uids and gids by their relative ID, the
// way Samba's idmap_rid does: an account's uid (or a group's gid) is Base
// plus the last part of its SID. Local accounts use the uid and gid they are
// configured with.
type SMBIDMap struct {
	// DomainSID is the Active Directory domain's SID, as
	// "Get-ADDomain | select DomainSID" shows it. Empty maps no domain
	// accounts: files they create keep the default owner.
	DomainSID string `mapstructure:"domain_sid"`
	// Base is the uid and gid of relative ID 0. Local accounts' uids and
	// gids must be below it.
	Base int `mapstructure:"base"`
}

// SMBShare is one share: a directory of the bucket, and who may use it.
//
// Users are named as in Windows: "DOMAIN\user" or "user" for an account,
// "@group" for a group of local accounts, or a SID ("S-1-5-21-...") for a
// domain account or group, as "Get-ADGroup <name>" shows it. Domain groups
// can only be named by SID.
type SMBShare struct {
	// Name is the share's name, as in \\host\name.
	Name string `mapstructure:"name"`
	// Path is the directory of the bucket the share serves; "/" (the
	// default) serves the whole bucket. Shares may not overlap.
	Path string `mapstructure:"path"`
	// ReadOnly lets users read the share but change nothing, except those
	// in WriteList.
	ReadOnly bool `mapstructure:"read_only"`
	// ValidUsers are the only users who may use the share; empty lets every
	// user who can sign in.
	ValidUsers []string `mapstructure:"valid_users"`
	// ReadList are users who may only read, even when the share is not
	// read-only, and WriteList users who may write even when it is.
	// WriteList wins for a user in both.
	ReadList  []string `mapstructure:"read_list"`
	WriteList []string `mapstructure:"write_list"`
}

// DefaultIDMapBase is the uid and gid of a domain's relative ID 0: domain
// accounts' ids start well above those of local accounts.
const DefaultIDMapBase = 100000

// DefaultMaxStreamBytes is the default cap on a file's named streams, and
// MaxStreamBytesLimit the most IBM COS metadata can hold beside the
// gateway's own attributes.
const (
	DefaultMaxStreamBytes = 2048
	MaxStreamBytesLimit   = 2560
)

// SMBLimits bound what SMB clients can make the gateway hold, and how fast a
// client may keep failing to authenticate. A value of 0 means no limit,
// except in auth_*, where 0 selects the default.
type SMBLimits struct {
	// MaxConnections caps connections to the server, and
	// MaxConnectionsPerClient caps those from one client address, so one
	// client cannot use them all up.
	MaxConnections          int `mapstructure:"max_connections"`
	MaxConnectionsPerClient int `mapstructure:"max_connections_per_client"`
	// MaxSessionsPerConnection caps authenticated sessions on one
	// connection, MaxTreesPerSession caps share connections on a session,
	// and MaxOpensPerSession caps the files it holds open.
	MaxSessionsPerConnection int `mapstructure:"max_sessions_per_connection"`
	MaxTreesPerSession       int `mapstructure:"max_trees_per_session"`
	MaxOpensPerSession       int `mapstructure:"max_opens_per_session"`
	// AuthFailures is how many failed logins from one client address start a
	// block, AuthWindow how long failures are remembered, AuthBlock the
	// first block (each further failure doubles it), and AuthMaxBlock the
	// longest block.
	AuthFailures int    `mapstructure:"auth_failures"`
	AuthWindow   string `mapstructure:"auth_window"`
	AuthBlock    string `mapstructure:"auth_block"`
	AuthMaxBlock string `mapstructure:"auth_max_block"`
}

// SMBUser is an account allowed to connect to the SMB share. Give it either
// ntlm_hash or password: the hash keeps the password itself out of the
// configuration file. Generate one with "bluestone -smb-hash".
type SMBUser struct {
	Username string `mapstructure:"username"`
	// Password is the account's password, in clear. Deprecated: use
	// NTLMHash. A configuration with passwords still works, and logs a
	// warning at startup.
	Password string `mapstructure:"password"`
	// NTLMHash is the account's NT hash as 32 hex characters. It
	// authenticates exactly as the password does, so it is still a secret,
	// but the password itself is not written down and cannot be reused
	// against other systems where the user picked the same one.
	NTLMHash string `mapstructure:"ntlm_hash"`
	// UID and GID are the owner the gateway records for files the account
	// creates; 0 leaves the default owner.
	UID int `mapstructure:"uid"`
	GID int `mapstructure:"gid"`
	// Groups name the account's groups, for "@group" in share access lists.
	Groups []string `mapstructure:"groups"`
}

// NTHashBytes returns the configured NT hash, or nil when the account uses a
// password.
func (u *SMBUser) NTHashBytes() ([]byte, error) {
	if strings.TrimSpace(u.NTLMHash) == "" {
		return nil, nil
	}
	hash, err := hex.DecodeString(strings.TrimSpace(u.NTLMHash))
	if err != nil {
		return nil, fmt.Errorf("ntlm_hash is not hexadecimal: %w", err)
	}
	if len(hash) != 16 {
		return nil, fmt.Errorf("ntlm_hash is %d bytes, want 16 (32 hex characters)", len(hash))
	}
	return hash, nil
}

// UsesPlaintextPasswords reports whether any account is configured with a
// password rather than a hash, so startup can warn about it.
func (c *SMBConfig) UsesPlaintextPasswords() []string {
	var users []string
	for _, user := range c.Users {
		if strings.TrimSpace(user.NTLMHash) == "" && user.Password != "" {
			users = append(users, user.Username)
		}
	}
	return users
}

// HAConfig controls active/passive fencing through a bucket lease. Exactly
// one gateway may serve a bucket; the lease makes violations fail loudly.
type HAConfig struct {
	Enabled           bool   `mapstructure:"enabled"`
	HeartbeatInterval string `mapstructure:"heartbeat_interval"`
	LeaseTimeout      string `mapstructure:"lease_timeout"`
	// ForceTakeover steals a fresh foreign lease at startup. Break-glass
	// only (set BLUESTONE_HA_FORCE_TAKEOVER=true); never leave enabled in
	// a config file.
	ForceTakeover bool `mapstructure:"force_takeover"`
	// OnLeaseLost is what the gateway does when it can no longer prove it
	// holds the lease, because another gateway took it or because the lease
	// could not be renewed for longer than lease_timeout:
	//
	//   stop  stop serving and exit non-zero, so a supervisor restarts the
	//         gateway, which then fences itself against the holder. This is
	//         the default: two gateways writing one bucket is what the
	//         lease exists to prevent.
	//   warn  log and keep serving, which risks two writers but never
	//         interrupts this one.
	OnLeaseLost string `mapstructure:"on_lease_lost"`
}

// Values for HAConfig.OnLeaseLost.
const (
	LeaseLostStop = "stop"
	LeaseLostWarn = "warn"
)

// GetOnLeaseLost reports what to do when the lease is lost, defaulting to
// stopping.
func (c *HAConfig) GetOnLeaseLost() string {
	if strings.TrimSpace(c.OnLeaseLost) == "" {
		return LeaseLostStop
	}
	return strings.ToLower(strings.TrimSpace(c.OnLeaseLost))
}

// GetDrainTimeout parses how long a shutdown waits for requests in flight,
// defaulting to 30 seconds.
func (c *SMBConfig) GetDrainTimeout() (time.Duration, error) {
	return smbDuration(c.DrainTimeout, 30*time.Second)
}

// GetAuthWindow parses how long authentication failures are remembered,
// defaulting to 5 minutes.
func (l *SMBLimits) GetAuthWindow() (time.Duration, error) {
	return smbDuration(l.AuthWindow, 5*time.Minute)
}

// GetAuthBlock parses the first block after repeated failures, defaulting to
// 30 seconds.
func (l *SMBLimits) GetAuthBlock() (time.Duration, error) {
	return smbDuration(l.AuthBlock, 30*time.Second)
}

// GetAuthMaxBlock parses the longest block, defaulting to 15 minutes.
func (l *SMBLimits) GetAuthMaxBlock() (time.Duration, error) {
	return smbDuration(l.AuthMaxBlock, 15*time.Minute)
}

func smbDuration(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	return time.ParseDuration(value)
}

// GetHeartbeatInterval parses the heartbeat interval with a 15s default.
func (c *HAConfig) GetHeartbeatInterval() (time.Duration, error) {
	if c.HeartbeatInterval == "" {
		return 15 * time.Second, nil
	}
	return time.ParseDuration(c.HeartbeatInterval)
}

// GetLeaseTimeout parses the lease timeout with a 60s default.
func (c *HAConfig) GetLeaseTimeout() (time.Duration, error) {
	if c.LeaseTimeout == "" {
		return 60 * time.Second, nil
	}
	return time.ParseDuration(c.LeaseTimeout)
}

// ServerConfig represents NFS server configuration
type ServerConfig struct {
	NFSPort        int    `mapstructure:"nfs_port"`
	NFSVersion     string `mapstructure:"nfs_version"`
	MetricsEnabled bool   `mapstructure:"metrics_enabled"`
	MetricsPort    int    `mapstructure:"metrics_port"`
	HealthEnabled  bool   `mapstructure:"health_enabled"`
	HealthPort     int    `mapstructure:"health_port"`
	DebugEnabled   bool   `mapstructure:"debug_enabled"`
	DebugPort      int    `mapstructure:"debug_port"`
	MaxConnections int    `mapstructure:"max_connections"`
	ReadTimeout    string `mapstructure:"read_timeout"`
	WriteTimeout   string `mapstructure:"write_timeout"`
	// AllowedClients restricts which client addresses may connect to the NFS
	// port. Entries are CIDRs ("10.0.1.0/24") or single IPs ("10.0.1.5").
	// Empty means all clients are allowed (rely on external firewalling).
	AllowedClients []string `mapstructure:"allowed_clients"`
	// NFSConcurrentHandlers bounds how many requests are processed in
	// parallel per client connection. 0 selects the built-in default (64);
	// 1 restores fully serial per-connection handling.
	NFSConcurrentHandlers int `mapstructure:"nfs_concurrent_handlers"`
	// NFSPermissions is whether NFS enforces POSIX file permissions:
	// "posix" checks each call's user (its AUTH_SYS uid and groups) against
	// a file's mode, owner and group, as a kernel NFS server does, and
	// makes files belong to the user who creates them. "none" (the
	// default) lets every client user do anything, as before.
	NFSPermissions string `mapstructure:"nfs_permissions"`
	// NFSRootSquash makes root on NFS clients act as the anonymous user.
	// It needs nfs_permissions: posix.
	NFSRootSquash bool `mapstructure:"nfs_root_squash"`
	// NFSAnonUID and NFSAnonGID are who squashed root, and calls without
	// AUTH_SYS credentials, act as.
	NFSAnonUID int `mapstructure:"nfs_anon_uid"`
	NFSAnonGID int `mapstructure:"nfs_anon_gid"`
}

// Values for ServerConfig.NFSPermissions.
const (
	NFSPermissionsNone  = "none"
	NFSPermissionsPOSIX = "posix"
)

// EnforcesNFSPermissions reports whether NFS enforces POSIX permissions.
func (c *ServerConfig) EnforcesNFSPermissions() bool {
	return strings.EqualFold(strings.TrimSpace(c.NFSPermissions), NFSPermissionsPOSIX)
}

// COSConfig represents IBM Cloud COS configuration
type COSConfig struct {
	Endpoint              string `mapstructure:"endpoint"`
	Bucket                string `mapstructure:"bucket"`
	Region                string `mapstructure:"region"`
	AuthType              string `mapstructure:"auth_type"` // "iam" or "hmac"
	APIKey                string `mapstructure:"api_key"`
	ServiceID             string `mapstructure:"service_id"`
	AccessKey             string `mapstructure:"access_key"`
	SecretKey             string `mapstructure:"secret_key"`
	MaxRetries            int    `mapstructure:"max_retries"`
	Timeout               string `mapstructure:"timeout"`
	CircuitBreakerEnabled *bool  `mapstructure:"circuit_breaker_enabled"`
}

// CacheConfig represents caching configuration
type CacheConfig struct {
	Metadata MetadataCacheConfig `mapstructure:"metadata"`
	Data     DataCacheConfig     `mapstructure:"data"`
}

// MetadataCacheConfig represents metadata cache configuration
type MetadataCacheConfig struct {
	Enabled    bool `mapstructure:"enabled"`
	SizeMB     int  `mapstructure:"size_mb"`
	TTLSeconds int  `mapstructure:"ttl_seconds"`
	MaxEntries int  `mapstructure:"max_entries"`
}

// DataCacheConfig represents data cache configuration
type DataCacheConfig struct {
	Enabled   bool   `mapstructure:"enabled"`
	SizeGB    int    `mapstructure:"size_gb"`
	Path      string `mapstructure:"path"`
	ChunkSize int    `mapstructure:"chunk_size_kb"`
}

// PerformanceConfig represents performance tuning configuration
type PerformanceConfig struct {
	ReadAheadKB          int `mapstructure:"read_ahead_kb"`
	WriteBufferKB        int `mapstructure:"write_buffer_kb"`
	MultipartThresholdMB int `mapstructure:"multipart_threshold_mb"`
	MultipartChunkMB     int `mapstructure:"multipart_chunk_mb"`
	WorkerPoolSize       int `mapstructure:"worker_pool_size"`
	MaxConcurrentReads   int `mapstructure:"max_concurrent_reads"`
	MaxConcurrentWrites  int `mapstructure:"max_concurrent_writes"`
	MaxFullObjectReadMB  int `mapstructure:"max_full_object_read_mb"`
	MaxBufferedWriteMB   int `mapstructure:"max_buffered_write_mb"`
	MaxDirectoryEntries  int `mapstructure:"max_directory_entries"`
}

// ObjectRefreshConfig controls object-side change scans.
type ObjectRefreshConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	Interval string `mapstructure:"interval"`
	Prefix   string `mapstructure:"prefix"`
}

const (
	DefaultReadAheadKB         = 8192
	DefaultMaxFullObjectReadMB = 512
	DefaultMaxBufferedWriteMB  = 512
	DefaultMaxDirectoryEntries = 100000
)

// LoggingConfig represents logging configuration
type LoggingConfig struct {
	Level  string `mapstructure:"level"`  // debug, info, warn, error
	Format string `mapstructure:"format"` // json, text
	Output string `mapstructure:"output"` // stdout, stderr, file path
}

// StagingConfig represents staging layer configuration
type StagingConfig struct {
	Enabled                      bool   `mapstructure:"enabled"`
	RootDir                      string `mapstructure:"root_dir"`
	SyncInterval                 string `mapstructure:"sync_interval"`
	SyncThresholdMB              int64  `mapstructure:"sync_threshold_mb"`
	MaxDirtyAge                  string `mapstructure:"max_dirty_age"`
	SyncOnClose                  bool   `mapstructure:"sync_on_close"`
	MaxStagingSizeGB             int64  `mapstructure:"max_staging_size_gb"`
	MaxDirtyFiles                int    `mapstructure:"max_dirty_files"`
	SyncWorkerCount              int    `mapstructure:"sync_worker_count"`
	SyncQueueSize                int    `mapstructure:"sync_queue_size"`
	MaxSyncRetries               int    `mapstructure:"max_sync_retries"`
	RetryBackoffInit             string `mapstructure:"retry_backoff_initial"`
	RetryBackoffMax              string `mapstructure:"retry_backoff_max"`
	CleanAfterSync               bool   `mapstructure:"clean_after_sync"`
	StaleFileAge                 string `mapstructure:"stale_file_age"`
	BackpressureEnabled          bool   `mapstructure:"backpressure_enabled"`
	BackpressureMode             string `mapstructure:"backpressure_mode"`
	BackpressureHighWatermarkPct int    `mapstructure:"backpressure_high_watermark_percent"`
	BackpressureCritWatermarkPct int    `mapstructure:"backpressure_critical_watermark_percent"`
	BackpressureWaitTimeout      string `mapstructure:"backpressure_wait_timeout"`
	BackpressureCheckInterval    string `mapstructure:"backpressure_check_interval"`
}

// Load loads configuration from file and environment variables
func Load(configPath string) (*Config, error) {
	v := viper.New()

	// Set default values
	setDefaults(v)

	// Set config file path
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath("/etc/bluestone/")
		v.AddConfigPath("$HOME/.bluestone")
		v.AddConfigPath("/etc/nfs-gateway/")
		v.AddConfigPath("$HOME/.nfs-gateway")
		v.AddConfigPath("./configs")
		v.AddConfigPath(".")
	}

	// Enable environment variable overrides for nested config keys, e.g.
	// cos.api_key -> BLUESTONE_COS_API_KEY. The pre-rename NFS_GATEWAY_*
	// names are still honored; the new name wins when both are set.
	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := bindEnvOverrides(v); err != nil {
		return nil, err
	}

	// Read config file
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		// Config file not found; using defaults and env vars
	}

	// Unmarshal config
	var config Config
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	config.Notices = legacyEnvNotices()
	if dir, ok := legacyStagingRootFallback(v); ok {
		config.Staging.RootDir = dir
		config.Notices = append(config.Notices, fmt.Sprintf(
			"staging.root_dir is not set and %s does not exist; using pre-rename staging directory %s so unsynced writes are recovered. Set staging.root_dir explicitly to silence this.",
			defaultStagingRootDir, dir))
	}

	// Validate configuration
	if err := Validate(&config); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &config, nil
}

func bindEnvOverrides(v *viper.Viper) error {
	keys := []string{
		"server.nfs_port",
		"server.nfs_version",
		"server.metrics_enabled",
		"server.metrics_port",
		"server.health_enabled",
		"server.health_port",
		"server.debug_enabled",
		"server.debug_port",
		"server.max_connections",
		"server.read_timeout",
		"server.write_timeout",
		"server.allowed_clients",
		"server.nfs_concurrent_handlers",
		"server.nfs_permissions",
		"server.nfs_root_squash",
		"server.nfs_anon_uid",
		"server.nfs_anon_gid",
		"cos.endpoint",
		"cos.bucket",
		"cos.region",
		"cos.auth_type",
		"cos.api_key",
		"cos.service_id",
		"cos.access_key",
		"cos.secret_key",
		"cos.max_retries",
		"cos.timeout",
		"cos.circuit_breaker_enabled",
		"cache.metadata.enabled",
		"cache.metadata.size_mb",
		"cache.metadata.ttl_seconds",
		"cache.metadata.max_entries",
		"cache.data.enabled",
		"cache.data.size_gb",
		"cache.data.path",
		"cache.data.chunk_size_kb",
		"performance.read_ahead_kb",
		"performance.write_buffer_kb",
		"performance.multipart_threshold_mb",
		"performance.multipart_chunk_mb",
		"performance.worker_pool_size",
		"performance.max_concurrent_reads",
		"performance.max_concurrent_writes",
		"performance.max_full_object_read_mb",
		"performance.max_buffered_write_mb",
		"performance.max_directory_entries",
		"object_refresh.enabled",
		"object_refresh.interval",
		"object_refresh.prefix",
		"logging.level",
		"logging.format",
		"logging.output",
		"staging.enabled",
		"staging.root_dir",
		"staging.sync_interval",
		"staging.sync_threshold_mb",
		"staging.max_dirty_age",
		"staging.sync_on_close",
		"staging.max_staging_size_gb",
		"staging.max_dirty_files",
		"staging.sync_worker_count",
		"staging.sync_queue_size",
		"staging.max_sync_retries",
		"staging.retry_backoff_initial",
		"staging.retry_backoff_max",
		"staging.clean_after_sync",
		"staging.stale_file_age",
		"staging.backpressure_enabled",
		"staging.backpressure_mode",
		"staging.backpressure_high_watermark_percent",
		"staging.backpressure_critical_watermark_percent",
		"staging.backpressure_wait_timeout",
		"staging.backpressure_check_interval",
		"ha.enabled",
		"ha.heartbeat_interval",
		"ha.lease_timeout",
		"ha.force_takeover",
		"ha.on_lease_lost",
		"smb.enabled",
		"smb.port",
		"smb.share_name",
		"smb.domain",
		"smb.encryption_required",
		"smb.concurrent_requests",
		"smb.drain_timeout",
		"smb.max_stream_bytes",
		"smb.leases",
		"smb.durable_handles",
		"smb.max_dialect",
		"smb.kerberos.keytab",
		"smb.kerberos.max_clock_skew",
		"smb.id_map.domain_sid",
		"smb.id_map.base",
		"smb.limits.max_connections",
		"smb.limits.max_connections_per_client",
		"smb.limits.max_sessions_per_connection",
		"smb.limits.max_trees_per_session",
		"smb.limits.max_opens_per_session",
		"smb.limits.auth_failures",
		"smb.limits.auth_window",
		"smb.limits.auth_block",
		"smb.limits.auth_max_block",
	}

	for _, key := range keys {
		// Viper takes the first bound name that is set, so the new name wins.
		if err := v.BindEnv(key, envName(EnvPrefix, key), envName(LegacyEnvPrefix, key)); err != nil {
			return fmt.Errorf("failed to bind environment variable for %s: %w", key, err)
		}
	}

	return nil
}

// envName maps a config key to its environment variable, e.g.
// cos.api_key -> BLUESTONE_COS_API_KEY.
func envName(prefix, key string) string {
	return prefix + "_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// legacyEnvNotices reports set pre-rename environment variables by name only;
// values may be secrets.
func legacyEnvNotices() []string {
	var notices []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if rest, ok := strings.CutPrefix(name, LegacyEnvPrefix+"_"); ok {
			notices = append(notices, fmt.Sprintf(
				"environment variable %s is deprecated; rename it to %s_%s", name, EnvPrefix, rest))
		}
	}
	sort.Strings(notices)
	return notices
}

// legacyStagingRootFallback keeps an installation that relied on the
// pre-rename default staging root pointed at its existing staged data.
// Moving the default silently would strand unsynced writes, tombstones, and
// the HA holder marker.
func legacyStagingRootFallback(v *viper.Viper) (string, bool) {
	if v.InConfig("staging.root_dir") {
		return "", false
	}
	for _, prefix := range []string{EnvPrefix, LegacyEnvPrefix} {
		if _, set := os.LookupEnv(envName(prefix, "staging.root_dir")); set {
			return "", false
		}
	}
	if dirExists(defaultStagingRootDir) || !dirExists(legacyStagingRootDir) {
		return "", false
	}
	return legacyStagingRootDir, true
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// setDefaults sets default configuration values
func setDefaults(v *viper.Viper) {
	// Server defaults
	v.SetDefault("server.nfs_port", 2049)
	v.SetDefault("server.nfs_version", "4")
	v.SetDefault("server.metrics_enabled", false)
	v.SetDefault("server.metrics_port", 8080)
	v.SetDefault("server.health_enabled", false)
	v.SetDefault("server.health_port", 8081)
	v.SetDefault("server.debug_enabled", false)
	v.SetDefault("server.debug_port", 8082)
	v.SetDefault("server.max_connections", 1000)
	v.SetDefault("server.allowed_clients", []string{})
	v.SetDefault("server.nfs_concurrent_handlers", 0)
	v.SetDefault("server.nfs_permissions", NFSPermissionsNone)
	v.SetDefault("server.nfs_root_squash", false)
	v.SetDefault("server.nfs_anon_uid", 65534)
	v.SetDefault("server.nfs_anon_gid", 65534)
	v.SetDefault("server.read_timeout", "30s")
	v.SetDefault("server.write_timeout", "30s")

	// COS defaults
	v.SetDefault("cos.auth_type", "iam")
	v.SetDefault("cos.max_retries", 3)
	v.SetDefault("cos.timeout", "30s")
	v.SetDefault("cos.circuit_breaker_enabled", true)

	// Metadata cache defaults
	v.SetDefault("cache.metadata.enabled", true)
	v.SetDefault("cache.metadata.size_mb", 256)
	v.SetDefault("cache.metadata.ttl_seconds", 60)
	v.SetDefault("cache.metadata.max_entries", 10000)

	// Data cache defaults
	v.SetDefault("cache.data.enabled", true)
	v.SetDefault("cache.data.size_gb", 10)
	v.SetDefault("cache.data.path", "/var/cache/bluestone")
	v.SetDefault("cache.data.chunk_size_kb", 1024)

	// Performance defaults
	v.SetDefault("performance.read_ahead_kb", DefaultReadAheadKB)
	v.SetDefault("performance.write_buffer_kb", 4096)
	v.SetDefault("performance.multipart_threshold_mb", 100)
	v.SetDefault("performance.multipart_chunk_mb", 10)
	v.SetDefault("performance.worker_pool_size", 100)
	v.SetDefault("performance.max_concurrent_reads", 50)
	v.SetDefault("performance.max_concurrent_writes", 25)
	v.SetDefault("performance.max_full_object_read_mb", DefaultMaxFullObjectReadMB)
	v.SetDefault("performance.max_buffered_write_mb", DefaultMaxBufferedWriteMB)
	v.SetDefault("performance.max_directory_entries", DefaultMaxDirectoryEntries)

	// Object-side refresh defaults (disabled unless explicitly enabled)
	v.SetDefault("object_refresh.enabled", false)
	v.SetDefault("object_refresh.interval", "5m")
	v.SetDefault("object_refresh.prefix", "")

	// Logging defaults
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")
	v.SetDefault("logging.output", "stdout")

	// Staging defaults (disabled by default for safety)
	v.SetDefault("staging.enabled", false)
	v.SetDefault("staging.root_dir", defaultStagingRootDir)
	v.SetDefault("staging.sync_interval", "30s")
	v.SetDefault("staging.sync_threshold_mb", 10)
	v.SetDefault("staging.max_dirty_age", "5m")
	v.SetDefault("staging.sync_on_close", false)
	v.SetDefault("staging.max_staging_size_gb", 10)
	v.SetDefault("staging.max_dirty_files", 1000)
	v.SetDefault("staging.sync_worker_count", 4)
	v.SetDefault("staging.sync_queue_size", 100)
	v.SetDefault("staging.max_sync_retries", 3)
	v.SetDefault("staging.retry_backoff_initial", "1s")
	v.SetDefault("staging.retry_backoff_max", "60s")
	v.SetDefault("staging.clean_after_sync", true)
	v.SetDefault("staging.stale_file_age", "24h")
	v.SetDefault("staging.backpressure_enabled", true)
	v.SetDefault("staging.backpressure_mode", "block")
	v.SetDefault("staging.backpressure_high_watermark_percent", 80)
	v.SetDefault("staging.backpressure_critical_watermark_percent", 95)
	v.SetDefault("staging.backpressure_wait_timeout", "30s")
	v.SetDefault("staging.backpressure_check_interval", "250ms")

	v.SetDefault("ha.enabled", false)
	v.SetDefault("ha.heartbeat_interval", "15s")
	v.SetDefault("ha.lease_timeout", "60s")
	v.SetDefault("ha.force_takeover", false)
	v.SetDefault("ha.on_lease_lost", "stop")

	// SMB server defaults (disabled unless explicitly enabled)
	v.SetDefault("smb.enabled", false)
	v.SetDefault("smb.port", 445)
	v.SetDefault("smb.share_name", "bluestone")
	v.SetDefault("smb.domain", "BLUESTONE")
	v.SetDefault("smb.encryption_required", false)
	v.SetDefault("smb.concurrent_requests", 0)
	v.SetDefault("smb.drain_timeout", "30s")
	v.SetDefault("smb.max_stream_bytes", DefaultMaxStreamBytes)
	v.SetDefault("smb.leases", true)
	v.SetDefault("smb.durable_handles", true)
	v.SetDefault("smb.max_dialect", "3.1.1")
	v.SetDefault("smb.kerberos.max_clock_skew", "5m")
	v.SetDefault("smb.id_map.base", DefaultIDMapBase)
	// Limits are generous enough that no ordinary client meets them, and
	// small enough that one client cannot exhaust the gateway.
	v.SetDefault("smb.limits.max_connections", 256)
	v.SetDefault("smb.limits.max_connections_per_client", 64)
	v.SetDefault("smb.limits.max_sessions_per_connection", 32)
	v.SetDefault("smb.limits.max_trees_per_session", 64)
	v.SetDefault("smb.limits.max_opens_per_session", 4096)
	v.SetDefault("smb.limits.auth_failures", 5)
	v.SetDefault("smb.limits.auth_window", "5m")
	v.SetDefault("smb.limits.auth_block", "30s")
	v.SetDefault("smb.limits.auth_max_block", "15m")
}

// GetReadTimeout returns the parsed read timeout duration
func (c *ServerConfig) GetReadTimeout() (time.Duration, error) {
	return time.ParseDuration(c.ReadTimeout)
}

// GetWriteTimeout returns the parsed write timeout duration
func (c *ServerConfig) GetWriteTimeout() (time.Duration, error) {
	return time.ParseDuration(c.WriteTimeout)
}

// GetNFSVersions returns the enabled NFS protocol versions.
func (c *ServerConfig) GetNFSVersions() []uint32 {
	switch strings.ToLower(strings.TrimSpace(c.NFSVersion)) {
	case "3", "v3", "nfsv3":
		return []uint32{3}
	case "dual", "both", "3,4", "4,3":
		return []uint32{3, 4}
	default:
		return []uint32{4}
	}
}

// GetTimeout returns the parsed COS timeout duration
func (c *COSConfig) GetTimeout() (time.Duration, error) {
	return time.ParseDuration(c.Timeout)
}

// GetTTL returns the metadata cache TTL as a duration
func (c *MetadataCacheConfig) GetTTL() time.Duration {
	return time.Duration(c.TTLSeconds) * time.Second
}

// GetInterval returns the parsed object refresh interval.
func (c *ObjectRefreshConfig) GetInterval() (time.Duration, error) {
	return time.ParseDuration(c.Interval)
}

// GetSyncInterval returns the parsed sync interval duration
func (c *StagingConfig) GetSyncInterval() (time.Duration, error) {
	return time.ParseDuration(c.SyncInterval)
}

// GetMaxDirtyAge returns the parsed max dirty age duration
func (c *StagingConfig) GetMaxDirtyAge() (time.Duration, error) {
	return time.ParseDuration(c.MaxDirtyAge)
}

// GetRetryBackoffInitial returns the parsed initial retry backoff duration
func (c *StagingConfig) GetRetryBackoffInitial() (time.Duration, error) {
	return time.ParseDuration(c.RetryBackoffInit)
}

// GetRetryBackoffMax returns the parsed max retry backoff duration
func (c *StagingConfig) GetRetryBackoffMax() (time.Duration, error) {
	return time.ParseDuration(c.RetryBackoffMax)
}

// GetStaleFileAge returns the parsed stale file age duration
func (c *StagingConfig) GetStaleFileAge() (time.Duration, error) {
	return time.ParseDuration(c.StaleFileAge)
}

// GetBackpressureWaitTimeout returns the parsed max backpressure wait duration.
func (c *StagingConfig) GetBackpressureWaitTimeout() (time.Duration, error) {
	return time.ParseDuration(c.BackpressureWaitTimeout)
}

// GetBackpressureCheckInterval returns the parsed backpressure polling interval.
func (c *StagingConfig) GetBackpressureCheckInterval() (time.Duration, error) {
	return time.ParseDuration(c.BackpressureCheckInterval)
}

// Made with Bob
