package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const usersFileConfig = `
cos:
  endpoint: "s3.example.test"
  bucket: "b"
  region: "r"
  auth_type: "hmac"
  access_key: "a"
  secret_key: "s"
cache:
  data:
    path: "%d/cache"
staging:
  root_dir: "%d/staging"
smb:
  enabled: true
  users_file: "%s"
`

func writeUsersFileConfig(t *testing.T, usersFile, users string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	if users != "" {
		if err := os.WriteFile(filepath.Join(dir, "smb-users.yaml"), []byte(users), mode); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(strings.NewReplacer("%s", usersFile, "%d", dir).Replace(usersFileConfig)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const oneUser = `
users:
  - username: "alice"
    ntlm_hash: "0123456789abcdef0123456789abcdef"
    uid: 2001
    gid: 2001
`

// Accounts in smb.users_file count as the configuration's own, and a
// relative path is taken from the configuration file's directory.
func TestSMBUsersFileSuppliesTheAccounts(t *testing.T) {
	cfg, err := Load(writeUsersFileConfig(t, "smb-users.yaml", oneUser, 0o600))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.SMB.Users) != 1 || cfg.SMB.Users[0].Username != "alice" || cfg.SMB.Users[0].UID != 2001 {
		t.Fatalf("users = %+v, want alice from the users file", cfg.SMB.Users)
	}
	for _, notice := range cfg.Notices {
		if strings.Contains(notice, "users_file") {
			t.Errorf("unexpected notice for a file only its owner can read: %s", notice)
		}
	}
}

func TestSMBUsersFileReadableByOthersIsReported(t *testing.T) {
	cfg, err := Load(writeUsersFileConfig(t, "smb-users.yaml", oneUser, 0o644))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, notice := range cfg.Notices {
		found = found || strings.Contains(notice, "chmod 600")
	}
	if !found {
		t.Errorf("no notice for a users file with mode 0644; notices: %v", cfg.Notices)
	}
}

func TestSMBUsersFileProblemsStopTheGateway(t *testing.T) {
	for name, tc := range map[string]struct{ users, want string }{
		"missing file":  {"", "smb.users_file"},
		"no accounts":   {"users: []\n", "no accounts"},
		"unknown field": {"users:\n  - username: bob\n    ntml_hash: x\n", "smb.users_file"},
		"bad hash":      {"users:\n  - username: bob\n    ntlm_hash: nothex\n", "bob"},
	} {
		_, err := Load(writeUsersFileConfig(t, "smb-users.yaml", tc.users, 0o600))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", name, err, tc.want)
		}
	}
}
