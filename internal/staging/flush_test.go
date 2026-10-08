package staging

import (
	"os"
	"sync"
	"testing"
	"time"
)

// heldFlush stands in for the flush of staging files in a test. The first
// flush after hold waits for release; every flush is recorded.
type heldFlush struct {
	mu       sync.Mutex
	armed    bool
	flushing chan *os.File
	release  chan struct{}
	flushed  []*os.File
}

// holdNextFlush makes the next flush of a staging file wait until the
// returned heldFlush is released, which the test must do.
func holdNextFlush(t *testing.T) *heldFlush {
	t.Helper()
	h := &heldFlush{
		armed:    true,
		flushing: make(chan *os.File, 1),
		release:  make(chan struct{}),
	}
	flushStagingFile = func(file *os.File) error {
		h.mu.Lock()
		held := h.armed
		h.armed = false
		h.flushed = append(h.flushed, file)
		h.mu.Unlock()
		if held {
			h.flushing <- file
			<-h.release
		}
		return file.Sync()
	}
	t.Cleanup(func() { flushStagingFile = (*os.File).Sync })
	return h
}

// started waits for the held flush to begin and returns the file it is of.
func (h *heldFlush) started(t *testing.T) *os.File {
	t.Helper()
	select {
	case file := <-h.flushing:
		return file
	case <-time.After(10 * time.Second):
		t.Fatal("no flush began")
		return nil
	}
}

func (h *heldFlush) files() []*os.File {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*os.File(nil), h.flushed...)
}

// finishes fails the test if do is still running after a while: it is
// waiting for the flush the test holds open.
func finishes(t *testing.T, what string, do func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		do()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("%s waited for the flush to end", what)
	}
}

// While one file is flushed to disk, nothing else waits for it: not a write
// to another file, which first adds up what every session stages, not an
// open of the file itself, which takes its session under the manager's lock,
// and not a write to it. They all used to, because the flush held the
// session's lock for as long as the disk took.
func TestFlushKeepsNothingElseWaiting(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const flushed, other = "/flushed.txt", "/other.txt"

	session := stageWrite(t, manager, flushed, "hello")
	otherSession := stageWrite(t, manager, other, "hello")

	held := holdNextFlush(t)
	committed := make(chan error, 1)
	go func() { committed <- manager.CommitPath(flushed) }()
	held.started(t)

	finishes(t, "counting the staged bytes", func() { manager.GetTotalStagingSize() })
	finishes(t, "a write to another file", func() {
		if _, err := otherSession.Write([]byte(" world"), 5); err != nil {
			t.Errorf("Write() to another file error = %v", err)
		}
		if err := manager.MarkDirty(other, otherSession.GetSize()); err != nil {
			t.Errorf("MarkDirty() of another file error = %v", err)
		}
	})
	finishes(t, "a commit of another file", func() {
		if err := manager.CommitPath(other); err != nil {
			t.Errorf("CommitPath() of another file error = %v", err)
		}
	})
	finishes(t, "an open of the file being flushed", func() {
		if _, err := manager.GetOrCreateSession(flushed); err != nil {
			t.Errorf("GetOrCreateSession() error = %v", err)
		}
		manager.ReleaseSession(flushed)
	})
	finishes(t, "a write to the file being flushed", func() {
		if _, err := session.Write([]byte(" world"), 5); err != nil {
			t.Errorf("Write() error = %v", err)
		}
		if err := manager.MarkDirty(flushed, session.GetSize()); err != nil {
			t.Errorf("MarkDirty() error = %v", err)
		}
	})

	close(held.release)
	if err := <-committed; err != nil {
		t.Fatalf("CommitPath() error = %v", err)
	}
	if !sidecarOf(t, manager, flushed).Committed {
		t.Fatal("sidecar not marked committed")
	}
}

// A write that arrives while its file is flushed, with an upload of the file
// in flight, moves the session to a new staging file. The commit has to
// leave that file on disk, and its name in the directory: it is the one a
// restart would find.
func TestCommitFlushesTheFileTheSessionMovedTo(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path = "/file.txt"

	session := stageWrite(t, manager, path, "hello")
	if err := manager.CommitPath(path); err != nil {
		t.Fatalf("CommitPath() error = %v", err)
	}
	snapshot, err := session.OpenUploadSnapshot()
	if err != nil {
		t.Fatalf("OpenUploadSnapshot() error = %v", err)
	}
	defer snapshot.Close()

	held := holdNextFlush(t)
	committed := make(chan error, 1)
	go func() { committed <- manager.CommitPath(path) }()
	first := held.started(t)

	if _, err := session.Write([]byte("HELLO, again"), 0); err != nil {
		t.Fatalf("Write() during the flush error = %v", err)
	}
	if err := manager.MarkDirty(path, session.GetSize()); err != nil {
		t.Fatalf("MarkDirty() error = %v", err)
	}
	session.mu.Lock()
	current := session.File
	session.mu.Unlock()
	if current == first {
		t.Fatal("the write did not move the session to a new staging file")
	}

	close(held.release)
	if err := <-committed; err != nil {
		t.Fatalf("CommitPath() error = %v", err)
	}
	if files := held.files(); files[len(files)-1] != current {
		t.Fatal("commit did not flush the staging file the session moved to")
	}
	if _, uncommitted, err := session.syncToDisk(); err != nil || uncommitted != 0 {
		t.Fatalf("after the commit: uncommitted %d, error %v; want the new file's name flushed too", uncommitted, err)
	}
	if data, err := os.ReadFile(manager.stagingFilePath(path)); err != nil || string(data) != "HELLO, again" {
		t.Fatalf("staged bytes = %q, %v", data, err)
	}
}

// A session closed while its file is flushed has nothing left to commit:
// its bytes went to the object store, or with the file.
func TestCommitOfSessionClosedDuringFlush(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path = "/file.txt"

	session := stageWrite(t, manager, path, "hello")
	held := holdNextFlush(t)
	committed := make(chan error, 1)
	go func() { committed <- manager.CommitPath(path) }()
	held.started(t)

	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	close(held.release)
	if err := <-committed; err != nil {
		t.Fatalf("CommitPath() of a session closed meanwhile error = %v", err)
	}
	if err := session.Sync(); err == nil {
		t.Fatal("Sync() of a closed session succeeded")
	}
}

// Opening the staged bytes for an upload flushes them first, and does not
// hold the session for that either. The snapshot describes the file as it is
// when the flush is over, a write that arrived meanwhile included.
func TestOpenUploadSnapshotDoesNotHoldSessionWhileFlushing(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path = "/file.txt"

	session := stageWrite(t, manager, path, "hello")
	held := holdNextFlush(t)
	type opened struct {
		snapshot *UploadSnapshot
		err      error
	}
	result := make(chan opened, 1)
	go func() {
		snapshot, err := session.OpenUploadSnapshot()
		result <- opened{snapshot, err}
	}()
	held.started(t)

	finishes(t, "a write to the file being flushed for upload", func() {
		if _, err := session.Write([]byte(" world"), 5); err != nil {
			t.Errorf("Write() error = %v", err)
		}
	})

	close(held.release)
	got := <-result
	if got.err != nil {
		t.Fatalf("OpenUploadSnapshot() error = %v", got.err)
	}
	defer got.snapshot.Close()
	if got.snapshot.Size != 11 {
		t.Fatalf("snapshot size = %d, want the 11 bytes staged when it was opened", got.snapshot.Size)
	}
	data := make([]byte, 16)
	n, _ := got.snapshot.File.ReadAt(data, 0)
	if string(data[:n]) != "hello world" {
		t.Fatalf("snapshot reads %q, want %q", data[:n], "hello world")
	}
}

// A session can be held for a long time by one operation: a write to it that
// the kernel makes wait for a slow disk, or a copy of its bytes. Whatever
// the manager does for every file must not wait for it: counting what is
// staged before a write, and opening, closing or looking up a file, even the
// busy one itself.
func TestBusySessionDoesNotHoldUpTheManager(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const busy, other = "/busy.txt", "/other.txt"

	busySession := stageWrite(t, manager, busy, "hello")
	otherSession := stageWrite(t, manager, other, "hello")

	// What a write to the file does for as long as the disk takes.
	busySession.mu.Lock()
	released := false
	release := func() {
		if !released {
			released = true
			busySession.mu.Unlock()
		}
	}
	defer release()

	finishes(t, "counting the staged bytes", func() {
		if size := manager.GetTotalStagingSize(); size != 10 {
			t.Errorf("GetTotalStagingSize() = %d, want the 10 bytes staged", size)
		}
	})
	finishes(t, "a write to another file", func() {
		if _, err := otherSession.Write([]byte(" world"), 5); err != nil {
			t.Errorf("Write() to another file error = %v", err)
		}
		if err := manager.MarkDirty(other, otherSession.GetSize()); err != nil {
			t.Errorf("MarkDirty() of another file error = %v", err)
		}
	})
	finishes(t, "an open and a close of the busy file", func() {
		session, err := manager.GetOrCreateSession(busy)
		if err != nil {
			t.Errorf("GetOrCreateSession() error = %v", err)
			return
		}
		if refs := session.GetRefCount(); refs != 2 {
			t.Errorf("references after a second open = %d, want 2", refs)
		}
		manager.ReleaseSession(busy)
	})
	finishes(t, "an open of another file", func() {
		if _, err := manager.GetOrCreateSession("/third.txt"); err != nil {
			t.Errorf("GetOrCreateSession() of another file error = %v", err)
		}
	})
	finishes(t, "the manager's statistics", func() { manager.Stats() })

	release()
	if refs := busySession.GetRefCount(); refs != 1 {
		t.Fatalf("references after the open was released = %d, want 1", refs)
	}
	manager.ReleaseSession(busy)
	manager.ReleaseSession(busy)
	if refs := busySession.GetRefCount(); refs != 0 {
		t.Fatalf("references after more releases than opens = %d, want 0", refs)
	}
}
