package staging

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

// The cleanup that follows a sync removes the session and its staging file
// only when nothing has taken the path up again since the upload.
func TestCleanupSyncedSessionLeavesPathInUse(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path = "/file.txt"
	stagingPath := manager.stagingFilePath(path)

	session := stageWrite(t, manager, path, "hello")
	manager.MarkClean(path)
	// kept checks that a cleanup after a sync of synced leaves the path's
	// session and staged bytes where they are.
	kept := func(synced *WriteSession, why string) {
		t.Helper()
		cleaned, err := manager.cleanupSyncedSession(path, synced)
		if err != nil {
			t.Fatalf("cleanupSyncedSession() error = %v", err)
		}
		if cleaned {
			t.Fatalf("cleanup removed a session %s", why)
		}
		if current, _ := manager.GetSession(path); current != session {
			t.Fatalf("session gone though %s", why)
		}
		if data, err := os.ReadFile(stagingPath); err != nil || string(data) != "hello" {
			t.Fatalf("staged bytes = %q, %v, though %s", data, err, why)
		}
	}

	kept(session, "a handle holds")

	manager.ReleaseSession(path)
	if err := manager.MarkDirty(path, session.GetSize()); err != nil {
		t.Fatalf("MarkDirty() error = %v", err)
	}
	kept(session, "written since the upload")

	manager.MarkClean(path)
	kept(&WriteSession{}, "that took the path after the one that was synced")

	cleaned, err := manager.cleanupSyncedSession(path, session)
	if err != nil || !cleaned {
		t.Fatalf("cleanupSyncedSession() of an idle, clean session = %v, %v", cleaned, err)
	}
	if _, exists := manager.GetSession(path); exists {
		t.Fatal("idle, clean session still there after cleanup")
	}
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Fatalf("staging file still there after cleanup, stat error = %v", err)
	}
	if sidecarExists(t, manager, path) {
		t.Fatal("sidecar still there after cleanup")
	}
}

// A client that opens, writes and closes once per write, as NFS does for
// each WRITE, against syncs that clean up after themselves. A write that
// arrives as a sync finishes must end up uploaded like any other: it used to
// be able to land in the session, or in the staging file, that the sync was
// removing.
func TestWritesRacingSyncCleanupAreNotLost(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path = "/race.txt"
	const writes = 500

	cosClient := NewMockCOSClient()
	worker := NewSyncWorker(manager, cosClient, cfg)

	// staged is what the last complete write left in the file. A write
	// holds mu from its MarkDirty on, so the check below never sees one
	// that has written its bytes and not yet marked the file dirty.
	var mu sync.Mutex
	var staged string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < writes; i++ {
			data := fmt.Sprintf("version-%06d", i)
			session, err := manager.GetOrCreateSession(path)
			if err != nil {
				t.Errorf("GetOrCreateSession() error = %v", err)
				return
			}
			_, err = session.Write([]byte(data), 0)
			mu.Lock()
			if err == nil {
				err = manager.MarkDirty(path, int64(len(data)))
			}
			if err != nil {
				t.Errorf("write %d error = %v", i, err)
			}
			staged = data
			mu.Unlock()
			manager.ReleaseSession(path)
		}
	}()

	check := func() {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if manager.IsDirty(path) {
			return
		}
		if uploaded, _ := cosClient.GetUpload(path); string(uploaded) != staged {
			t.Fatalf("file is clean with %q written last and %q uploaded", staged, uploaded)
		}
	}
	for writing := true; writing; {
		select {
		case <-done:
			writing = false
		default:
		}
		// A sync that finds the file changed fails and leaves it dirty.
		_ = worker.syncFile(path)
		check()
	}
	if manager.IsDirty(path) {
		if err := worker.syncFile(path); err != nil {
			t.Fatalf("sync after the last write error = %v", err)
		}
	}
	check()
}

// A rename onto a path whose file has just synced puts the renamed bytes
// under the path's name before the path's session and dirty entry say so.
// The cleanup after that sync, arriving in between, must not take them for
// the synced file's and remove them.
func TestCleanupSyncedSessionLeavesRenameDestinationAlone(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	// The destination as its sync leaves it: uploaded, clean, and its idle
	// session about to be cleaned up.
	synced := stageWrite(t, manager, "/dst.txt", "old")
	manager.ReleaseSession("/dst.txt")
	manager.MarkClean("/dst.txt")
	// The file renamed onto it is dirty with no session of its own, as one
	// found on disk at start is.
	stageWrite(t, manager, "/src.txt", "renamed")
	manager.ReleaseSession("/src.txt")
	if err := manager.CleanupSession("/src.txt", false); err != nil {
		t.Fatalf("CleanupSession() error = %v", err)
	}

	t.Cleanup(func() { moveStagedFile = os.Rename })
	moveStagedFile = func(oldStaging, newStaging string) error {
		if err := os.Rename(oldStaging, newStaging); err != nil {
			return err
		}
		cleaned, err := manager.cleanupSyncedSession("/dst.txt", synced)
		if err != nil || cleaned {
			t.Errorf("cleanup of the destination during the rename = %v, %v; want it to keep off", cleaned, err)
		}
		return nil
	}
	if err := manager.RenameStagedPath("/src.txt", "/dst.txt"); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}

	if data, err := os.ReadFile(manager.stagingFilePath("/dst.txt")); err != nil || string(data) != "renamed" {
		t.Fatalf("staged bytes of the renamed file = %q, %v", data, err)
	}
	if !manager.IsDirty("/dst.txt") || sidecarOf(t, manager, "/dst.txt").OriginalPath != "/dst.txt" {
		t.Fatal("renamed file is not dirty with its sidecar")
	}
	cosClient := NewMockCOSClient()
	if err := syncClaimed(t, manager, NewSyncWorker(manager, cosClient, cfg), "/dst.txt"); err != nil {
		t.Fatalf("sync of the renamed file error = %v", err)
	}
	if uploaded, _ := cosClient.GetUpload("/dst.txt"); string(uploaded) != "renamed" {
		t.Fatalf("upload = %q, want the renamed file's bytes", uploaded)
	}

	// Once the rename is over, the cleanup works on the path as before.
	if manager.renameTargets["/dst.txt"] != 0 {
		t.Fatalf("rename left the destination marked: %v", manager.renameTargets)
	}
}

// A reader takes a session and its reference in one step, so the cleanup
// after a sync never finds idle a session a reader is about to use.
func TestAcquireSessionHoldsOffSyncCleanup(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path = "/file.txt"

	if _, ok := manager.AcquireSession(path); ok {
		t.Fatal("AcquireSession() of a path with no session succeeded")
	}
	session := stageWrite(t, manager, path, "hello")
	manager.ReleaseSession(path)
	manager.MarkClean(path)

	acquired, ok := manager.AcquireSession(path)
	if !ok || acquired != session || session.GetRefCount() != 1 {
		t.Fatalf("AcquireSession() = %v, %v with %d references; want the session with one", acquired, ok, session.GetRefCount())
	}
	if cleaned, err := manager.cleanupSyncedSession(path, session); err != nil || cleaned {
		t.Fatalf("cleanup with a reader on the session = %v, %v", cleaned, err)
	}
	buf := make([]byte, 5)
	if n, err := acquired.Read(buf, 0); err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("read through the acquired session = %q, %v", buf[:n], err)
	}

	manager.ReleaseSession(path)
	if cleaned, err := manager.cleanupSyncedSession(path, session); err != nil || !cleaned {
		t.Fatalf("cleanup once the reader is gone = %v, %v", cleaned, err)
	}
}
