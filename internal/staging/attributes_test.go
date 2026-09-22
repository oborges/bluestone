package staging

import (
	"os"
	"reflect"
	"testing"
	"time"
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

	session.SeedAttributes(StagedAttributes{Mode: 0755, UID: 42, GID: 7})
	session.SeedAttributes(StagedAttributes{Mode: 0600, UID: 1, GID: 1}) // later seeds must not override
	manager.MarkDirty("/a.txt", 4)
	if got := readSidecarAttributes(t, manager, "/a.txt"); got == nil || !reflect.DeepEqual(*got, StagedAttributes{Mode: 0755, UID: 42, GID: 7}) {
		t.Fatalf("sidecar attributes after MarkDirty = %+v, want seeded 755/42/7", got)
	}

	session.SetMode(0640)
	if got := readSidecarAttributes(t, manager, "/a.txt"); got == nil || got.Mode != 0640 || got.UID != 42 {
		t.Fatalf("sidecar attributes after SetMode = %+v, want mode 640 uid 42", got)
	}

	if err := manager.RenameStagedPath("/a.txt", "/b.txt"); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}
	if got := readSidecarAttributes(t, manager, "/b.txt"); got == nil || !reflect.DeepEqual(*got, StagedAttributes{Mode: 0640, UID: 42, GID: 7}) {
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

func TestCreationTimeAndWindowsAttributesPersist(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}

	created := time.Date(2026, 9, 15, 10, 30, 0, 123456789, time.UTC)
	session, err := manager.GetOrCreateSession("/new.txt")
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	session.SetCreationIfUnset(created, 0)
	session.SetCreationIfUnset(created.Add(time.Hour), 0) // a second call keeps the first
	if _, err := session.Write([]byte("x"), 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	manager.MarkDirty("/new.txt", 1)
	session.SetWindowsAttributes(0x2)

	got := readSidecarAttributes(t, manager, "/new.txt")
	if got == nil || !got.Btime.Equal(created) || got.WindowsAttributes != 0x2 {
		t.Fatalf("sidecar attributes = %+v, want btime %v and flags 0x2", got, created)
	}
	if err := manager.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	recovered, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after restart error = %v", err)
	}
	defer recovered.Shutdown()
	restored, ok := recovered.GetSession("/new.txt")
	if !ok {
		t.Fatal("dirty session not recovered")
	}
	if attrs := restored.Attributes(); !attrs.Btime.Equal(created) || attrs.WindowsAttributes != 0x2 {
		t.Fatalf("recovered attributes = %+v, want btime %v and flags 0x2", attrs, created)
	}
}

func TestSetCreationIfUnsetKeepsSeededAttributes(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	seeded := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	session, err := manager.GetOrCreateSession("/existing.txt")
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	session.SeedAttributes(StagedAttributes{Mode: 0644, UID: 42, GID: 7, Btime: seeded})
	session.SetCreationIfUnset(time.Now(), 0o644)
	if got := session.Attributes().Btime; !got.Equal(seeded) {
		t.Fatalf("btime = %v, want the seeded %v", got, seeded)
	}
}

// A staged file's named streams survive a gateway crash along with its
// data, so they are uploaded with it on recovery.
func TestStreamsPersistThroughRecovery(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	session, err := manager.GetOrCreateSession("/tagged.txt")
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("x"), 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	manager.MarkDirty("/tagged.txt", 1)
	want := map[string][]byte{"Zone.Identifier": []byte("ZoneId=3"), "AFP_AfpInfo": {0, 1, 2}}
	session.SetStreams(want)

	if got := readSidecarAttributes(t, manager, "/tagged.txt"); got == nil || !reflect.DeepEqual(got.Streams, want) {
		t.Fatalf("sidecar streams = %+v, want %v", got, want)
	}
	if err := manager.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	recovered, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after restart error = %v", err)
	}
	defer recovered.Shutdown()
	restored, ok := recovered.GetSession("/tagged.txt")
	if !ok {
		t.Fatal("dirty session not recovered")
	}
	if got := restored.Attributes().Streams; !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered streams = %v, want %v", got, want)
	}
}
