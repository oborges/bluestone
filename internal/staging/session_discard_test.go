package staging

import (
	"os"
	"testing"
)

func TestWriteSession_PrefetchFailureDiscardsPartialDownload(t *testing.T) {
	ws, err := NewWriteSession(nil, "/p.bin", t.TempDir()+"/p.data")
	if err != nil {
		t.Fatalf("NewWriteSession() error = %v", err)
	}
	defer ws.Close()

	errDropped := os.ErrDeadlineExceeded
	err = ws.Prefetch(func() error {
		// A download that wrote some bytes, then failed.
		if err := os.WriteFile(ws.StagingPath, []byte("partial"), 0600); err != nil {
			return err
		}
		return errDropped
	})
	if err != errDropped {
		t.Fatalf("Prefetch() error = %v, want %v", err, errDropped)
	}
	info, err := os.Stat(ws.StagingPath)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("staged file keeps %d bytes of a failed download, want 0", info.Size())
	}
	if ws.Prefetched || ws.GetSize() != 0 {
		t.Fatal("failed prefetch must leave the session unprefetched and empty")
	}

	if err := ws.Prefetch(func() error {
		return os.WriteFile(ws.StagingPath, []byte("complete"), 0600)
	}); err != nil {
		t.Fatalf("retried Prefetch() error = %v", err)
	}
	if got := ws.GetSize(); got != int64(len("complete")) {
		t.Fatalf("size after retried prefetch = %d, want %d", got, len("complete"))
	}
}

func TestStagingManager_ReleaseSessionDiscardingIfEmpty(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	t.Run("unused empty session is removed with its files", func(t *testing.T) {
		session, err := manager.GetOrCreateSession("/empty.txt")
		if err != nil {
			t.Fatalf("GetOrCreateSession() error = %v", err)
		}
		manager.ReleaseSessionDiscardingIfEmpty("/empty.txt")
		if _, ok := manager.GetSession("/empty.txt"); ok {
			t.Fatal("unused empty session should be removed")
		}
		for _, file := range []string{session.StagingPath, manager.pathMetadataPath(session.StagingPath)} {
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("%s should be removed, stat err = %v", file, err)
			}
		}
	})

	t.Run("session held by another handle is kept", func(t *testing.T) {
		if _, err := manager.GetOrCreateSession("/held.txt"); err != nil {
			t.Fatalf("GetOrCreateSession() error = %v", err)
		}
		session, err := manager.GetOrCreateSession("/held.txt")
		if err != nil {
			t.Fatalf("GetOrCreateSession() error = %v", err)
		}
		manager.ReleaseSessionDiscardingIfEmpty("/held.txt")
		if kept, ok := manager.GetSession("/held.txt"); !ok || kept != session {
			t.Fatal("session with a remaining reference must be kept")
		}
		if got := session.GetRefCount(); got != 1 {
			t.Fatalf("refcount = %d, want 1", got)
		}
	})

	t.Run("session with staged data is kept", func(t *testing.T) {
		session, err := manager.GetOrCreateSession("/dirty.txt")
		if err != nil {
			t.Fatalf("GetOrCreateSession() error = %v", err)
		}
		if _, err := session.Write([]byte("data"), 0); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		manager.MarkDirty("/dirty.txt", 4)
		manager.ReleaseSessionDiscardingIfEmpty("/dirty.txt")
		if _, ok := manager.GetSession("/dirty.txt"); !ok {
			t.Fatal("session holding staged data must be kept")
		}
	})
}
