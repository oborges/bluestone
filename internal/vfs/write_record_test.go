package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oborges/bluestone/internal/staging"
)

// stagedSidecars returns the sidecars in a staging root.
func stagedSidecars(t *testing.T, root string) []string {
	t.Helper()
	sidecars, err := filepath.Glob(filepath.Join(root, "active", "*.metadata"))
	if err != nil {
		t.Fatal(err)
	}
	return sidecars
}

// A write or truncate the gateway cannot record for recovery fails, where it
// used to be accepted with only a warning in the log.
func TestWriteFailsWhenItCannotBeRecorded(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	t.Cleanup(func() { manager.Shutdown() })
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())
	writeTestFile(t, fs, "file.txt", "hello")

	// A directory where the sidecar's temporary file goes fails its write,
	// as a full or failing staging disk would.
	for _, sidecar := range stagedSidecars(t, cfg.RootDir) {
		if err := os.Mkdir(sidecar+".tmp", 0700); err != nil {
			t.Fatal(err)
		}
	}

	f, err := fs.OpenFile("file.txt", os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	defer f.Close()
	if n, err := f.Write([]byte("HELLO")); err == nil {
		t.Fatalf("Write() = %d, nil; want an error", n)
	}
	if err := f.Truncate(2); err == nil {
		t.Fatal("Truncate() succeeded; want an error")
	}
	if _, err := fs.OpenFile("file.txt", os.O_WRONLY|os.O_TRUNC, 0); err == nil {
		t.Fatal("truncating OpenFile() succeeded; want an error")
	}
}
