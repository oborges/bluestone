package config

import (
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Validate validates the configuration
func Validate(config *Config) error {
	if err := validateServer(&config.Server); err != nil {
		return fmt.Errorf("server config: %w", err)
	}

	if err := validateCOS(&config.COS); err != nil {
		return fmt.Errorf("cos config: %w", err)
	}

	if err := validateCache(&config.Cache); err != nil {
		return fmt.Errorf("cache config: %w", err)
	}

	if err := validatePerformance(&config.Performance); err != nil {
		return fmt.Errorf("performance config: %w", err)
	}

	if err := validateObjectRefresh(&config.ObjectRefresh); err != nil {
		return fmt.Errorf("object_refresh config: %w", err)
	}

	if err := validateLogging(&config.Logging); err != nil {
		return fmt.Errorf("logging config: %w", err)
	}

	if err := validateStaging(&config.Staging); err != nil {
		return fmt.Errorf("staging config: %w", err)
	}

	if err := validateHA(&config.HA); err != nil {
		return fmt.Errorf("ha config: %w", err)
	}

	if err := validateSMB(&config.SMB); err != nil {
		return fmt.Errorf("smb config: %w", err)
	}

	return nil
}

func validateHA(config *HAConfig) error {
	heartbeat, err := config.GetHeartbeatInterval()
	if err != nil {
		return fmt.Errorf("invalid heartbeat_interval: %w", err)
	}
	timeout, err := config.GetLeaseTimeout()
	if err != nil {
		return fmt.Errorf("invalid lease_timeout: %w", err)
	}
	if config.Enabled && timeout <= heartbeat*2 {
		return fmt.Errorf("lease_timeout (%s) must be more than twice heartbeat_interval (%s) or transient heartbeat delays cause spurious takeovers", timeout, heartbeat)
	}
	switch config.GetOnLeaseLost() {
	case LeaseLostStop, LeaseLostWarn:
	default:
		return fmt.Errorf("invalid on_lease_lost %q: want %q or %q", config.OnLeaseLost, LeaseLostStop, LeaseLostWarn)
	}
	return nil
}

// validateSMB validates the SMB server configuration. Nothing is checked
// while the server is disabled.
func validateSMBLimits(limits *SMBLimits) error {
	counts := []struct {
		name  string
		value int
	}{
		{"max_connections", limits.MaxConnections},
		{"max_connections_per_client", limits.MaxConnectionsPerClient},
		{"max_sessions_per_connection", limits.MaxSessionsPerConnection},
		{"max_trees_per_session", limits.MaxTreesPerSession},
		{"max_opens_per_session", limits.MaxOpensPerSession},
		{"auth_failures", limits.AuthFailures},
	}
	for _, count := range counts {
		if count.value < 0 {
			return fmt.Errorf("invalid limits.%s: %d (must be 0 or more)", count.name, count.value)
		}
	}
	if limits.MaxConnections > 0 && limits.MaxConnectionsPerClient > limits.MaxConnections {
		return fmt.Errorf("limits.max_connections_per_client (%d) exceeds limits.max_connections (%d)",
			limits.MaxConnectionsPerClient, limits.MaxConnections)
	}

	window, err := limits.GetAuthWindow()
	if err != nil {
		return fmt.Errorf("invalid limits.auth_window %q: %w", limits.AuthWindow, err)
	}
	block, err := limits.GetAuthBlock()
	if err != nil {
		return fmt.Errorf("invalid limits.auth_block %q: %w", limits.AuthBlock, err)
	}
	maxBlock, err := limits.GetAuthMaxBlock()
	if err != nil {
		return fmt.Errorf("invalid limits.auth_max_block %q: %w", limits.AuthMaxBlock, err)
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{{"auth_window", window}, {"auth_block", block}, {"auth_max_block", maxBlock}} {
		if d.value <= 0 {
			return fmt.Errorf("invalid limits.%s: %s (must be positive)", d.name, d.value)
		}
	}
	if maxBlock < block {
		return fmt.Errorf("limits.auth_max_block (%s) is shorter than limits.auth_block (%s)", maxBlock, block)
	}
	return nil
}

func validateSMB(config *SMBConfig) error {
	if !config.Enabled {
		return nil
	}
	if config.Port < 1 || config.Port > 65535 {
		return fmt.Errorf("invalid port: %d (must be 1-65535)", config.Port)
	}
	if len(config.Shares) == 0 {
		if err := validShareName(config.ShareName); err != nil {
			return fmt.Errorf("share_name: %w", err)
		}
	}
	if strings.TrimSpace(config.Domain) == "" {
		return fmt.Errorf("domain must not be empty")
	}
	if config.ConcurrentRequests < 0 {
		return fmt.Errorf("invalid concurrent_requests: %d (must be 0 or more)", config.ConcurrentRequests)
	}
	drain, err := config.GetDrainTimeout()
	if err != nil {
		return fmt.Errorf("invalid drain_timeout %q: %w", config.DrainTimeout, err)
	}
	if drain <= 0 {
		return fmt.Errorf("invalid drain_timeout: %s (must be positive)", drain)
	}
	switch config.MaxDialect {
	case "", "3.1.1", "3.0.2":
	default:
		return fmt.Errorf("invalid max_dialect %q: must be 3.1.1 or 3.0.2", config.MaxDialect)
	}
	if config.MaxStreamBytes < 0 || config.MaxStreamBytes > MaxStreamBytesLimit {
		return fmt.Errorf("invalid max_stream_bytes: %d (must be 0-%d: streams are kept in object metadata)", config.MaxStreamBytes, MaxStreamBytesLimit)
	}
	if err := validateSMBLimits(&config.Limits); err != nil {
		return err
	}
	if len(config.Users) == 0 && !config.Kerberos.Enabled() {
		return fmt.Errorf("at least one user, or kerberos.keytab, is required when enabled")
	}
	if skew, err := config.Kerberos.GetMaxClockSkew(); err != nil || skew <= 0 {
		return fmt.Errorf("invalid kerberos.max_clock_skew %q: must be a positive duration", config.Kerberos.MaxClockSkew)
	}
	if err := validateIDMap(config); err != nil {
		return err
	}
	if err := validateShares(config.Shares); err != nil {
		return err
	}
	seen := make(map[string]bool, len(config.Users))
	for i, user := range config.Users {
		if strings.TrimSpace(user.Username) == "" {
			return fmt.Errorf("users[%d]: username must not be empty", i)
		}
		hash, err := user.NTHashBytes()
		if err != nil {
			return fmt.Errorf("users[%d] (%s): %w", i, user.Username, err)
		}
		switch {
		case len(hash) == 0 && user.Password == "":
			return fmt.Errorf("users[%d] (%s): set ntlm_hash (from \"bluestone -smb-hash\") or password", i, user.Username)
		case len(hash) > 0 && user.Password != "":
			return fmt.Errorf("users[%d] (%s): set ntlm_hash or password, not both", i, user.Username)
		}
		if user.UID < 0 || user.GID < 0 {
			return fmt.Errorf("users[%d] (%s): uid and gid must not be negative", i, user.Username)
		}
		key := strings.ToLower(user.Username)
		if seen[key] {
			return fmt.Errorf("users[%d]: duplicate username %q", i, user.Username)
		}
		seen[key] = true
	}
	return nil
}

// validShareName checks a share's name.
func validShareName(name string) error {
	if name == "" || len(name) > 80 {
		return fmt.Errorf("invalid share name %q: must be 1-80 characters", name)
	}
	if strings.ContainsAny(name, `\/:*?"<>|`) || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 }) {
		return fmt.Errorf("invalid share name %q: contains a character share names cannot use", name)
	}
	if strings.EqualFold(name, "IPC$") {
		return fmt.Errorf("invalid share name %q: reserved", name)
	}
	return nil
}

// SharePath cleans a share's path: absolute, with "/" for the whole bucket.
func SharePath(p string) string {
	return path.Clean("/" + strings.TrimSpace(p))
}

// validateShares checks the configured shares: valid, distinct names, and
// directories that do not overlap. Overlapping shares would let one file be
// open through two shares that each keep their own open state.
func validateShares(shares []SMBShare) error {
	names := map[string]bool{}
	for i, share := range shares {
		if err := validShareName(share.Name); err != nil {
			return fmt.Errorf("shares[%d]: %w", i, err)
		}
		key := strings.ToLower(share.Name)
		if names[key] {
			return fmt.Errorf("shares[%d]: duplicate share name %q", i, share.Name)
		}
		names[key] = true
		for _, list := range [][]string{share.ValidUsers, share.ReadList, share.WriteList} {
			for _, entry := range list {
				if err := validPrincipal(entry); err != nil {
					return fmt.Errorf("shares[%d] (%s): %w", i, share.Name, err)
				}
			}
		}
		for j := range i {
			a, b := SharePath(share.Path), SharePath(shares[j].Path)
			if pathWithin(a, b) || pathWithin(b, a) {
				return fmt.Errorf("shares[%d] (%s): path %s overlaps share %s (%s); shares may not overlap", i, share.Name, a, shares[j].Name, b)
			}
		}
	}
	return nil
}

// pathWithin reports whether p is dir or below it, ignoring case as SMB
// does.
func pathWithin(p, dir string) bool {
	p, dir = strings.ToLower(p), strings.ToLower(dir)
	return dir == "/" || p == dir || strings.HasPrefix(p, dir+"/")
}

// validPrincipal checks a user, group or SID in a share access list.
func validPrincipal(entry string) error {
	e := strings.TrimSpace(entry)
	switch {
	case e == "" || e == "@":
		return fmt.Errorf("empty user in an access list")
	case strings.HasPrefix(strings.ToUpper(e), "S-1-"):
		if !isSID(e) {
			return fmt.Errorf("invalid SID %q", entry)
		}
	case strings.Count(e, `\`) > 1:
		return fmt.Errorf("invalid user %q: use DOMAIN\\user", entry)
	}
	return nil
}

// isSID reports whether s is a SID in string form: S-1-<authority>-<sub>...
func isSID(s string) bool {
	parts := strings.Split(strings.ToUpper(s), "-")
	if len(parts) < 3 || len(parts) > 3+15 || parts[0] != "S" || parts[1] != "1" {
		return false
	}
	if _, err := strconv.ParseUint(parts[2], 10, 48); err != nil {
		return false
	}
	for _, p := range parts[3:] {
		if _, err := strconv.ParseUint(p, 10, 32); err != nil {
			return false
		}
	}
	return true
}

// validateIDMap checks the id map, and that local accounts' ids sit below
// the domain's.
func validateIDMap(config *SMBConfig) error {
	m := config.IDMap
	if m.DomainSID == "" {
		return nil
	}
	parts := strings.Split(m.DomainSID, "-")
	if !isSID(m.DomainSID) || len(parts) != 7 || parts[2] != "5" || parts[3] != "21" {
		return fmt.Errorf("invalid id_map.domain_sid %q: want a domain SID, S-1-5-21-<n>-<n>-<n>", m.DomainSID)
	}
	if m.Base < 1000 || m.Base > 1<<30 {
		return fmt.Errorf("invalid id_map.base %d: must be 1000-%d", m.Base, 1<<30)
	}
	for i, user := range config.Users {
		if user.UID >= m.Base || user.GID >= m.Base {
			return fmt.Errorf("users[%d] (%s): uid and gid must be below id_map.base (%d), where domain accounts start", i, user.Username, m.Base)
		}
	}
	return nil
}

// validateServer validates server configuration
func validateServer(config *ServerConfig) error {
	if config.NFSPort < 1 || config.NFSPort > 65535 {
		return fmt.Errorf("invalid nfs_port: %d (must be 1-65535)", config.NFSPort)
	}

	switch strings.ToLower(strings.TrimSpace(config.NFSVersion)) {
	case "3", "v3", "nfsv3", "4", "v4", "nfsv4", "dual", "both", "3,4", "4,3":
	default:
		return fmt.Errorf("invalid nfs_version: %s (must be '4', '3', or 'dual')", config.NFSVersion)
	}

	for _, client := range config.AllowedClients {
		if _, err := ParseClientRule(client); err != nil {
			return fmt.Errorf("invalid allowed_clients entry %q: %w", client, err)
		}
	}

	if config.NFSConcurrentHandlers < 0 {
		return fmt.Errorf("invalid nfs_concurrent_handlers: %d (must be >= 0; 0 selects the default)", config.NFSConcurrentHandlers)
	}

	if config.MetricsPort < 1 || config.MetricsPort > 65535 {
		return fmt.Errorf("invalid metrics_port: %d (must be 1-65535)", config.MetricsPort)
	}

	if config.HealthPort < 1 || config.HealthPort > 65535 {
		return fmt.Errorf("invalid health_port: %d (must be 1-65535)", config.HealthPort)
	}

	if config.MaxConnections < 1 {
		return fmt.Errorf("invalid max_connections: %d (must be > 0)", config.MaxConnections)
	}

	if _, err := config.GetReadTimeout(); err != nil {
		return fmt.Errorf("invalid read_timeout: %w", err)
	}

	if _, err := config.GetWriteTimeout(); err != nil {
		return fmt.Errorf("invalid write_timeout: %w", err)
	}

	return nil
}

// validateCOS validates COS configuration
func validateCOS(config *COSConfig) error {
	if config.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}

	if config.Bucket == "" {
		return fmt.Errorf("bucket is required")
	}

	if config.Region == "" {
		return fmt.Errorf("region is required")
	}

	authType := strings.ToLower(config.AuthType)
	if authType != "iam" && authType != "hmac" {
		return fmt.Errorf("invalid auth_type: %s (must be 'iam' or 'hmac')", config.AuthType)
	}

	// Validate IAM authentication
	if authType == "iam" {
		if config.APIKey == "" {
			return fmt.Errorf("api_key is required for IAM authentication")
		}
	}

	// Validate HMAC authentication
	if authType == "hmac" {
		if config.AccessKey == "" {
			return fmt.Errorf("access_key is required for HMAC authentication")
		}
		if config.SecretKey == "" {
			return fmt.Errorf("secret_key is required for HMAC authentication")
		}
	}

	if config.MaxRetries < 0 {
		return fmt.Errorf("invalid max_retries: %d (must be >= 0)", config.MaxRetries)
	}

	if _, err := config.GetTimeout(); err != nil {
		return fmt.Errorf("invalid timeout: %w", err)
	}

	return nil
}

// validateCache validates cache configuration
func validateCache(config *CacheConfig) error {
	// Validate metadata cache
	if config.Metadata.Enabled {
		if config.Metadata.SizeMB < 1 {
			return fmt.Errorf("invalid metadata cache size_mb: %d (must be > 0)", config.Metadata.SizeMB)
		}
		if config.Metadata.TTLSeconds < 1 {
			return fmt.Errorf("invalid metadata cache ttl_seconds: %d (must be > 0)", config.Metadata.TTLSeconds)
		}
		if config.Metadata.MaxEntries < 1 {
			return fmt.Errorf("invalid metadata cache max_entries: %d (must be > 0)", config.Metadata.MaxEntries)
		}
	}

	// Validate data cache
	if config.Data.Enabled {
		if config.Data.SizeGB < 1 {
			return fmt.Errorf("invalid data cache size_gb: %d (must be > 0)", config.Data.SizeGB)
		}
		if config.Data.Path == "" {
			return fmt.Errorf("data cache path is required")
		}
		if config.Data.ChunkSize < 1 {
			return fmt.Errorf("invalid data cache chunk_size_kb: %d (must be > 0)", config.Data.ChunkSize)
		}

		// Check if cache directory exists or can be created
		if _, err := os.Stat(config.Data.Path); os.IsNotExist(err) {
			if err := os.MkdirAll(config.Data.Path, 0755); err != nil {
				return fmt.Errorf("cannot create cache directory %s: %w", config.Data.Path, err)
			}
		}
	}

	return nil
}

// validatePerformance validates performance configuration
func validatePerformance(config *PerformanceConfig) error {
	if config.ReadAheadKB < 0 {
		return fmt.Errorf("invalid read_ahead_kb: %d (must be >= 0)", config.ReadAheadKB)
	}

	if config.WriteBufferKB < 1 {
		return fmt.Errorf("invalid write_buffer_kb: %d (must be > 0)", config.WriteBufferKB)
	}

	if config.MultipartThresholdMB < 1 {
		return fmt.Errorf("invalid multipart_threshold_mb: %d (must be > 0)", config.MultipartThresholdMB)
	}

	if config.MultipartChunkMB < 1 {
		return fmt.Errorf("invalid multipart_chunk_mb: %d (must be > 0)", config.MultipartChunkMB)
	}

	if config.MultipartChunkMB > config.MultipartThresholdMB {
		return fmt.Errorf("multipart_chunk_mb (%d) cannot be larger than multipart_threshold_mb (%d)",
			config.MultipartChunkMB, config.MultipartThresholdMB)
	}

	if config.WorkerPoolSize < 1 {
		return fmt.Errorf("invalid worker_pool_size: %d (must be > 0)", config.WorkerPoolSize)
	}

	if config.MaxConcurrentReads < 1 {
		return fmt.Errorf("invalid max_concurrent_reads: %d (must be > 0)", config.MaxConcurrentReads)
	}

	if config.MaxConcurrentWrites < 1 {
		return fmt.Errorf("invalid max_concurrent_writes: %d (must be > 0)", config.MaxConcurrentWrites)
	}

	if config.MaxFullObjectReadMB < 1 {
		return fmt.Errorf("invalid max_full_object_read_mb: %d (must be > 0)", config.MaxFullObjectReadMB)
	}

	if config.MaxBufferedWriteMB < 1 {
		return fmt.Errorf("invalid max_buffered_write_mb: %d (must be > 0)", config.MaxBufferedWriteMB)
	}

	if config.MaxDirectoryEntries < 1 {
		return fmt.Errorf("invalid max_directory_entries: %d (must be > 0)", config.MaxDirectoryEntries)
	}

	return nil
}

// validateObjectRefresh validates object-side refresh configuration.
func validateObjectRefresh(config *ObjectRefreshConfig) error {
	if !config.Enabled {
		return nil
	}

	interval, err := config.GetInterval()
	if err != nil {
		return fmt.Errorf("invalid interval: %w", err)
	}
	if interval <= 0 {
		return fmt.Errorf("invalid interval: %s (must be > 0)", interval)
	}

	if strings.Contains(config.Prefix, "\x00") {
		return fmt.Errorf("invalid prefix: contains NUL byte")
	}

	return nil
}

// validateLogging validates logging configuration
func validateLogging(config *LoggingConfig) error {
	level := strings.ToLower(config.Level)
	validLevels := []string{"debug", "info", "warn", "error"}
	valid := false
	for _, l := range validLevels {
		if level == l {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("invalid log level: %s (must be one of: %v)", config.Level, validLevels)
	}

	format := strings.ToLower(config.Format)
	if format != "json" && format != "text" {
		return fmt.Errorf("invalid log format: %s (must be 'json' or 'text')", config.Format)
	}

	if config.Output == "" {
		return fmt.Errorf("log output is required")
	}

	return nil
}

// validateStaging validates staging layer configuration
func validateStaging(config *StagingConfig) error {
	if config.Enabled {
		if config.RootDir == "" {
			return fmt.Errorf("staging root_dir is required when enabled")
		}
		if config.MaxStagingSizeGB < 1 {
			return fmt.Errorf("invalid staging max_staging_size_gb: %d (must be > 0)", config.MaxStagingSizeGB)
		}
		if config.SyncThresholdMB < 1 {
			return fmt.Errorf("invalid staging sync_threshold_mb: %d (must be > 0)", config.SyncThresholdMB)
		}
		if config.SyncWorkerCount < 1 {
			return fmt.Errorf("invalid staging sync_worker_count: %d (must be > 0)", config.SyncWorkerCount)
		}
		mode := strings.ToLower(config.BackpressureMode)
		if mode != "block" && mode != "fail_fast" {
			return fmt.Errorf("invalid staging backpressure_mode: %s (must be 'block' or 'fail_fast')", config.BackpressureMode)
		}
		if config.BackpressureHighWatermarkPct < 1 || config.BackpressureHighWatermarkPct > 99 {
			return fmt.Errorf("invalid staging backpressure_high_watermark_percent: %d (must be 1-99)", config.BackpressureHighWatermarkPct)
		}
		if config.BackpressureCritWatermarkPct < 1 || config.BackpressureCritWatermarkPct > 100 {
			return fmt.Errorf("invalid staging backpressure_critical_watermark_percent: %d (must be 1-100)", config.BackpressureCritWatermarkPct)
		}
		if config.BackpressureHighWatermarkPct >= config.BackpressureCritWatermarkPct {
			return fmt.Errorf("staging backpressure_high_watermark_percent (%d) must be lower than backpressure_critical_watermark_percent (%d)",
				config.BackpressureHighWatermarkPct, config.BackpressureCritWatermarkPct)
		}
		if timeout, err := config.GetBackpressureWaitTimeout(); err != nil || timeout < 0 {
			if err != nil {
				return fmt.Errorf("invalid staging backpressure_wait_timeout: %w", err)
			}
			return fmt.Errorf("invalid staging backpressure_wait_timeout: %s (must be >= 0)", timeout)
		}
		if interval, err := config.GetBackpressureCheckInterval(); err != nil || interval <= 0 {
			if err != nil {
				return fmt.Errorf("invalid staging backpressure_check_interval: %w", err)
			}
			return fmt.Errorf("invalid staging backpressure_check_interval: %s (must be > 0)", interval)
		}

		// Check if staging directory exists or can be created
		if _, err := os.Stat(config.RootDir); os.IsNotExist(err) {
			if err := os.MkdirAll(config.RootDir, 0755); err != nil {
				return fmt.Errorf("cannot create staging directory %s: %w", config.RootDir, err)
			}
		}
	}

	return nil
}

// Made with Bob
