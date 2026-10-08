package staging

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

func readStagedBytes(t *testing.T, manager *StagingManager, path string, size int) string {
	t.Helper()

	session, exists := manager.GetSession(path)
	if !exists {
		t.Fatalf("no session for %s", path)
	}
	buf := make([]byte, size)
	if _, err := session.Read(buf, 0); err != nil {
		t.Fatalf("read staged bytes for %s: %v", path, err)
	}
	return string(buf)
}

func TestRenameStagedPathMovesStateAndTombstonesSource(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	oldPath := "/rename-src.txt"
	newPath := "/rename-dst.txt"
	payload := "rename payload"
	writeDirtyTestFile(t, manager, oldPath, payload)
	oldStaging := manager.stagingFilePath(oldPath)
	newStaging := manager.stagingFilePath(newPath)

	if err := manager.RenameStagedPath(oldPath, newPath); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}

	if manager.IsDirty(oldPath) {
		t.Fatal("source must not stay dirty")
	}
	if !manager.IsDirty(newPath) {
		t.Fatal("destination must be dirty")
	}
	if !manager.HasPendingDelete(oldPath) {
		t.Fatal("source must have a pending delete tombstone")
	}
	if manager.HasPendingDelete(newPath) {
		t.Fatal("destination must not have a pending delete")
	}
	if _, err := os.Stat(oldStaging); !os.IsNotExist(err) {
		t.Fatalf("source staging file should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(manager.pathMetadataPath(oldStaging)); !os.IsNotExist(err) {
		t.Fatalf("source sidecar should be gone, stat err = %v", err)
	}
	state, err := readPathMetadataState(manager.pathMetadataPath(newStaging))
	if err != nil {
		t.Fatalf("read destination sidecar: %v", err)
	}
	if state.OriginalPath != newPath {
		t.Fatalf("destination sidecar original_path = %q, want %q", state.OriginalPath, newPath)
	}
	if got := readStagedBytes(t, manager, newPath, len(payload)); got != payload {
		t.Fatalf("moved staged bytes = %q, want %q", got, payload)
	}
	if meta := manager.dirtyIndex.GetMetadata(newPath); meta == nil || meta.ObjectKey != "rename-dst.txt" {
		t.Fatalf("destination dirty metadata = %+v, want object key rename-dst.txt", meta)
	}
}

func TestRenameStagedPathWhileSourceSyncKeepsClaimAndDefers(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	oldPath := "/rename-syncing-src.txt"
	newPath := "/rename-syncing-dst.txt"
	writeDirtyTestFile(t, manager, oldPath, "in-flight payload")

	// Simulate a sync worker mid-upload of the source.
	if !manager.dirtyIndex.LockFile(oldPath) {
		t.Fatal("failed to claim sync lock for test")
	}

	if err := manager.RenameStagedPath(oldPath, newPath); err != nil {
		t.Fatalf("RenameStagedPath() during source sync error = %v", err)
	}

	// The worker's claim must be intact so the tombstone processor cannot
	// delete the old object before the in-flight upload lands.
	if !manager.dirtyIndex.IsSyncing(oldPath) {
		t.Fatal("in-flight sync claim on the source was released by the rename")
	}
	if !manager.HasPendingDelete(oldPath) {
		t.Fatal("source must have a pending delete tombstone")
	}
	if !manager.IsDirty(newPath) {
		t.Fatal("destination must be dirty")
	}

	// While the upload is in flight, tombstone processing must skip the path.
	cosClient := NewMockCOSClient()
	worker := NewSyncWorker(manager, cosClient, cfg)
	worker.processPendingDeletes(0)
	if len(cosClient.GetDeletes()) != 0 {
		t.Fatalf("COS deletes = %v, want none while sync claim is held", cosClient.GetDeletes())
	}
	if !manager.HasPendingDelete(oldPath) {
		t.Fatal("tombstone must survive while the sync claim is held")
	}

	// Once the upload finishes (claim released), the tombstone completes.
	manager.dirtyIndex.UnlockFile(oldPath)
	worker.processPendingDeletes(0)
	deletes := cosClient.GetDeletes()
	if len(deletes) != 1 || deletes[0] != "rename-syncing-src.txt" {
		t.Fatalf("COS deletes = %v, want [rename-syncing-src.txt]", deletes)
	}
	if manager.HasPendingDelete(oldPath) {
		t.Fatal("tombstone should be resolved after the deferred delete")
	}
}

func TestRenameStagedPathReplacesDirtyDestination(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	oldPath := "/rename-replace-src.txt"
	newPath := "/rename-replace-dst.txt"
	writeDirtyTestFile(t, manager, newPath, "doomed destination bytes")
	writeDirtyTestFile(t, manager, oldPath, "winning source bytes!!")

	// A pending delete on the destination is superseded by the rename.
	if _, err := manager.RegisterPendingDelete(newPath); err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}
	// Re-stage destination bytes after the delete to model dirty state.
	writeDirtyTestFile(t, manager, newPath, "doomed destination bytes")

	if err := manager.RenameStagedPath(oldPath, newPath); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}

	if manager.HasPendingDelete(newPath) {
		t.Fatal("destination pending delete must be canceled by the rename")
	}
	payload := "winning source bytes!!"
	if got := readStagedBytes(t, manager, newPath, len(payload)); got != payload {
		t.Fatalf("destination staged bytes = %q, want %q", got, payload)
	}
	if !manager.IsDirty(newPath) {
		t.Fatal("destination must be dirty with the moved bytes")
	}
	if manager.IsDirty(oldPath) {
		t.Fatal("source must not stay dirty")
	}
}

func TestRenameStagedPathSurvivesRestart(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}

	oldPath := "/rename-crash-src.txt"
	newPath := "/rename-crash-dst.txt"
	payload := "crash-safe rename bytes"
	writeDirtyTestFile(t, manager, oldPath, payload)

	if err := manager.RenameStagedPath(oldPath, newPath); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}
	if err := manager.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	recovered, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after crash error = %v", err)
	}
	defer recovered.Shutdown()

	if recovered.IsDirty(oldPath) {
		t.Fatal("source must not be restored as dirty after restart")
	}
	if !recovered.IsDirty(newPath) {
		t.Fatal("destination must be restored as dirty after restart")
	}
	if !recovered.HasPendingDelete(oldPath) {
		t.Fatal("source tombstone must be recovered after restart")
	}
	if got := readStagedBytes(t, recovered, newPath, len(payload)); got != payload {
		t.Fatalf("recovered staged bytes = %q, want %q", got, payload)
	}
}

func TestRenameStagedPathWithoutStagedBytesFallsBack(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	oldPath := "/rename-stale-src.txt"
	// Dirty entry without staged bytes on disk (stale bookkeeping).
	manager.dirtyIndex.MarkDirty(oldPath, 42)

	err = manager.RenameStagedPath(oldPath, "/rename-stale-dst.txt")
	if !os.IsNotExist(err) {
		t.Fatalf("RenameStagedPath() error = %v, want not-exist for stale dirty entry", err)
	}
	if manager.IsDirty(oldPath) {
		t.Fatal("stale dirty entry should be dropped")
	}
	if manager.HasPendingDelete(oldPath) {
		t.Fatal("no tombstone should be registered for a failed rename")
	}
}

// syncAwayBeforeMove makes the next staged-file move find its source gone:
// what happens when a sync worker finishes uploading the file, and removes
// its staged bytes, after the rename has checked for them.
func syncAwayBeforeMove(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { moveStagedFile = os.Rename })
	moveStagedFile = func(oldStaging, newStaging string) error {
		if err := os.Remove(oldStaging); err != nil {
			t.Errorf("remove staged bytes: %v", err)
		}
		return os.Rename(oldStaging, newStaging)
	}
}

// A sync worker that finishes uploading a file removes its staged bytes. When
// that lands between the rename's check for them and its move, the rename
// must report a source without staged bytes, as it does when the check itself
// finds none: the caller then renames the object in the bucket. It used to
// report a failure, and a directory rename gave up over it.
func TestRenameStagedPathReportsBytesSyncedAwayMidRename(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	oldPath := "/rename-synced-src.txt"
	newPath := "/rename-synced-dst.txt"
	writeDirtyTestFile(t, manager, oldPath, "payload")
	newSidecar := manager.pathMetadataPath(manager.stagingFilePath(newPath))

	syncAwayBeforeMove(t)
	err = manager.RenameStagedPath(oldPath, newPath)
	if !os.IsNotExist(err) {
		t.Fatalf("RenameStagedPath() error = %v, want not-exist for a source whose staged bytes were synced away", err)
	}

	if _, err := os.Stat(newSidecar); !os.IsNotExist(err) {
		t.Fatalf("destination sidecar should be taken back, stat err = %v", err)
	}
	if manager.IsDirty(newPath) {
		t.Fatal("destination must not be dirty: nothing moved there")
	}
	if manager.IsDirty(oldPath) {
		t.Fatal("source dirty entry should be dropped: its bytes are gone")
	}
	if manager.HasPendingDelete(oldPath) {
		t.Fatal("no tombstone should be registered for a rename that did not happen")
	}
}

// The destination of such a rename may have staged bytes of its own. Its
// sidecar, overwritten by the rename's intent, must come back as it was.
func TestRenameStagedPathRestoresDestinationSidecarWhenBytesGoMidRename(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	oldPath := "/rename-synced-over-src.txt"
	newPath := "/rename-synced-over-dst.txt"
	destinationPayload := "destination"
	writeDirtyTestFile(t, manager, oldPath, "a longer source payload")
	writeDirtyTestFile(t, manager, newPath, destinationPayload)
	newSidecar := manager.pathMetadataPath(manager.stagingFilePath(newPath))
	before, err := os.ReadFile(newSidecar)
	if err != nil {
		t.Fatalf("read destination sidecar: %v", err)
	}

	syncAwayBeforeMove(t)
	err = manager.RenameStagedPath(oldPath, newPath)
	if !os.IsNotExist(err) {
		t.Fatalf("RenameStagedPath() error = %v, want not-exist for a source whose staged bytes were synced away", err)
	}

	after, err := os.ReadFile(newSidecar)
	if err != nil {
		t.Fatalf("read destination sidecar after the rename: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("destination sidecar changed:\nbefore %s\nafter  %s", before, after)
	}
	if !manager.IsDirty(newPath) {
		t.Fatal("destination must stay dirty: its own bytes are still staged")
	}
	if got := readStagedBytes(t, manager, newPath, len(destinationPayload)); got != destinationPayload {
		t.Fatalf("destination staged bytes = %q, want %q", got, destinationPayload)
	}
}

func TestRenameStagedPathMovedBytesSyncToNewKey(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	oldPath := "/rename-sync-src.txt"
	newPath := "/rename-sync-dst.txt"
	payload := "bytes that sync to the new key"
	writeDirtyTestFile(t, manager, oldPath, payload)

	if err := manager.RenameStagedPath(oldPath, newPath); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}

	cosClient := NewMockCOSClient()
	worker := NewSyncWorker(manager, cosClient, cfg)
	if err := worker.syncFile(newPath); err != nil {
		t.Fatalf("syncFile() error = %v", err)
	}

	uploaded, ok := cosClient.GetUpload(newPath)
	if !ok {
		t.Fatal("moved bytes were not uploaded to the destination key")
	}
	if string(uploaded) != payload {
		t.Fatalf("uploaded bytes = %q, want %q", uploaded, payload)
	}
	if _, ok := cosClient.GetUpload(oldPath); ok {
		t.Fatal("nothing must be uploaded to the source key")
	}
	if manager.IsDirty(newPath) {
		t.Fatal("destination should be clean after sync")
	}
}

func TestSyncWorkerNotifiesObjectMutated(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	cosClient := NewMockCOSClient()
	worker := NewSyncWorker(manager, cosClient, cfg)
	var mutated, synced []string
	worker.SetObjectMutatedCallback(func(path string) {
		mutated = append(mutated, path)
	})
	worker.SetObjectSyncedCallback(func(path string) {
		synced = append(synced, path)
	})

	// A successful sync upload must notify the synced hook, so caches
	// poisoned during the dirty window (e.g. a zero-byte truncate object)
	// get invalidated without purging ancestor listings.
	syncPath := "/mutate-sync.txt"
	writeDirtyTestFile(t, manager, syncPath, "sync payload")
	if err := worker.syncFile(syncPath); err != nil {
		t.Fatalf("syncFile() error = %v", err)
	}
	if len(synced) != 1 || synced[0] != syncPath {
		t.Fatalf("synced after sync = %v, want [%s]", synced, syncPath)
	}
	if len(mutated) != 0 {
		t.Fatalf("mutated after sync = %v, want none (uploads use the synced hook)", mutated)
	}

	// A deferred delete changes the namespace and must notify the mutated
	// hook.
	deletePath := "/mutate-delete.txt"
	writeDirtyTestFile(t, manager, deletePath, "delete payload")
	if _, err := manager.RegisterPendingDelete(deletePath); err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}
	worker.processPendingDeletes(0)
	if len(mutated) != 1 || mutated[0] != deletePath {
		t.Fatalf("mutated after deferred delete = %v, want [%s]", mutated, deletePath)
	}
}

// Made with Bob

// A rename over a destination whose own upload is in flight: when that
// upload finishes it must not mark the destination clean. The entry is the
// renamed file's by then, and cleaning it would remove the moved bytes and
// their sidecar without their having been uploaded.
func TestRenameOverDestinationDuringItsUploadKeepsMovedBytes(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	oldPath := "/rename-upload-src.txt"
	newPath := "/rename-upload-dst.txt"
	// The destination is written twice and the source once, so that
	// counting writes per file would give the renamed entry the number the
	// destination's upload holds.
	writeDirtyTestFile(t, manager, newPath, "destination, first")
	writeDirtyTestFile(t, manager, newPath, "destination, again")
	writeDirtyTestFile(t, manager, oldPath, "moved bytes")

	cosClient := &duringPutCOSClient{MockCOSClient: NewMockCOSClient()}
	cosClient.duringPut = func() {
		if err := manager.RenameStagedPath(oldPath, newPath); err != nil {
			t.Errorf("RenameStagedPath() during the destination's upload error = %v", err)
		}
	}
	worker := NewSyncWorker(manager, cosClient, cfg)

	if err := syncClaimed(t, manager, worker, newPath); err == nil {
		t.Fatal("sync reported success for a destination replaced while it uploaded")
	}
	if !manager.IsDirty(newPath) {
		t.Fatal("renamed file marked clean by the upload of the file it replaced")
	}
	if state := sidecarOf(t, manager, newPath); state.OriginalPath != newPath {
		t.Fatalf("destination sidecar names %q, want %q", state.OriginalPath, newPath)
	}
	if got := readStagedBytes(t, manager, newPath, len("moved bytes")); got != "moved bytes" {
		t.Fatalf("destination staged bytes = %q, want the moved bytes", got)
	}
	if err := manager.CommitPath(newPath); err != nil {
		t.Fatalf("CommitPath() of the renamed file error = %v", err)
	}

	if err := syncClaimed(t, manager, worker, newPath); err != nil {
		t.Fatalf("second sync error = %v", err)
	}
	if uploaded, _ := cosClient.GetUpload(newPath); string(uploaded) != "moved bytes" {
		t.Fatalf("upload after the second sync = %q, want the moved bytes", uploaded)
	}
}

// The destination's upload can also finish in the middle of the rename,
// after the rename has written the destination's new sidecar and before it
// has moved the dirty entry. Marking the destination clean then would take
// that sidecar with it.
func TestRenameKeepsDestinationSidecarWhenItsUploadFinishesMidRename(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	oldPath := "/rename-midway-src.txt"
	newPath := "/rename-midway-dst.txt"
	writeDirtyTestFile(t, manager, newPath, "destination")
	writeDirtyTestFile(t, manager, oldPath, "moved bytes")
	uploading := manager.dirtyIndex.GetMetadata(newPath).LocalDirtyGeneration

	t.Cleanup(func() { moveStagedFile = os.Rename })
	moveStagedFile = func(oldStaging, newStaging string) error {
		if manager.markCleanIfUnchanged(newPath, uploading) {
			t.Error("destination marked clean in the middle of a rename over it")
		}
		return os.Rename(oldStaging, newStaging)
	}
	if err := manager.RenameStagedPath(oldPath, newPath); err != nil {
		t.Fatalf("RenameStagedPath() error = %v", err)
	}

	if !manager.IsDirty(newPath) {
		t.Fatal("destination must be dirty with the moved bytes")
	}
	if state := sidecarOf(t, manager, newPath); state.OriginalPath != newPath {
		t.Fatalf("destination sidecar names %q, want %q", state.OriginalPath, newPath)
	}
	if err := manager.CommitPath(newPath); err != nil {
		t.Fatalf("CommitPath() of the renamed file error = %v", err)
	}
}

// A rename that does not happen leaves the destination as it was. When the
// destination is dirty and what it had for a sidecar could not be read back,
// there is nothing to put back: it gets a sidecar written from memory
// rather than none, since nothing else would write one for a file that is
// already dirty.
func TestFailedRenameLeavesDirtyDestinationWithSidecar(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	stageWrite(t, manager, "/src.txt", "moved")
	kept := stageWrite(t, manager, "/dst.txt", "kept")
	kept.SetMode(0640)
	sidecar := manager.pathMetadataPath(manager.stagingFilePath("/dst.txt"))
	if err := os.WriteFile(sidecar, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { moveStagedFile = os.Rename })
	moveStagedFile = func(string, string) error { return errors.New("disk gone") }
	if err := manager.RenameStagedPath("/src.txt", "/dst.txt"); err == nil {
		t.Fatal("RenameStagedPath() succeeded though the staged bytes could not be moved")
	}

	state := sidecarOf(t, manager, "/dst.txt")
	if state.OriginalPath != "/dst.txt" || state.Attributes == nil || state.Attributes.Mode != 0640 {
		t.Fatalf("destination sidecar after the failed rename = path %q attributes %+v", state.OriginalPath, state.Attributes)
	}
	if !manager.IsDirty("/dst.txt") {
		t.Fatal("destination no longer dirty after the failed rename")
	}
	if data, err := os.ReadFile(manager.stagingFilePath("/dst.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("destination bytes after the failed rename = %q, %v", data, err)
	}
	if err := manager.CommitPath("/dst.txt"); err != nil {
		t.Fatalf("CommitPath() of the destination error = %v", err)
	}
}
