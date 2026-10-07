package staging

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
)

// duringPutCOSClient runs duringPut once, after an upload has read the staged
// bytes and before it returns, so a test can change the file while its upload
// is in flight.
type duringPutCOSClient struct {
	*MockCOSClient
	duringPut func()
}

func (c *duringPutCOSClient) PutObjectStream(ctx context.Context, path string, body io.ReadSeeker, metadata map[string]string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if hook := c.duringPut; hook != nil {
		c.duringPut = nil
		hook()
	}
	return c.MockCOSClient.PutObject(ctx, path, data, metadata)
}

func sidecarExists(t *testing.T, manager *StagingManager, path string) bool {
	t.Helper()
	_, err := os.Stat(manager.pathMetadataPath(manager.stagingFilePath(path)))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat sidecar of %s: %v", path, err)
	}
	return err == nil
}

// A write that lands while its file uploads leaves the file dirty. The upload
// does not cover the write, so marking the file clean would remove the staged
// bytes, and the sidecar a restart recovers them by, without their ever
// reaching the object store.
func TestWriteDuringUploadKeepsFileDirty(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path = "/dir/file.txt"

	session := stageWrite(t, manager, path, "first")
	if err := manager.CommitPath(path); err != nil {
		t.Fatalf("CommitPath() error = %v", err)
	}
	// No handle holds the file: a sync that takes it for clean removes its
	// staged bytes.
	manager.ReleaseSession(path)

	cosClient := &duringPutCOSClient{MockCOSClient: NewMockCOSClient()}
	cosClient.duringPut = func() {
		// What a client's WRITE and COMMIT do.
		if _, err := session.Write([]byte("second!"), 0); err != nil {
			t.Errorf("Write() during upload error = %v", err)
		}
		if err := manager.MarkDirty(path, session.GetSize()); err != nil {
			t.Errorf("MarkDirty() during upload error = %v", err)
		}
		if err := manager.CommitPath(path); err != nil {
			t.Errorf("CommitPath() during upload error = %v", err)
		}
	}
	worker := NewSyncWorker(manager, cosClient, cfg)

	if err := syncClaimed(t, manager, worker, path); err == nil {
		t.Fatal("sync reported success for a file that changed while it uploaded")
	}
	if uploaded, _ := cosClient.GetUpload(path); string(uploaded) != "first" {
		t.Fatalf("upload = %q, want the bytes staged when it started", uploaded)
	}
	if !manager.IsDirty(path) {
		t.Fatal("file marked clean though a write landed during its upload")
	}
	if state := sidecarOf(t, manager, path); !state.Committed || state.OriginalPath != path {
		t.Fatalf("sidecar after the upload = committed %v path %q, want the committed sidecar of %s",
			state.Committed, state.OriginalPath, path)
	}
	if data, err := os.ReadFile(manager.stagingFilePath(path)); err != nil || string(data) != "second!" {
		t.Fatalf("staged bytes after the upload = %q, %v", data, err)
	}

	if err := syncClaimed(t, manager, worker, path); err != nil {
		t.Fatalf("second sync error = %v", err)
	}
	if uploaded, _ := cosClient.GetUpload(path); string(uploaded) != "second!" {
		t.Fatalf("upload after the second sync = %q, want the later write", uploaded)
	}
	if manager.IsDirty(path) || sidecarExists(t, manager, path) {
		t.Fatal("file still dirty, or its sidecar still there, after an upload nothing interrupted")
	}
}

// Writes race the end of uploads. Whichever gets there first, the file is
// never left clean while it holds bytes the object store does not have.
func TestWritesRacingUploadsNeverLeaveUnsyncedFileClean(t *testing.T) {
	cfg := createTestConfig(t)
	cfg.CleanAfterSync = false
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path = "/race.txt"
	const writes = 2000

	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	cosClient := NewMockCOSClient()
	worker := NewSyncWorker(manager, cosClient, cfg)

	// staged is what the last complete write left in the file. A write holds
	// mu from its bytes to its MarkDirty, so the check below never looks at
	// one half done.
	var mu sync.Mutex
	var staged string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < writes; i++ {
			data := fmt.Sprintf("version-%06d", i)
			mu.Lock()
			if _, err := session.Write([]byte(data), 0); err != nil {
				t.Errorf("Write() error = %v", err)
			}
			if err := manager.MarkDirty(path, int64(len(data))); err != nil {
				t.Errorf("MarkDirty() error = %v", err)
			}
			staged = data
			mu.Unlock()
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
			t.Fatalf("file is clean with %q staged and %q uploaded", staged, uploaded)
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
	if uploaded, _ := cosClient.GetUpload(path); string(uploaded) != fmt.Sprintf("version-%06d", writes-1) {
		t.Fatalf("uploaded %q, want the last write", uploaded)
	}
}

// markCleanIfUnchanged is how an upload that has finished marks its file
// clean: only if the file is still the one the upload read.
func TestMarkCleanIfUnchanged(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path = "/file.txt"

	session := stageWrite(t, manager, path, "hello")
	uploading := manager.dirtyIndex.GetMetadata(path).LocalDirtyGeneration
	if _, err := session.Write([]byte("!"), 5); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := manager.MarkDirty(path, session.GetSize()); err != nil {
		t.Fatalf("MarkDirty() error = %v", err)
	}

	if manager.markCleanIfUnchanged(path, uploading) {
		t.Fatal("file written after the upload read it was marked clean")
	}
	if !manager.IsDirty(path) || !sidecarExists(t, manager, path) {
		t.Fatal("a refused clean dropped the dirty entry or the sidecar")
	}

	current := manager.dirtyIndex.GetMetadata(path).LocalDirtyGeneration
	if !manager.markCleanIfUnchanged(path, current) {
		t.Fatal("unchanged file was not marked clean")
	}
	if manager.IsDirty(path) || sidecarExists(t, manager, path) {
		t.Fatal("clean file kept its dirty entry or its sidecar")
	}

	// The path gets a new file, not yet written, while the upload of the old
	// one is still finishing. Its sidecar is not the upload's to remove.
	if _, err := os.Stat(manager.stagingFilePath(path)); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsurePathMetadata(path, manager.stagingFilePath(path)); err != nil {
		t.Fatalf("EnsurePathMetadata() error = %v", err)
	}
	if !manager.markCleanIfUnchanged(path, current) {
		t.Fatal("a path with no dirty entry was not reported clean")
	}
	if !sidecarExists(t, manager, path) {
		t.Fatal("an upload removed the sidecar of a path it found without a dirty entry")
	}
}

// Committing a file again flushes only its bytes, unless the sidecar or a
// directory entry has changed since the last commit.
func TestCommitPathFlushesMetadataOnlyWhenChanged(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	const path, renamed = "/file.txt", "/renamed.txt"

	session := stageWrite(t, manager, path, "hello")
	// metadataToCommit reports whether a commit now would flush the sidecar
	// and the directory besides the staged bytes.
	metadataToCommit := func() bool {
		t.Helper()
		_, uncommitted, err := session.syncToDisk()
		if err != nil {
			t.Fatalf("syncToDisk() error = %v", err)
		}
		return uncommitted != 0
	}
	commit := func(path string) {
		t.Helper()
		if err := manager.CommitPath(path); err != nil {
			t.Fatalf("CommitPath(%s) error = %v", path, err)
		}
		if metadataToCommit() {
			t.Fatalf("CommitPath(%s) left the sidecar or the directory to flush", path)
		}
	}

	if !metadataToCommit() {
		t.Fatal("a file never committed has no sidecar or directory to flush")
	}
	commit(path)

	if _, err := session.Write([]byte(" world"), 5); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := manager.MarkDirty(path, session.GetSize()); err != nil {
		t.Fatalf("MarkDirty() error = %v", err)
	}
	if metadataToCommit() {
		t.Fatal("a write alone gave the next commit a sidecar or a directory to flush")
	}

	session.SetMode(0640)
	if !metadataToCommit() {
		t.Fatal("a changed attribute was not left for the next commit to flush")
	}
	commit(path)

	if err := manager.RenameStagedPath(path, renamed); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}
	if !metadataToCommit() {
		t.Fatal("a rename was not left for the next commit to flush")
	}
	commit(renamed)

	// Once the file has synced its sidecar goes. A write after that starts
	// a new one, which the commit of that write has to put on disk.
	manager.MarkClean(renamed)
	if _, err := session.Write([]byte("!"), 11); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := manager.MarkDirty(renamed, session.GetSize()); err != nil {
		t.Fatalf("MarkDirty() error = %v", err)
	}
	if !metadataToCommit() {
		t.Fatal("a sidecar written anew was not left for the next commit to flush")
	}
	commit(renamed)
	if state := sidecarOf(t, manager, renamed); !state.Committed || state.OriginalPath != renamed {
		t.Fatalf("sidecar = committed %v path %q, want the committed sidecar of %s",
			state.Committed, state.OriginalPath, renamed)
	}
}

// A restart reads the sidecars it finds and leaves them as they are.
func TestRecoverFromDiskLeavesSidecarsAlone(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	const path = "/file.txt"
	stageWrite(t, manager, path, "hello")
	if err := manager.CommitPath(path); err != nil {
		t.Fatalf("CommitPath() error = %v", err)
	}
	sidecarPath := manager.pathMetadataPath(manager.stagingFilePath(path))
	before, err := os.Stat(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	manager.Shutdown()

	recovered, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after restart error = %v", err)
	}
	defer recovered.Shutdown()
	if !recovered.IsDirty(path) {
		t.Fatal("staged file not queued for sync after restart")
	}
	if after, err := os.Stat(sidecarPath); err != nil || !os.SameFile(before, after) {
		t.Fatalf("recovery rewrote the sidecar (stat error %v)", err)
	}
}
