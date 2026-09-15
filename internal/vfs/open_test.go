package vfs

import (
	"errors"
	"os"
	"testing"

	"github.com/oborges/bluestone/internal/staging"
)

var (
	errConnectionDropped = errors.New("read tcp: connection reset by peer")
	errBackendDown       = errors.New("dial tcp: connection refused")
)

func TestOpenFileRefusedWhenPrefetchFails(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	store.put("file.txt", []byte("original content"))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	// The download delivers a few bytes, then the connection drops.
	store.failDownloads(3, errConnectionDropped)
	if f, err := fs.OpenFile("file.txt", os.O_RDWR, 0); err == nil {
		f.Close()
		t.Fatal("OpenFile() succeeded although the existing object could not be staged")
	} else if !errors.Is(err, errConnectionDropped) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenFile() error = %v, want the download error (not ENOENT)", err)
	}
	assertNoStagedState(t, manager, "/file.txt")

	// Readers still see the object, not an empty staged stand-in.
	info, err := fs.Stat("file.txt")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Size() != int64(len("original content")) {
		t.Fatalf("Stat() size = %d, want %d", info.Size(), len("original content"))
	}
	if got := readTestFile(t, fs, "file.txt"); got != "original content" {
		t.Fatalf("content after failed open = %q, want original", got)
	}

	// Once the backend recovers, the open retries the download and writes
	// apply on top of the real content.
	store.failDownloads(0, nil)
	f, err := fs.OpenFile("file.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile() after recovery error = %v", err)
	}
	if _, err := f.(interface {
		WriteAt([]byte, int64) (int, error)
	}).WriteAt([]byte("!"), int64(len("original content"))); err != nil {
		t.Fatalf("WriteAt() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := readTestFile(t, fs, "file.txt"); got != "original content!" {
		t.Fatalf("content after recovered write = %q, want %q", got, "original content!")
	}
}

func TestOpenFileRefusedWhenExistenceUnknown(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	store.put("file.txt", []byte("original content"))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	store.failLookups(errBackendDown)
	for _, flag := range []int{os.O_RDWR, os.O_WRONLY | os.O_APPEND, os.O_RDWR | os.O_CREATE} {
		if f, err := fs.OpenFile("file.txt", flag, 0644); err == nil {
			f.Close()
			t.Fatalf("OpenFile(flag %#x) succeeded although existing content is unknown", flag)
		} else if !errors.Is(err, errBackendDown) || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("OpenFile(flag %#x) error = %v, want the backend error (not ENOENT)", flag, err)
		}
		assertNoStagedState(t, manager, "/file.txt")
	}

	store.failLookups(nil)
	if got := readTestFile(t, fs, "file.txt"); got != "original content" {
		t.Fatalf("content after outage = %q, want original", got)
	}
}

func TestOpenFileTruncatingCreateWorksWhenExistenceUnknown(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	// A truncating create replaces any content, so it stays safe (and
	// available) while the object store cannot answer.
	store.failLookups(errBackendDown)
	writeTestFile(t, fs, "new.txt", "fresh")
	if !manager.IsDirty("/new.txt") {
		t.Fatal("created file should be staged dirty for sync")
	}
	store.failLookups(nil)
	if got := readTestFile(t, fs, "new.txt"); got != "fresh" {
		t.Fatalf("content = %q, want %q", got, "fresh")
	}
}

func TestOpenFileFailedPrefetchKeepsSessionHeldByAnotherHandle(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	store.put("file.txt", []byte("original content"))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	// Another open of the same path holds the (still empty) session.
	held, err := manager.GetOrCreateSession("/file.txt")
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	defer manager.ReleaseSession("/file.txt")

	store.failDownloads(5, errConnectionDropped)
	if f, err := fs.OpenFile("file.txt", os.O_RDWR, 0); err == nil {
		f.Close()
		t.Fatal("OpenFile() succeeded although the existing object could not be staged")
	}

	session, ok := manager.GetSession("/file.txt")
	if !ok || session != held {
		t.Fatal("a session another handle holds must not be discarded")
	}
	if got := session.GetRefCount(); got != 1 {
		t.Fatalf("refcount = %d, want 1 (only the other handle)", got)
	}
	onDisk, err := os.Stat(session.StagingPath)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	if onDisk.Size() != 0 {
		t.Fatalf("staged file holds %d bytes of a failed download, want 0", onDisk.Size())
	}
}

// assertNoStagedState checks that a refused open left nothing behind: an
// empty session would answer Stat and ReadDir as a zero-byte file.
func assertNoStagedState(t *testing.T, manager *staging.StagingManager, path string) {
	t.Helper()
	if manager.IsDirty(path) {
		t.Fatalf("%s must not be dirty after a refused open", path)
	}
	if _, ok := manager.GetSession(path); ok {
		t.Fatalf("%s must not keep a staging session after a refused open", path)
	}
}
