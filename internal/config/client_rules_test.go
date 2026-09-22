package config

import (
	"testing"
)

func TestParseClientRule(t *testing.T) {
	valid := []string{"10.0.1.0/24", "10.0.1.5", " 192.168.1.1 ", "fd00::/8", "fd00::1"}
	for _, rule := range valid {
		if _, err := ParseClientRule(rule); err != nil {
			t.Errorf("ParseClientRule(%q) error = %v, want nil", rule, err)
		}
	}

	invalid := []string{"", "not-an-ip", "10.0.0.0/33", "10.0.0/24", "example.com"}
	for _, rule := range invalid {
		if _, err := ParseClientRule(rule); err == nil {
			t.Errorf("ParseClientRule(%q) = nil error, want failure", rule)
		}
	}
}

func TestValidateServerClientRulesAndConcurrency(t *testing.T) {
	base := ServerConfig{
		NFSPort:        2049,
		NFSVersion:     "4",
		MetricsPort:    8080,
		HealthPort:     8081,
		DebugPort:      8082,
		MaxConnections: 1000,
		ReadTimeout:    "30s",
		WriteTimeout:   "30s",
	}

	cfg := base
	cfg.AllowedClients = []string{"10.0.1.0/24", "192.168.7.5"}
	cfg.NFSConcurrentHandlers = 32
	if err := validateServer(&cfg); err != nil {
		t.Fatalf("validateServer(valid) error = %v", err)
	}

	cfg = base
	cfg.AllowedClients = []string{"bogus"}
	if err := validateServer(&cfg); err == nil {
		t.Fatal("validateServer should reject invalid allowed_clients entries")
	}

	cfg = base
	cfg.NFSConcurrentHandlers = -1
	if err := validateServer(&cfg); err == nil {
		t.Fatal("validateServer should reject negative nfs_concurrent_handlers")
	}
}

func TestValidateServerNFSPermissions(t *testing.T) {
	base := ServerConfig{
		NFSPort: 2049, NFSVersion: "4", MetricsPort: 8080, HealthPort: 8081, DebugPort: 8082,
		MaxConnections: 1000, ReadTimeout: "30s", WriteTimeout: "30s", NFSAnonUID: 65534, NFSAnonGID: 65534,
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*ServerConfig)
		enforce bool
		wantErr bool
	}{
		{name: "default", mutate: func(*ServerConfig) {}},
		{name: "none", mutate: func(c *ServerConfig) { c.NFSPermissions = "none" }},
		{name: "posix", mutate: func(c *ServerConfig) { c.NFSPermissions = "POSIX" }, enforce: true},
		{name: "posix with root squash", mutate: func(c *ServerConfig) {
			c.NFSPermissions, c.NFSRootSquash = "posix", true
		}, enforce: true},
		{name: "unknown mode", mutate: func(c *ServerConfig) { c.NFSPermissions = "acl" }, wantErr: true},
		{name: "root squash without enforcement", mutate: func(c *ServerConfig) { c.NFSRootSquash = true }, wantErr: true},
		{name: "negative anonymous uid", mutate: func(c *ServerConfig) { c.NFSAnonUID = -1 }, wantErr: true},
	} {
		cfg := base
		tc.mutate(&cfg)
		if err := validateServer(&cfg); (err != nil) != tc.wantErr {
			t.Errorf("%s: validateServer() error = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
		if got := cfg.EnforcesNFSPermissions(); got != tc.enforce && !tc.wantErr {
			t.Errorf("%s: EnforcesNFSPermissions() = %v, want %v", tc.name, got, tc.enforce)
		}
	}
}

// Made with Bob
