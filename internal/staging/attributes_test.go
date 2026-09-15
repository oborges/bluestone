package staging

import (
	"os"
	"testing"
)

func readSidecarAttributes(t *testing.T, manager *StagingManager, path string) *StagedAttributes {
	t.Helper()
	state, err := readPathMetadataState(manager.pathMetadataPath(manager.stagingFilePath(path)))
	if err != nil {
		t.Fatalf("readPathMetadataState(%s) error = %v", path, err)
	}
	return state.Attributes
}

func TestSessionAttributesPersistThroughDirtyRenameAndRecovery(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}

	session, err := manager.GetOrCreateSession("/a.txt")
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("data"), 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	session.SeedAttributes(0755, 42, 7)
	session.SeedAttributes(0600, 1, 1) // later seeds must not override
	manager.MarkDirty("/a.txt", 4)
	if got := readSidecarAttributes(t, manager, "/a.txt"); got == nil || *got != (StagedAttributes{Mode: 0755, UID: 42, GID: 7}) {
		t.Fatalf("sidecar attributes after MarkDirty = %+v, want seeded 755/42/7", got)
	}

	session.SetMode(0640)
	if got := readSidecarAttributes(t, manager, "/a.txt"); got == nil || got.Mode != 0640 || got.UID != 42 {
		t.Fatalf("sidecar attributes after SetMode = %+v, want mode 640 uid 42", got)
	}

	if err := manager.RenameStagedPath("/a.txt", "/b.txt"); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}
	if got := readSidecarAttributes(t, manager, "/b.txt"); got == nil || *got != (StagedAttributes{Mode: 0640, UID: 42, GID: 7}) {
		t.Fatalf("renamed sidecar attributes = %+v, want 640/42/7", got)
	}
	if err := manager.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	recovered, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after restart error = %v", err)
	}
	defer recovered.Shutdown()
	restored, ok := recovered.GetSession("/b.txt")
	if !ok {
		t.Fatal("renamed dirty session not recovered")
	}
	_, _, mode, uid, gid, _, _, _ := restored.Snapshot()
	if mode != 0640 || uid != 42 || gid != 7 {
		t.Fatalf("recovered attributes = %o/%d/%d, want 640/42/7", mode, uid, gid)
	}
}

func TestUnseededSessionKeepsDefaultsWithoutPersisting(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	session, err := manager.GetOrCreateSession("/new.txt")
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("x"), 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	manager.MarkDirty("/new.txt", 1)

	if got := readSidecarAttributes(t, manager, "/new.txt"); got != nil {
		t.Fatalf("sidecar attributes for a new file = %+v, want none (defaults apply)", got)
	}
	_, _, mode, _, _, _, _, _ := session.Snapshot()
	if mode != os.FileMode(0600) {
		t.Fatalf("new session mode = %o, want default 600", mode)
	}
}
