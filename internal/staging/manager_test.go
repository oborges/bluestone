package staging

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oborges/bluestone/internal/config"
)

func createTestConfig(t *testing.T) *config.StagingConfig {
	tmpDir := t.TempDir()

	return &config.StagingConfig{
		Enabled:          true,
		RootDir:          tmpDir,
		SyncInterval:     "30s",
		SyncThresholdMB:  1, // 1MB
		MaxDirtyAge:      "5m",
		SyncOnClose:      false,
		MaxStagingSizeGB: 10,
		MaxDirtyFiles:    100,
		SyncWorkerCount:  2,
		SyncQueueSize:    10,
		MaxSyncRetries:   3,
		RetryBackoffInit: "1s",
		RetryBackoffMax:  "30s",
		CleanAfterSync:   true,
		StaleFileAge:     "24h",
	}
}

func TestStagingManager_New(t *testing.T) {
	cfg := createTestConfig(t)

	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	// Verify active directory was created
	activeDir := filepath.Join(cfg.RootDir, "active")
	if _, err := os.Stat(activeDir); os.IsNotExist(err) {
		t.Error("Active directory was not created")
	}
}

func TestStagingManager_GetOrCreateSession(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	path := "/test/file.txt"

	// Create new session
	session1, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	if session1.Path != path {
		t.Errorf("Expected path %s, got %s", path, session1.Path)
	}

	if session1.GetRefCount() != 1 {
		t.Errorf("Expected RefCount 1, got %d", session1.GetRefCount())
	}

	// Get existing session
	session2, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}

	if session2 != session1 {
		t.Error("Should return same session instance")
	}

	if session2.GetRefCount() != 2 {
		t.Errorf("Expected RefCount 2, got %d", session2.GetRefCount())
	}
}

func TestStagingManager_ReleaseSession(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	path := "/test/file.txt"

	// Create session
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Release session
	manager.ReleaseSession(path)

	if session.GetRefCount() != 0 {
		t.Errorf("Expected RefCount 0, got %d", session.GetRefCount())
	}

	// Session should still exist (not cleaned up yet)
	_, exists := manager.GetSession(path)
	if !exists {
		t.Error("Session should still exist after release")
	}
}

func TestStagingManager_MarkDirty(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	path := "/test/file.txt"
	size := int64(1024)

	manager.MarkDirty(path, size)

	if !manager.IsDirty(path) {
		t.Error("File should be marked as dirty")
	}

	dirtyFiles := manager.GetDirtyFiles()
	if len(dirtyFiles) != 1 {
		t.Errorf("Expected 1 dirty file, got %d", len(dirtyFiles))
	}

	if dirtyFiles[0].Path != path {
		t.Errorf("Expected path %s, got %s", path, dirtyFiles[0].Path)
	}

	if dirtyFiles[0].Size != size {
		t.Errorf("Expected size %d, got %d", size, dirtyFiles[0].Size)
	}
}

func TestStagingManager_MarkClean(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	path := "/test/file.txt"

	manager.MarkDirty(path, 1024)
	manager.MarkClean(path)

	if manager.IsDirty(path) {
		t.Error("File should not be dirty after marking clean")
	}

	dirtyFiles := manager.GetDirtyFiles()
	if len(dirtyFiles) != 0 {
		t.Errorf("Expected 0 dirty files, got %d", len(dirtyFiles))
	}
}

func TestStagingManager_RecordConflictPreservesLocalStagedContent(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	path := "/test/conflicted.txt"
	localData := []byte("local dirty data")

	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write(localData, 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	activePath := session.StagingPath
	manager.MarkDirty(path, int64(len(localData)))

	conflict, err := manager.RecordConflict(path, ExternalChangeSnapshot{
		ObjectKey:    "test/conflicted.txt",
		Size:         22,
		ETag:         `"external"`,
		LastModified: time.Unix(200, 0),
		Reason:       "test_external_change",
	})
	if err != nil {
		t.Fatalf("RecordConflict() error = %v", err)
	}
	if conflict == nil {
		t.Fatal("RecordConflict() returned nil conflict")
	}

	preserved, err := os.ReadFile(conflict.PreservedPath)
	if err != nil {
		t.Fatalf("Read preserved conflict file error = %v", err)
	}
	if string(preserved) != string(localData) {
		t.Fatalf("preserved data = %q, want %q", preserved, localData)
	}
	if _, err := os.Stat(conflict.PreservedMetadataPath); err != nil {
		t.Fatalf("conflict metadata was not written: %v", err)
	}
	if _, err := os.Stat(activePath); !os.IsNotExist(err) {
		t.Fatalf("active staging file should be removed after preservation, stat err = %v", err)
	}
	if manager.IsDirty(path) {
		t.Fatal("conflicted path should be removed from dirty sync queue")
	}
	if !manager.IsConflicted(path) {
		t.Fatal("path should have an unresolved conflict record")
	}
	if _, exists := manager.GetSession(path); exists {
		t.Fatal("conflicted path should not expose the old staging session")
	}

	count, paths, last := manager.ConflictStats()
	if count != 1 || len(paths) != 1 || paths[0] != path {
		t.Fatalf("ConflictStats() = count %d paths %v, want one %s", count, paths, path)
	}
	if last.IsZero() {
		t.Fatal("last conflict time should be set")
	}
}

func TestStagingManager_GetSession(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	path := "/test/file.txt"

	// Get non-existent session
	_, exists := manager.GetSession(path)
	if exists {
		t.Error("Session should not exist")
	}

	// Create session
	session1, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Get existing session
	session2, exists := manager.GetSession(path)
	if !exists {
		t.Error("Session should exist")
	}

	if session2 != session1 {
		t.Error("Should return same session instance")
	}
}

func TestStagingManager_CleanupSession(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	path := "/test/file.txt"

	// Create session and write data
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	data := []byte("Hello, World!")
	session.Write(data, 0)
	session.Sync()

	stagingPath := session.StagingPath

	// Cleanup with file deletion
	err = manager.CleanupSession(path, true)
	if err != nil {
		t.Fatalf("Failed to cleanup session: %v", err)
	}

	// Session should not exist
	_, exists := manager.GetSession(path)
	if exists {
		t.Error("Session should not exist after cleanup")
	}

	// Staging file should be deleted
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Error("Staging file should be deleted")
	}
}

func TestStagingManager_CleanupSessionDeleteClearsDirtyEntry(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	path := "/test/deleted-file.txt"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	data := []byte("delete me before sync")
	if _, err := session.Write(data, 0); err != nil {
		t.Fatalf("Failed to write staging data: %v", err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("Failed to sync staging data: %v", err)
	}
	manager.MarkDirty(path, int64(len(data)))

	if err := manager.CleanupSession(path, true); err != nil {
		t.Fatalf("Failed to cleanup session: %v", err)
	}

	if manager.IsDirty(path) {
		t.Fatal("Deleted staging file should not remain in dirty index")
	}

	depth, bytes, _ := manager.SyncQueueStats()
	if depth != 0 || bytes != 0 {
		t.Fatalf("Expected empty sync queue, got depth=%d bytes=%d", depth, bytes)
	}
}

func TestStagingManager_CleanupSessionKeepFile(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	path := "/test/file.txt"

	// Create session
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	stagingPath := session.StagingPath

	// Cleanup without file deletion
	err = manager.CleanupSession(path, false)
	if err != nil {
		t.Fatalf("Failed to cleanup session: %v", err)
	}

	// Staging file should still exist
	if _, err := os.Stat(stagingPath); os.IsNotExist(err) {
		t.Error("Staging file should exist")
	}

	// Clean up
	os.Remove(stagingPath)
}

func TestStagingManager_Shutdown(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}

	// Create multiple sessions
	paths := []string{"/test/file1.txt", "/test/file2.txt", "/test/file3.txt"}
	for _, path := range paths {
		_, err := manager.GetOrCreateSession(path)
		if err != nil {
			t.Fatalf("Failed to create session: %v", err)
		}
	}

	// Shutdown
	err = manager.Shutdown()
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	// All sessions should be closed
	stats := manager.Stats()
	totalSessions := stats["total_sessions"].(int)
	if totalSessions != 0 {
		t.Errorf("Expected 0 sessions after shutdown, got %d", totalSessions)
	}
}

func TestStagingManager_Stats(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	// Create sessions
	session1, _ := manager.GetOrCreateSession("/test/file1.txt")
	session2, _ := manager.GetOrCreateSession("/test/file2.txt")
	manager.GetOrCreateSession("/test/file3.txt")

	// Release one session
	manager.ReleaseSession("/test/file1.txt")

	// Mark files as dirty
	manager.MarkDirty("/test/file1.txt", 1024)
	manager.MarkDirty("/test/file2.txt", 2048)

	stats := manager.Stats()

	totalSessions := stats["total_sessions"].(int)
	if totalSessions != 3 {
		t.Errorf("Expected 3 total sessions, got %d", totalSessions)
	}

	activeSessions := stats["active_sessions"].(int)
	if activeSessions != 2 { // file2 and file3 have RefCount > 0
		t.Errorf("Expected 2 active sessions, got %d", activeSessions)
	}

	dirtyFiles := stats["dirty_files"].(int)
	if dirtyFiles != 2 {
		t.Errorf("Expected 2 dirty files, got %d", dirtyFiles)
	}

	// Clean up refs
	session1.DecrementRefCount()
	session2.DecrementRefCount()
}

func TestStagingManager_MultipleSessions(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	// Create multiple sessions
	numSessions := 10
	for i := 0; i < numSessions; i++ {
		path := "/test/file" + string(rune(i)) + ".txt"
		_, err := manager.GetOrCreateSession(path)
		if err != nil {
			t.Fatalf("Failed to create session %d: %v", i, err)
		}
	}

	stats := manager.Stats()
	totalSessions := stats["total_sessions"].(int)
	if totalSessions != numSessions {
		t.Errorf("Expected %d sessions, got %d", numSessions, totalSessions)
	}
}

func TestStagingManager_StagingFilePath(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager.Shutdown()

	// Create two sessions with different paths
	session1, _ := manager.GetOrCreateSession("/test/file1.txt")
	session2, _ := manager.GetOrCreateSession("/test/file2.txt")

	// Staging paths should be different
	if session1.StagingPath == session2.StagingPath {
		t.Error("Different logical paths should have different staging paths")
	}

	// Staging paths should be in active directory
	activeDir := filepath.Join(cfg.RootDir, "active")
	if filepath.Dir(session1.StagingPath) != activeDir {
		t.Errorf("Staging path should be in active directory: %s", session1.StagingPath)
	}

	// Same path should get same staging path
	session3, _ := manager.GetOrCreateSession("/test/file1.txt")
	if session3.StagingPath != session1.StagingPath {
		t.Error("Same logical path should have same staging path")
	}
}

func TestStagingManager_RecoverFromDisk(t *testing.T) {
	cfg := createTestConfig(t)

	// Create first manager and add some files
	manager1, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}

	session, _ := manager1.GetOrCreateSession("/test/file.txt")
	session.Write([]byte("test data"), 0)
	session.Sync()

	manager1.Shutdown()

	// Create second manager (should recover)
	manager2, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager2.Shutdown()

	// Recovery should not fail (though it may not fully restore state in MVP)
	// This test mainly ensures RecoverFromDisk doesn't panic
}

func TestStagingManager_RecoverFromDiskRestoresPathMetadataState(t *testing.T) {
	cfg := createTestConfig(t)
	path := "/test/recovered-state.txt"
	data := []byte("durable dirty bytes")
	observedLastModified := time.Unix(1234, 0).UTC()

	manager1, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}

	session, err := manager1.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write(data, 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	manager1.MarkDirty(path, int64(len(data)))

	state, err := readPathMetadataState(session.StagingPath + ".metadata")
	if err != nil {
		t.Fatalf("readPathMetadataState() error = %v", err)
	}
	state.ObservedETag = `"before-write"`
	state.ObservedSize = 99
	state.ObservedLastModified = observedLastModified
	if err := writePathMetadataState(session.StagingPath+".metadata", state); err != nil {
		t.Fatalf("writePathMetadataState() error = %v", err)
	}

	if err := manager1.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	manager2, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager2.Shutdown()

	if !manager2.IsDirty(path) {
		t.Fatal("Recovered path should be dirty")
	}
	dirty := manager2.GetDirtyFiles()
	if len(dirty) != 1 {
		t.Fatalf("dirty files = %d, want 1", len(dirty))
	}
	got := dirty[0]
	if got.Path != path || got.ObjectKey != "test/recovered-state.txt" {
		t.Fatalf("recovered path/object key = %q/%q", got.Path, got.ObjectKey)
	}
	if got.ObservedETag != `"before-write"` || got.ObservedSize != 99 || !got.ObservedLastModified.Equal(observedLastModified) {
		t.Fatalf("observed state = etag %q size %d last_modified %v", got.ObservedETag, got.ObservedSize, got.ObservedLastModified)
	}
	if got.LocalDirtyGeneration != state.LocalDirtyGeneration {
		t.Fatalf("dirty generation = %d, want %d", got.LocalDirtyGeneration, state.LocalDirtyGeneration)
	}
	if got.StagedPath != session.StagingPath {
		t.Fatalf("staged path = %q, want %q", got.StagedPath, session.StagingPath)
	}
	if got.ConflictStatus != ConflictStatusNone {
		t.Fatalf("conflict status = %q, want %q", got.ConflictStatus, ConflictStatusNone)
	}
}

func TestStagingManager_RecoverFromDiskRemovesStaleMetadata(t *testing.T) {
	cfg := createTestConfig(t)
	path := "/test/stale-metadata.txt"

	manager1, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	session, err := manager1.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("stale"), 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	manager1.MarkDirty(path, session.Size)
	stagingPath := session.StagingPath
	metadataPath := stagingPath + ".metadata"
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatalf("metadata should exist before stale recovery test: %v", err)
	}
	if err := manager1.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := os.Remove(stagingPath); err != nil {
		t.Fatalf("failed to remove staged data file: %v", err)
	}

	manager2, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager2.Shutdown()

	if manager2.IsDirty(path) {
		t.Fatal("stale metadata without staged data should not recover as dirty")
	}
	if _, err := os.Stat(metadataPath); !os.IsNotExist(err) {
		t.Fatalf("stale metadata should be removed, stat err = %v", err)
	}
}

func TestStagingManager_RecoverFromDiskRestoresConflictState(t *testing.T) {
	cfg := createTestConfig(t)
	path := "/test/recovered-conflict.txt"

	manager1, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	session, err := manager1.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("local conflicted bytes"), 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	manager1.MarkDirty(path, session.Size)
	conflict, err := manager1.RecordConflict(path, ExternalChangeSnapshot{
		ObjectKey:    "test/recovered-conflict.txt",
		Size:         42,
		ETag:         `"external-conflict"`,
		LastModified: time.Unix(5678, 0).UTC(),
		Reason:       "test_recovery",
	})
	if err != nil {
		t.Fatalf("RecordConflict() error = %v", err)
	}
	if conflict == nil {
		t.Fatal("RecordConflict() returned nil")
	}
	if err := manager1.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	manager2, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("Failed to create staging manager: %v", err)
	}
	defer manager2.Shutdown()

	if !manager2.IsConflicted(path) {
		t.Fatal("conflict should be recovered after restart")
	}
	count, paths, _ := manager2.ConflictStats()
	if count != 1 || len(paths) != 1 || paths[0] != path {
		t.Fatalf("ConflictStats() = count %d paths %v, want one %s", count, paths, path)
	}
	recovered := manager2.GetConflicts()[0]
	if recovered.ConflictStatus != ConflictStatusConflicted {
		t.Fatalf("conflict status = %q, want %q", recovered.ConflictStatus, ConflictStatusConflicted)
	}
	if recovered.ObservedETag != `"external-conflict"` || recovered.ObservedSize != 42 {
		t.Fatalf("observed conflict state = etag %q size %d", recovered.ObservedETag, recovered.ObservedSize)
	}
}

// Made with Bob
