package vfs

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/oborges/bluestone/internal/staging"
)

// Commit is what the NFS server calls before it reports a write as on stable
// storage: it must leave the staged file marked as one recovery can rely on.
func TestCommitMarksStagedFileCommitted(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	t.Cleanup(func() { manager.Shutdown() })
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())
	writeTestFile(t, fs, "file.txt", "hello")

	if err := fs.Commit("file.txt"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	sidecars := stagedSidecars(t, cfg.RootDir)
	if len(sidecars) != 1 {
		t.Fatalf("sidecars = %v, want one", sidecars)
	}
	data, err := os.ReadFile(sidecars[0])
	if err != nil {
		t.Fatal(err)
	}
	var state staging.PathMetadataState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("sidecar is not valid: %v", err)
	}
	if !state.Committed || state.OriginalPath != "/file.txt" {
		t.Fatalf("sidecar = committed %v path %q, want committed /file.txt", state.Committed, state.OriginalPath)
	}

	if err := fs.Commit("never-written.txt"); err != nil {
		t.Fatalf("Commit() of a file with nothing staged error = %v", err)
	}
}

// Without staging a write is in the object store when its handle closes, so
// there is nothing to commit.
func TestCommitWithoutStaging(t *testing.T) {
	fs := newObjectOnlyTestFilesystem(t, newFakeObjectStore())
	writeTestFile(t, fs, "file.txt", "hello")
	if err := fs.Commit("file.txt"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}
