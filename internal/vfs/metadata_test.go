package vfs

import (
	"os"
	"testing"
	"time"

	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/internal/staging"
	"github.com/oborges/bluestone/pkg/types"
)

func existingObjectMetadata(mode os.FileMode, uid, gid int) map[string]string {
	return posix.EncodePOSIXAttributes(&types.POSIXAttributes{Mode: mode, UID: uid, GID: gid})
}

func assertSessionAttributes(t *testing.T, manager *staging.StagingManager, path string, mode os.FileMode, uid, gid uint32) {
	t.Helper()
	session, ok := manager.GetSession(path)
	if !ok {
		t.Fatalf("no staging session for %s", path)
	}
	_, _, gotMode, gotUID, gotGID, _, _, _ := session.Snapshot()
	if gotMode != mode || gotUID != uid || gotGID != gid {
		t.Fatalf("staged attributes = mode %o uid %d gid %d, want mode %o uid %d gid %d (what sync uploads)",
			gotMode, gotUID, gotGID, mode, uid, gid)
	}
}

func TestEditingExistingObjectKeepsModeAndOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag int
	}{
		{name: "write in place", flag: os.O_RDWR},
		{name: "truncating open", flag: os.O_RDWR | os.O_CREATE | os.O_TRUNC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newTestStagingManager(t)
			store := newFakeObjectStore()
			store.putWithMetadata("file.txt", []byte("original"), existingObjectMetadata(0755, 42, 7))
			fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

			f, err := fs.OpenFile("file.txt", tc.flag, 0644)
			if err != nil {
				t.Fatalf("OpenFile() error = %v", err)
			}
			if _, err := f.Write([]byte("edited")); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			assertSessionAttributes(t, manager, "/file.txt", 0755, 42, 7)
		})
	}
}

func TestChmodChownChtimesUpdateMetadataOnly(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	metadata := existingObjectMetadata(0644, 42, 7)
	metadata["owner-app"] = "keep-me"
	store.putWithMetadata("file.txt", []byte("synced content"), metadata)
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	writesBefore := store.putCount()

	if err := fs.Chmod("file.txt", 0600); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	got := posix.DecodePOSIXAttributes(store.metadataOf("file.txt"), false)
	if got.Mode != 0600 || got.UID != 42 || got.GID != 7 {
		t.Fatalf("after Chmod: mode %o uid %d gid %d, want mode 600 uid 42 gid 7", got.Mode, got.UID, got.GID)
	}

	if err := fs.Chown("file.txt", 5, 6); err != nil {
		t.Fatalf("Chown() error = %v", err)
	}
	got = posix.DecodePOSIXAttributes(store.metadataOf("file.txt"), false)
	if got.Mode != 0600 || got.UID != 5 || got.GID != 6 {
		t.Fatalf("after Chown: mode %o uid %d gid %d, want mode 600 uid 5 gid 6", got.Mode, got.UID, got.GID)
	}

	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := fs.Chtimes("file.txt", mtime, mtime); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	after := store.metadataOf("file.txt")
	got = posix.DecodePOSIXAttributes(after, false)
	if !got.Mtime.Equal(mtime) || got.UID != 5 || got.Mode != 0600 {
		t.Fatalf("after Chtimes: mtime %v uid %d mode %o, want mtime %v uid 5 mode 600", got.Mtime, got.UID, got.Mode, mtime)
	}

	if after["owner-app"] != "keep-me" {
		t.Fatalf("unrelated user metadata lost: %v", after)
	}
	if n := store.putCount(); n != writesBefore {
		t.Fatalf("object rewritten %d times; attribute changes must not rewrite data", n-writesBefore)
	}
	if string(store.get("file.txt")) != "synced content" {
		t.Fatalf("content changed: %q", store.get("file.txt"))
	}
}

func TestStagedAttributesSurviveRestart(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())

	writeTestFile(t, fs, "file.txt", "unsynced")
	if err := fs.Chmod("file.txt", 0640); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	if err := fs.Chown("file.txt", 9, 8); err != nil {
		t.Fatalf("Chown() error = %v", err)
	}
	if err := manager.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	recovered, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after restart error = %v", err)
	}
	defer recovered.Shutdown()
	assertSessionAttributes(t, recovered, "/file.txt", 0640, 9, 8)
}

func TestNewFileRecordsCreationTimeThroughRestart(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())

	before := time.Now()
	writeTestFile(t, fs, "new.txt", "x")
	after := time.Now()

	info, err := fs.Stat("new.txt")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	btime := FileAttributes(info).Btime
	if btime.Before(before) || btime.After(after) {
		t.Fatalf("creation time %v not within the create call [%v, %v]", btime, before, after)
	}
	if err := manager.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	recovered, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after restart error = %v", err)
	}
	defer recovered.Shutdown()
	session, ok := recovered.GetSession("/new.txt")
	if !ok {
		t.Fatal("dirty session not recovered")
	}
	if got := session.Attributes().Btime; !got.Equal(btime) {
		t.Fatalf("recovered creation time = %v, want %v", got, btime)
	}
}

func TestEditingExistingObjectKeepsCreationTimeAndWindowsAttributes(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	btime := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	store.putWithMetadata("file.txt", []byte("original"), posix.EncodePOSIXAttributes(&types.POSIXAttributes{
		Mode: 0644, UID: 42, GID: 7, Btime: btime, WindowsAttributes: posix.WindowsAttributeHidden,
	}))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	f, err := fs.OpenFile("file.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	if _, err := f.Write([]byte("edited")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	info, err := fs.Stat("file.txt")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	attrs := FileAttributes(info)
	if !attrs.Btime.Equal(btime) || attrs.WindowsAttributes != posix.WindowsAttributeHidden || attrs.UID != 42 {
		t.Fatalf("staged file attributes = %+v, want btime %v, hidden, uid 42", attrs, btime)
	}
}

func TestSetAttributesOnSyncedAndStagedFiles(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	store.putWithMetadata("synced.txt", []byte("content"), existingObjectMetadata(0644, 42, 7))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)
	writesBefore := store.putCount()

	btime := time.Date(2023, 1, 2, 3, 4, 5, 0, time.UTC)
	const directoryFlag = 0x10
	flags := posix.WindowsAttributeReadOnly | directoryFlag
	if err := fs.SetAttributes("synced.txt", posix.AttributeUpdate{Btime: &btime, WindowsAttributes: &flags}); err != nil {
		t.Fatalf("SetAttributes(synced) error = %v", err)
	}
	stored := posix.DecodePOSIXAttributes(store.metadataOf("synced.txt"), false)
	if !stored.Btime.Equal(btime) || stored.WindowsAttributes != posix.WindowsAttributeReadOnly || stored.UID != 42 {
		t.Fatalf("synced attributes = %+v, want btime %v, read-only only, uid 42", stored, btime)
	}
	if store.putCount() != writesBefore {
		t.Fatal("SetAttributes rewrote a synced object")
	}

	writeTestFile(t, fs, "staged.txt", "x")
	hiddenSystem := posix.WindowsAttributeHidden | posix.WindowsAttributeSystem
	uid := 77
	if err := fs.SetAttributes("staged.txt", posix.AttributeUpdate{Btime: &btime, WindowsAttributes: &hiddenSystem, UID: &uid}); err != nil {
		t.Fatalf("SetAttributes(staged) error = %v", err)
	}
	info, err := fs.Stat("staged.txt")
	if err != nil {
		t.Fatalf("Stat(staged) error = %v", err)
	}
	attrs := FileAttributes(info)
	if !attrs.Btime.Equal(btime) || attrs.WindowsAttributes != hiddenSystem || attrs.UID != 77 || attrs.GID != 1000 {
		t.Fatalf("staged attributes = %+v, want btime %v, hidden|system, uid 77, gid unchanged 1000", attrs, btime)
	}
}
