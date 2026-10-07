package staging

import (
	"os"
	"testing"
)

// stageWrite stages data at path as one accepted client write does.
func stageWrite(t *testing.T, manager *StagingManager, path, data string) *WriteSession {
	t.Helper()
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession(%s) error = %v", path, err)
	}
	if _, err := session.Write([]byte(data), 0); err != nil {
		t.Fatalf("Write(%s) error = %v", path, err)
	}
	if err := manager.MarkDirty(path, session.GetSize()); err != nil {
		t.Fatalf("MarkDirty(%s) error = %v", path, err)
	}
	return session
}

func sidecarOf(t *testing.T, manager *StagingManager, path string) *PathMetadataState {
	t.Helper()
	state, err := readPathMetadataState(manager.pathMetadataPath(manager.stagingFilePath(path)))
	if err != nil {
		t.Fatalf("sidecar of %s: %v", path, err)
	}
	return state
}

// A committed write is one a restart finds: its sidecar is marked committed,
// stays so across later writes, and recovery queues the file for sync.
func TestCommitPathKeepsWriteRecoverable(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	const path = "/dir/file.txt"

	session := stageWrite(t, manager, path, "hello")
	if sidecarOf(t, manager, path).Committed {
		t.Fatal("sidecar committed before any commit")
	}
	if err := manager.CommitPath(path); err != nil {
		t.Fatalf("CommitPath() error = %v", err)
	}
	if !sidecarOf(t, manager, path).Committed {
		t.Fatal("sidecar not marked committed")
	}

	// A later write rewrites the sidecar; it must stay committed, or the
	// rewrite could replace the one on disk with one that is not.
	generation := sidecarOf(t, manager, path).LocalDirtyGeneration
	if _, err := session.Write([]byte(" world"), 5); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := manager.MarkDirty(path, session.GetSize()); err != nil {
		t.Fatalf("MarkDirty() error = %v", err)
	}
	state := sidecarOf(t, manager, path)
	if !state.Committed || state.LocalDirtyGeneration <= generation {
		t.Fatalf("sidecar after a later write = committed %v generation %d, want committed and past %d",
			state.Committed, state.LocalDirtyGeneration, generation)
	}
	if err := manager.CommitPath(path); err != nil {
		t.Fatalf("second CommitPath() error = %v", err)
	}

	manager.Shutdown()
	recovered, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after restart error = %v", err)
	}
	defer recovered.Shutdown()
	if !recovered.IsDirty(path) {
		t.Fatal("committed write not queued for sync after restart")
	}
	data, err := os.ReadFile(recovered.stagingFilePath(path))
	if err != nil || string(data) != "hello world" {
		t.Fatalf("staged bytes after restart = %q, %v", data, err)
	}
}

// Nothing staged is nothing to commit: a path never written here, and one
// whose bytes the object store already has.
func TestCommitPathWithNothingOutstanding(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	if err := manager.CommitPath("/never/written"); err != nil {
		t.Fatalf("CommitPath() of an unstaged path error = %v", err)
	}

	const path = "/synced.txt"
	stageWrite(t, manager, path, "hello")
	manager.MarkClean(path)
	if err := manager.CommitPath(path); err != nil {
		t.Fatalf("CommitPath() of a synced path error = %v", err)
	}
}

// A dirty file without its sidecar could not be recovered, so it cannot be
// reported as committed.
func TestCommitPathFailsWithoutSidecar(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	const path = "/file.txt"
	stageWrite(t, manager, path, "hello")
	if err := os.Remove(manager.pathMetadataPath(manager.stagingFilePath(path))); err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitPath(path); err == nil {
		t.Fatal("CommitPath() of a dirty file with no sidecar succeeded")
	}
}

// Renaming a committed file keeps it committed under the new name.
func TestRenameStagedPathKeepsCommitted(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	stageWrite(t, manager, "/old.txt", "hello")
	if err := manager.CommitPath("/old.txt"); err != nil {
		t.Fatalf("CommitPath() error = %v", err)
	}
	if err := manager.RenameStagedPath("/old.txt", "/new.txt"); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}
	if !sidecarOf(t, manager, "/new.txt").Committed {
		t.Fatal("renamed file lost its committed mark")
	}

	stageWrite(t, manager, "/plain.txt", "hello")
	if err := manager.RenameStagedPath("/plain.txt", "/moved.txt"); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}
	if sidecarOf(t, manager, "/moved.txt").Committed {
		t.Fatal("rename marked an uncommitted file committed")
	}
}
