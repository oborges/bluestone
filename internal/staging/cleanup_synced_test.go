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
