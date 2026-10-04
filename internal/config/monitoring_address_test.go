package config

import (
	"strings"
	"testing"
)

// The metrics and health servers stay on loopback unless told otherwise, and
// an address that is not one is refused rather than failing at listen time.
func TestMonitoringAddress(t *testing.T) {
	cfg, err := Load(writeUsersFileConfig(t, "smb-users.yaml", oneUser, 0o600))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.MonitoringAddress != "127.0.0.1" {
		t.Errorf("default monitoring_address = %q, want 127.0.0.1", cfg.Server.MonitoringAddress)
	}

	for addr, ok := range map[string]bool{"0.0.0.0": true, "::": true, "10.1.2.3": true, "localhost": false, "": false, "0.0.0.0:8081": false} {
		t.Setenv("BLUESTONE_SERVER_MONITORING_ADDRESS", addr)
		cfg, err := Load(writeUsersFileConfig(t, "smb-users.yaml", oneUser, 0o600))
		if addr == "" {
			// An empty variable is unset as far as the loader goes.
			if err != nil || cfg.Server.MonitoringAddress != "127.0.0.1" {
				t.Errorf("empty address: %v, %v; want the default", cfg, err)
			}
			continue
		}
		if ok && (err != nil || cfg.Server.MonitoringAddress != addr) {
			t.Errorf("monitoring_address %q: err = %v", addr, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "monitoring_address")) {
			t.Errorf("monitoring_address %q: err = %v, want it refused", addr, err)
		}
	}
}
