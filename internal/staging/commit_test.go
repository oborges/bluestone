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
// a later write leaves that sidecar alone, and recovery queues the file for
// sync.
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

	// A later write changes nothing a restart needs: the sidecar on disk
	// is not rewritten, and the change is recorded in memory.
	sidecarPath := manager.pathMetadataPath(manager.stagingFilePath(path))
	committedSidecar, err := os.Stat(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	generation := manager.dirtyIndex.GetMetadata(path).LocalDirtyGeneration
	if _, err := session.Write([]byte(" world"), 5); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := manager.MarkDirty(path, session.GetSize()); err != nil {
		t.Fatalf("MarkDirty() error = %v", err)
	}
	if after, err := os.Stat(sidecarPath); err != nil || !os.SameFile(committedSidecar, after) {
		t.Fatalf("a later write replaced the committed sidecar (stat error %v)", err)
	}
	if got := manager.dirtyIndex.GetMetadata(path); got.LocalDirtyGeneration == generation || got.Size != 11 {
		t.Fatalf("dirty entry after a later write = generation %d size %d, want a new generation and size 11",
			got.LocalDirtyGeneration, got.Size)
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
	if got := recovered.dirtyIndex.GetMetadata(path); got.Size != 11 {
		t.Fatalf("recovered size = %d, want the staged file's 11 bytes", got.Size)
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

// A commit does not answer for a dirty file that has no sidecar: one that has
// gone is written again from what is in memory, whether or not anything was
// recorded as changed since the last commit.
func TestCommitPathRestoresLostSidecar(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}

	for _, tc := range []struct {
		path      string
		committed bool
	}{
		{"/never-committed.txt", false},
		{"/committed.txt", true},
	} {
		session := stageWrite(t, manager, tc.path, "hello")
		session.SetMode(0640)
		if tc.committed {
			if err := manager.CommitPath(tc.path); err != nil {
				t.Fatalf("CommitPath(%s) error = %v", tc.path, err)
			}
		}
		if err := os.Remove(manager.pathMetadataPath(manager.stagingFilePath(tc.path))); err != nil {
			t.Fatal(err)
		}

		if err := manager.CommitPath(tc.path); err != nil {
			t.Fatalf("CommitPath(%s) without its sidecar error = %v", tc.path, err)
		}
		state := sidecarOf(t, manager, tc.path)
		if !state.Committed || state.OriginalPath != tc.path {
			t.Fatalf("sidecar of %s after the commit = committed %v path %q", tc.path, state.Committed, state.OriginalPath)
		}
		if state.Attributes == nil || state.Attributes.Mode != 0640 {
			t.Fatalf("sidecar of %s after the commit lost the file's attributes: %+v", tc.path, state.Attributes)
		}
	}

	manager.Shutdown()
	recovered, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after restart error = %v", err)
	}
	defer recovered.Shutdown()
	for _, path := range []string{"/never-committed.txt", "/committed.txt"} {
		if !recovered.IsDirty(path) {
			t.Fatalf("%s not queued for sync after restart", path)
		}
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

	// A committed destination has a sidecar that is on disk. The sidecar of
	// an uncommitted file renamed over it has to go to disk before it takes
	// that one's place, which is what the mark makes happen: a power loss
	// must not leave the destination's name with an empty sidecar and
	// either file's bytes.
	stageWrite(t, manager, "/over.txt", "hello")
	if err := manager.RenameStagedPath("/over.txt", "/new.txt"); err != nil {
		t.Fatalf("RenameStagedPath() over a committed file error = %v", err)
	}
	if !sidecarOf(t, manager, "/new.txt").Committed {
		t.Fatal("sidecar replacing a committed one was not written as committed")
	}
}

// The write that queues a file tells the session of the new sidecar before
// the file is listed as dirty. Another write of the file takes the listing
// as its cue to skip the sidecar, and its commit must still find the sidecar
// to flush.
func TestQueuedFileIsNeverListedBeforeItsSidecarIsPending(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path = "/file.txt"

	// A session whose last commit covered everything, and whose file has
	// since synced: clean, with nothing pending.
	session := stageWrite(t, manager, path, "hello")
	for i := 0; i < 2000; i++ {
		session.mu.Lock()
		session.committedMetadataVersion = session.metadataVersion
		session.mu.Unlock()
		manager.MarkClean(path)

		listed := make(chan bool)
		go func() {
			for !manager.IsDirty(path) {
			}
			session.mu.Lock()
			pending := session.metadataVersion != session.committedMetadataVersion
			session.mu.Unlock()
			listed <- pending
		}()
		if err := manager.MarkDirty(path, session.GetSize()); err != nil {
			t.Fatalf("MarkDirty() error = %v", err)
		}
		if !<-listed {
			t.Fatalf("round %d: file listed as dirty with its new sidecar not yet pending for the next commit", i)
		}
	}
}
