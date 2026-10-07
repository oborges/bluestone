package staging

import (
	"os"
	"testing"
)

// A write whose sidecar cannot be written is reported, and still queued: the
// sync is the only way left for its bytes to reach the object store.
func TestMarkDirtyReportsUnrecordedWrite(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	const path = "/file.txt"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("hello"), 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	// A directory where the sidecar's temporary file goes fails its write,
	// as a full or failing staging disk would.
	blocked := manager.pathMetadataPath(manager.stagingFilePath(path)) + ".tmp"
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}

	if err := manager.MarkDirty(path, session.GetSize()); err == nil {
		t.Fatal("MarkDirty() succeeded though the sidecar could not be written")
	}
	if !manager.IsDirty(path) {
		t.Fatal("unrecorded write was not queued for sync")
	}

	// The file is dirty now, but still has no sidecar: the next write must
	// not pass for recorded just because the file is already queued.
	if err := manager.MarkDirty(path, session.GetSize()); err == nil {
		t.Fatal("MarkDirty() of a file still without its sidecar succeeded")
	}

	// Once the sidecar can be written, the next write records the file.
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkDirty(path, session.GetSize()); err != nil {
		t.Fatalf("MarkDirty() error = %v", err)
	}
	if state := sidecarOf(t, manager, path); state.OriginalPath != path {
		t.Fatalf("sidecar names %q, want %q", state.OriginalPath, path)
	}
	if err := manager.CommitPath(path); err != nil {
		t.Fatalf("CommitPath() error = %v", err)
	}
}
