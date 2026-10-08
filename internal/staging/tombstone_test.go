package staging

import (
	"os"
	"testing"
	"time"
)

func writeDirtyTestFile(t *testing.T, manager *StagingManager, path, payload string) *WriteSession {
	t.Helper()

	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession(%s) error = %v", path, err)
	}
	if _, err := session.Write([]byte(payload), 0); err != nil {
		t.Fatalf("session.Write(%s) error = %v", path, err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("session.Sync(%s) error = %v", path, err)
	}
	manager.MarkDirty(path, session.Size)
	manager.ReleaseSession(path)
	return session
}

func TestRegisterPendingDeleteImmediateWhenNotSyncing(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/tomb-immediate.txt"
	session := writeDirtyTestFile(t, manager, path, "dirty payload")

	immediate, err := manager.RegisterPendingDelete(path)
	if err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}
	if !immediate {
		t.Fatal("RegisterPendingDelete() immediate = false, want true for non-syncing path")
	}
	if manager.IsDirty(path) {
		t.Fatal("path should not remain dirty after accepted delete")
	}
	if !manager.HasPendingDelete(path) {
		t.Fatal("tombstone should remain until COS delete is confirmed")
	}
	if _, err := os.Stat(session.StagingPath); !os.IsNotExist(err) {
		t.Fatalf("staged data should be removed, stat err = %v", err)
	}
	if _, exists := manager.GetSession(path); exists {
		t.Fatal("session should be cleaned up after accepted delete")
	}

	manager.ResolvePendingDelete(path)
	if manager.HasPendingDelete(path) {
		t.Fatal("tombstone should be gone after resolve")
	}
	if _, err := os.Stat(manager.tombstoneFilePath(path)); !os.IsNotExist(err) {
		t.Fatalf("tombstone file should be removed after resolve, stat err = %v", err)
	}
}

func TestRegisterPendingDeleteDeferredWhileSyncing(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/tomb-deferred.txt"
	writeDirtyTestFile(t, manager, path, "dirty payload")

	// Simulate a sync worker holding the path.
	if !manager.dirtyIndex.LockFile(path) {
		t.Fatal("failed to claim sync lock for test")
	}

	immediate, err := manager.RegisterPendingDelete(path)
	if err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}
	if immediate {
		t.Fatal("RegisterPendingDelete() immediate = true, want false while path is claimed by sync")
	}
	if !manager.HasPendingDelete(path) {
		t.Fatal("tombstone should be registered")
	}

	manager.dirtyIndex.UnlockFile(path)

	// The sync worker completes the delete on its next pass.
	cosClient := NewMockCOSClient()
	worker := NewSyncWorker(manager, cosClient, cfg)
	worker.processPendingDeletes(0)

	if manager.HasPendingDelete(path) {
		t.Fatal("tombstone should be resolved after worker pass")
	}
	deletes := cosClient.GetDeletes()
	if len(deletes) != 1 || deletes[0] != "tomb-deferred.txt" {
		t.Fatalf("COS deletes = %v, want [tomb-deferred.txt]", deletes)
	}
}

func TestPendingDeleteSurvivesRestartAndSupersedesStagedData(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}

	path := "/tomb-crash.txt"
	session := writeDirtyTestFile(t, manager, path, "doomed payload")

	// Simulate a delete accepted while an upload was in flight, then a crash
	// before the deferred delete could run: the staged data file remains.
	if !manager.dirtyIndex.LockFile(path) {
		t.Fatal("failed to claim sync lock for test")
	}
	immediate, err := manager.RegisterPendingDelete(path)
	if err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}
	if immediate {
		t.Fatal("expected deferred delete while path is claimed")
	}
	if _, err := os.Stat(session.StagingPath); err != nil {
		t.Fatalf("staged data should still exist before crash: %v", err)
	}
	if err := manager.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	// Restart: the tombstone must be recovered and the staged bytes dropped
	// instead of being restored as dirty (which would resurrect the file).
	recovered, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after crash error = %v", err)
	}
	defer recovered.Shutdown()

	if !recovered.HasPendingDelete(path) {
		t.Fatal("tombstone should be recovered after restart")
	}
	if recovered.IsDirty(path) {
		t.Fatal("staged data must not be restored as dirty for a tombstoned path")
	}
	if _, err := os.Stat(session.StagingPath); !os.IsNotExist(err) {
		t.Fatalf("staged data superseded by tombstone should be removed, stat err = %v", err)
	}

	// The recovered tombstone is completed by the sync worker.
	cosClient := NewMockCOSClient()
	worker := NewSyncWorker(recovered, cosClient, cfg)
	worker.processPendingDeletes(0)

	if recovered.HasPendingDelete(path) {
		t.Fatal("recovered tombstone should be resolved after worker pass")
	}
	deletes := cosClient.GetDeletes()
	if len(deletes) != 1 || deletes[0] != "tomb-crash.txt" {
		t.Fatalf("COS deletes = %v, want [tomb-crash.txt]", deletes)
	}
}

func TestPendingDeleteRetriedWhenCOSDeleteFails(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/tomb-retry.txt"
	writeDirtyTestFile(t, manager, path, "dirty payload")
	if _, err := manager.RegisterPendingDelete(path); err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}

	cosClient := NewMockCOSClient()
	cosClient.SetError("tomb-retry.txt", os.ErrDeadlineExceeded)
	worker := NewSyncWorker(manager, cosClient, cfg)

	worker.processPendingDeletes(0)
	if !manager.HasPendingDelete(path) {
		t.Fatal("tombstone must survive a failed COS delete")
	}

	// Next pass succeeds once COS recovers.
	delete(cosClient.errors, "tomb-retry.txt")
	worker.processPendingDeletes(0)
	if manager.HasPendingDelete(path) {
		t.Fatal("tombstone should be resolved after successful retry")
	}
}

func TestSyncSkipsPathWithPendingDelete(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/tomb-no-upload.txt"
	writeDirtyTestFile(t, manager, path, "dirty payload")

	cosClient := NewMockCOSClient()
	worker := NewSyncWorker(manager, cosClient, cfg)

	// Register the delete, then attempt a sync: no upload must happen.
	if _, err := manager.RegisterPendingDelete(path); err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}
	if err := worker.syncFile(path); err != nil {
		t.Fatalf("syncFile() error = %v", err)
	}
	if _, uploaded := cosClient.GetUpload("tomb-no-upload.txt"); uploaded {
		t.Fatal("sync must not upload a path with a pending delete")
	}
}

func TestCancelPendingDeleteOnRecreate(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/tomb-recreate.txt"
	writeDirtyTestFile(t, manager, path, "old payload")
	if _, err := manager.RegisterPendingDelete(path); err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}

	// Recreating the path (new write session) must cancel the tombstone.
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if manager.HasPendingDelete(path) {
		t.Fatal("recreating the path must cancel the pending delete")
	}
	if _, err := session.Write([]byte("new payload"), 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	manager.MarkDirty(path, session.Size)
	manager.ReleaseSession(path)

	// A worker pass must not delete anything for the recreated path.
	cosClient := NewMockCOSClient()
	worker := NewSyncWorker(manager, cosClient, cfg)
	worker.processPendingDeletes(0)
	if len(cosClient.GetDeletes()) != 0 {
		t.Fatalf("COS deletes = %v, want none after cancel", cosClient.GetDeletes())
	}
	if !manager.IsDirty(path) {
		t.Fatal("recreated path should remain dirty for sync")
	}
}

func TestPendingDeleteStatsExposed(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/tomb-stats.txt"
	writeDirtyTestFile(t, manager, path, "dirty payload")
	if _, err := manager.RegisterPendingDelete(path); err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}

	stats := manager.Stats()
	if got, ok := stats["pending_deletes"].(int); !ok || got != 1 {
		t.Fatalf("stats[pending_deletes] = %v, want 1", stats["pending_deletes"])
	}

	tombs := manager.PendingDeletes()
	if len(tombs) != 1 || tombs[0].Path != path {
		t.Fatalf("PendingDeletes() = %+v, want single entry for %s", tombs, path)
	}
	if tombs[0].CreatedAt.IsZero() || time.Since(tombs[0].CreatedAt) < 0 {
		t.Fatalf("tombstone CreatedAt = %v, want recent timestamp", tombs[0].CreatedAt)
	}
}

// Made with Bob

// A delete whose cleanup was put off, as when an upload was in flight, must
// not take a file created again in the meantime for the deleted one: that
// file's staged bytes are a write the client was told was accepted.
func TestDiscardDeletedStagingLeavesFileCreatedAgain(t *testing.T) {
	manager, err := NewStagingManager(createTestConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	// A delete that stands drops everything staged for the path.
	const gone = "/gone.txt"
	deleted := writeDirtyTestFile(t, manager, gone, "old")
	if !manager.TryLockSync(gone) {
		t.Fatal("could not claim the path as an upload in flight does")
	}
	if immediate, err := manager.RegisterPendingDelete(gone); err != nil || immediate {
		t.Fatalf("RegisterPendingDelete() during an upload = %v, %v; want it put off", immediate, err)
	}
	manager.UnlockSync(gone)
	if dropped, err := manager.discardDeletedStaging(gone); err != nil || !dropped {
		t.Fatalf("discardDeletedStaging() of a deleted path = %v, %v", dropped, err)
	}
	if manager.IsDirty(gone) || sidecarExists(t, manager, gone) {
		t.Fatal("deleted path still dirty, or still has its sidecar")
	}
	if _, err := os.Stat(deleted.StagingPath); !os.IsNotExist(err) {
		t.Fatalf("staged bytes of the deleted path still there, stat error = %v", err)
	}
	if _, exists := manager.GetSession(gone); exists {
		t.Fatal("session of the deleted path still listed")
	}

	// One created again before the cleanup gets to it is left alone.
	const back = "/back.txt"
	writeDirtyTestFile(t, manager, back, "old")
	if !manager.TryLockSync(back) {
		t.Fatal("could not claim the path as an upload in flight does")
	}
	if _, err := manager.RegisterPendingDelete(back); err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}
	manager.UnlockSync(back)
	recreated := writeDirtyTestFile(t, manager, back, "new")
	if manager.HasPendingDelete(back) {
		t.Fatal("creating the file again did not cancel its delete")
	}

	if dropped, err := manager.discardDeletedStaging(back); err != nil || dropped {
		t.Fatalf("discardDeletedStaging() of a path created again = %v, %v; want nothing dropped", dropped, err)
	}
	if !manager.IsDirty(back) || !sidecarExists(t, manager, back) {
		t.Fatal("file created again lost its dirty entry or its sidecar")
	}
	if data, err := os.ReadFile(recreated.StagingPath); err != nil || string(data) != "new" {
		t.Fatalf("staged bytes of the file created again = %q, %v", data, err)
	}
	if current, _ := manager.GetSession(back); current != recreated {
		t.Fatal("session of the file created again is no longer listed")
	}
}
