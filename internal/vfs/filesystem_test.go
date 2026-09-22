package vfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/oborges/bluestone/internal/cache"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/feature"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/internal/staging"
	"github.com/oborges/bluestone/pkg/types"
	"go.uber.org/zap"
)

func testStagingConfig(t *testing.T) *config.StagingConfig {
	t.Helper()

	return &config.StagingConfig{
		Enabled:          true,
		RootDir:          t.TempDir(),
		SyncInterval:     "30s",
		SyncThresholdMB:  1,
		MaxDirtyAge:      "5m",
		SyncOnClose:      false,
		MaxStagingSizeGB: 20,
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

func TestFilesystemChrootPreservesRecoveredStagingSessions(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}

	path := "/crash-safe.bin"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	payload := []byte("crash recovery payload")
	if _, err := session.Write(payload, 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("session.Sync() error = %v", err)
	}
	manager.MarkDirty(path, int64(len(payload)))
	if err := manager.Shutdown(); err != nil {
		t.Fatalf("manager.Shutdown() error = %v", err)
	}

	recoveredManager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() after crash error = %v", err)
	}
	defer recoveredManager.Shutdown()
	recoveredSession, exists := recoveredManager.GetSession(path)
	if !exists {
		t.Fatalf("recovered staging session for %s not found", path)
	}
	if !recoveredSession.Dirty {
		t.Fatal("recovered staging session was not marked dirty")
	}

	perfConfig := &config.PerformanceConfig{
		WriteBufferKB:       4096,
		MaxBufferedWriteMB:  config.DefaultMaxBufferedWriteMB,
		MaxDirectoryEntries: config.DefaultMaxDirectoryEntries,
	}
	metadataCache := cache.NewMetadataCache(&config.MetadataCacheConfig{
		Enabled:    true,
		MaxEntries: 10,
		TTLSeconds: 60,
	})
	metadataCache.SetDirEntries("/", []os.FileInfo{})
	ops := posix.NewOperationsHandler(nil, metadataCache, nil, perfConfig)
	fs := NewFilesystem(
		ops,
		logging.NewKVLogger(zap.NewNop()),
		"/",
		perfConfig,
		recoveredManager,
		nil,
		&feature.FeatureFlags{UseStagingPath: true},
	)

	chrooted, err := fs.Chroot("/")
	if err != nil {
		t.Fatalf("Chroot() error = %v", err)
	}

	chrootedFS, ok := chrooted.(*Filesystem)
	if !ok {
		t.Fatalf("Chroot() returned %T, want *Filesystem", chrooted)
	}
	if chrootedFS.stagingManager != recoveredManager {
		t.Fatal("Chroot() dropped staging manager")
	}
	if chrootedFS.featureFlags == nil || !chrootedFS.featureFlags.IsStagingEnabled() {
		t.Fatal("Chroot() dropped staging feature flags")
	}

	info, err := chrootedFS.Stat(filepath.Base(path))
	if err != nil {
		t.Fatalf("Stat() after chroot/recovery error = %v", err)
	}
	if info.Size() != int64(len(payload)) {
		t.Fatalf("Stat() size = %d, want %d", info.Size(), len(payload))
	}

	entries, err := chrootedFS.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir() after chroot/recovery error = %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name() == filepath.Base(path) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ReadDir() did not include recovered staging file %s", path)
	}

	file, err := os.Open(recoveredSession.StagingPath)
	if err != nil {
		t.Fatalf("open recovered staging file: %v", err)
	}
	defer file.Close()
	recoveredPayload := make([]byte, len(payload))
	if _, err := file.ReadAt(recoveredPayload, 0); err != nil {
		t.Fatalf("read recovered staging payload: %v", err)
	}
	if string(recoveredPayload) != string(payload) {
		t.Fatalf("recovered payload = %q, want %q", recoveredPayload, payload)
	}
}

func TestFileCloseReleasesStagingSessionOnce(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/refcount-on-close.bin"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	session.IncrementRefCount()

	file := &File{
		logger:         logging.NewKVLogger(zap.NewNop()),
		path:           path,
		stagingManager: manager,
		stagingSession: session,
		featureFlags:   &feature.FeatureFlags{UseStagingPath: true},
	}

	if got := session.GetRefCount(); got != 2 {
		t.Fatalf("initial refcount = %d, want 2", got)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := session.GetRefCount(); got != 1 {
		t.Fatalf("refcount after Close() = %d, want 1", got)
	}
}

func TestFilesystemRenameDirtyStagedFileSucceeds(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/dirty.txt"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	payload := []byte("dirty data")
	if _, err := session.Write(payload, 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("session.Sync() error = %v", err)
	}
	manager.MarkDirty(path, session.Size)
	manager.ReleaseSession(path)
	oldStagingPath := session.StagingPath

	store := newFakeObjectStore()
	// Simulate a previously synced source object that must not survive.
	store.put("dirty.txt", []byte("stale synced data"))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	if err := fs.Rename("dirty.txt", "renamed.txt"); err != nil {
		t.Fatalf("Rename() of dirty staged file error = %v, want success (POSIX write-back semantics)", err)
	}

	if manager.IsDirty(path) {
		t.Fatal("source path should not remain dirty after rename")
	}
	if !manager.IsDirty("/renamed.txt") {
		t.Fatal("destination path should be dirty after rename")
	}
	movedSession, exists := manager.GetSession("/renamed.txt")
	if !exists {
		t.Fatal("staging session should have moved to the destination path")
	}
	if _, exists := manager.GetSession(path); exists {
		t.Fatal("staging session must not remain at the source path")
	}
	if _, err := os.Stat(oldStagingPath); !os.IsNotExist(err) {
		t.Fatalf("source staging file should have moved, stat err = %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := movedSession.Read(buf, 0); err != nil {
		t.Fatalf("read of moved staged bytes error = %v", err)
	}
	if string(buf) != string(payload) {
		t.Fatalf("moved staged bytes = %q, want %q", buf, payload)
	}

	// The source object was deleted inline (no sync in flight) and its
	// tombstone resolved.
	if !store.wasDeleted("dirty.txt") {
		t.Fatal("source COS object should have been deleted")
	}
	if manager.HasPendingDelete(path) {
		t.Fatal("source tombstone should be resolved after inline COS delete")
	}

	// Namespace: source gone, destination visible.
	if _, err := fs.Stat("dirty.txt"); !os.IsNotExist(err) {
		t.Fatalf("Stat(source) after rename = %v, want not-exist", err)
	}
	info, err := fs.Stat("renamed.txt")
	if err != nil {
		t.Fatalf("Stat(destination) after rename error = %v", err)
	}
	if info.Size() != int64(len(payload)) {
		t.Fatalf("Stat(destination) size = %d, want %d", info.Size(), len(payload))
	}
}

func TestFilesystemRenameCleanSourceOverDirtyDestination(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	destPath := "/dest.txt"
	session, err := manager.GetOrCreateSession(destPath)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("doomed dest data"), 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	manager.MarkDirty(destPath, session.Size)
	manager.ReleaseSession(destPath)

	store := newFakeObjectStore()
	store.put("source.txt", []byte("source content"))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	if err := fs.Rename("source.txt", "dest.txt"); err != nil {
		t.Fatalf("Rename() over dirty destination error = %v, want success", err)
	}
	if manager.IsDirty(destPath) {
		t.Fatal("destination staged bytes should be discarded by rename-over")
	}
	if _, exists := manager.GetSession(destPath); exists {
		t.Fatal("destination staging session should be discarded by rename-over")
	}
	if got := string(store.get("dest.txt")); got != "source content" {
		t.Fatalf("destination object = %q, want %q", got, "source content")
	}
	if !store.wasDeleted("source.txt") {
		t.Fatal("source object should be deleted by object-store rename")
	}
}

func TestFilesystemRenameCleanSourceOverSyncingDestinationStaysBusy(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	destPath := "/dest-syncing.txt"
	session, err := manager.GetOrCreateSession(destPath)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("doomed dest data"), 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	manager.MarkDirty(destPath, session.Size)
	manager.ReleaseSession(destPath)

	// Simulate a sync worker uploading the destination's doomed bytes.
	if !manager.TryLockSync(destPath) {
		t.Fatal("failed to claim sync lock for test")
	}
	defer manager.UnlockSync(destPath)

	store := newFakeObjectStore()
	store.put("source.txt", []byte("source content"))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	err = fs.Rename("source.txt", "dest-syncing.txt")
	if !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("Rename() over syncing destination error = %v, want EBUSY", err)
	}
	if !manager.IsDirty(destPath) {
		t.Fatal("destination staged state must be untouched by the blocked rename")
	}
}

func TestFilesystemRenameDirectoryWithDirtyChildIsBlocked(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/dir/dirty.txt"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("dirty data"), 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	manager.MarkDirty(path, session.Size)

	fs := newDirtyStagingTestFilesystem(t, manager)
	err = fs.Rename("dir", "renamed-dir")
	if !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("Rename(directory) error = %v, want EBUSY", err)
	}
	if !manager.IsDirty(path) {
		t.Fatal("dirty child path should remain dirty after blocked directory rename")
	}
}

func TestFilesystemRemoveDirtyStagedFileSucceeds(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/dirty-delete.txt"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("dirty data"), 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("session.Sync() error = %v", err)
	}
	manager.MarkDirty(path, session.Size)
	manager.ReleaseSession(path)
	stagingPath := session.StagingPath

	store := newFakeObjectStore()
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)
	if err := fs.Remove("dirty-delete.txt"); err != nil {
		t.Fatalf("Remove() of dirty staged file error = %v, want success (POSIX unlink semantics)", err)
	}
	if manager.IsDirty(path) {
		t.Fatal("path should not be dirty after accepted delete")
	}
	if manager.HasPendingDelete(path) {
		t.Fatal("pending delete should be resolved after successful COS delete")
	}
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Fatalf("staged data should be removed after accepted delete, stat err = %v", err)
	}
	if !store.wasDeleted("dirty-delete.txt") {
		t.Fatal("COS object should have been deleted")
	}
	if _, err := fs.Stat("dirty-delete.txt"); !os.IsNotExist(err) {
		t.Fatalf("Stat() after delete = %v, want not-exist", err)
	}
}

func TestFilesystemRemoveCleanFileFallsBackToTombstoneOnBackendError(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	store := newFakeObjectStore()
	store.put("clean.txt", []byte("clean synced data"))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	// Backend cannot perform deletes (outage).
	store.setDeleteErr(errors.New("dial tcp: connection refused"))

	if err := fs.Remove("clean.txt"); err != nil {
		t.Fatalf("Remove(clean file, backend down) error = %v, want tombstone-accepted delete", err)
	}
	if !manager.HasPendingDelete("/clean.txt") {
		t.Fatal("delete must be recorded as a pending tombstone")
	}
	if _, err := fs.Stat("clean.txt"); !os.IsNotExist(err) {
		t.Fatalf("Stat() after accepted delete = %v, want not-exist", err)
	}

	// The deferred COS delete is completed by the sync worker's tombstone
	// pass (covered by staging tests); here assert it is queued exactly once.
	if manager.PendingDeleteCount() != 1 {
		t.Fatalf("PendingDeleteCount() = %d, want 1", manager.PendingDeleteCount())
	}
}

func TestFilesystemHidesPendingDeletePaths(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/doomed.txt"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("doomed data"), 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	manager.MarkDirty(path, session.Size)
	manager.ReleaseSession(path)

	// Simulate the deferred state: tombstone registered but COS delete not
	// yet confirmed (as when an upload is in flight during Remove).
	if _, err := manager.RegisterPendingDelete(path); err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}
	if !manager.HasPendingDelete(path) {
		t.Fatal("expected pending delete to be registered")
	}

	store := newFakeObjectStore()
	// The object is still visible in COS until the deferred delete completes.
	store.put("doomed.txt", []byte("doomed data"))
	store.put("survivor.txt", []byte("survivor"))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	if _, err := fs.Stat("doomed.txt"); !os.IsNotExist(err) {
		t.Fatalf("Stat() of pending-delete path = %v, want not-exist", err)
	}
	if _, err := fs.Open("doomed.txt"); !os.IsNotExist(err) {
		t.Fatalf("Open() of pending-delete path = %v, want not-exist", err)
	}

	entries, err := fs.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == "doomed.txt" {
			t.Fatal("ReadDir() must not list a pending-delete path")
		}
	}
	foundSurvivor := false
	for _, entry := range entries {
		if entry.Name() == "survivor.txt" {
			foundSurvivor = true
		}
	}
	if !foundSurvivor {
		t.Fatal("ReadDir() should still list unaffected paths")
	}
}

func TestFilesystemRecreateCancelsPendingDelete(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/recreated.txt"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("old data"), 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	manager.MarkDirty(path, session.Size)
	manager.ReleaseSession(path)

	if _, err := manager.RegisterPendingDelete(path); err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}

	store := newFakeObjectStore()
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	file, err := fs.Create("recreated.txt")
	if err != nil {
		t.Fatalf("Create() over pending-delete path error = %v", err)
	}
	defer file.Close()

	if manager.HasPendingDelete(path) {
		t.Fatal("recreating the file must cancel the pending delete")
	}
	if _, err := fs.Stat("recreated.txt"); err != nil {
		t.Fatalf("Stat() after recreate error = %v", err)
	}
}

func TestFilesystemRemoveDirectoryWithDirtyChildIsBlocked(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	path := "/dir/dirty-delete.txt"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("dirty data"), 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	manager.MarkDirty(path, session.Size)

	fs := newDirtyStagingTestFilesystem(t, manager)
	err = fs.Remove("dir")
	if !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("Remove(directory) error = %v, want EBUSY", err)
	}
	if !manager.IsDirty(path) {
		t.Fatal("dirty child path should remain dirty after blocked directory remove")
	}
}

// fakeObjectStore is an in-memory posix.ObjectStore for filesystem tests. It
// is safe for concurrent use: the operations handler issues object calls from
// its own goroutines (e.g. Stat's parallel lookups).
type fakeObjectStore struct {
	mu        sync.Mutex
	objects   map[string][]byte
	deleted   map[string]bool
	deleteErr error
	// metadata holds each object's user metadata.
	metadata map[string]map[string]string
	// putCalls counts object writes, telling data rewrites from
	// metadata-only updates.
	putCalls int
	// lookupErr fails HeadObject and ListObjects, as during an outage.
	lookupErr error
	// streamErr fails GetObjectStream bodies after streamPartialBytes bytes.
	streamErr          error
	streamPartialBytes int
}

// failingReader returns its error on every read, like a dropped connection.
type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }

// failDownloads makes object bodies fail after partialBytes bytes; a nil err
// restores normal downloads.
func (s *fakeObjectStore) failDownloads(partialBytes int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamPartialBytes = partialBytes
	s.streamErr = err
}

// failLookups makes existence probes fail; a nil err restores them.
func (s *fakeObjectStore) failLookups(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupErr = err
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{
		objects:  make(map[string][]byte),
		deleted:  make(map[string]bool),
		metadata: make(map[string]map[string]string),
	}
}

func copyMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	out := make(map[string]string, len(metadata))
	for k, v := range metadata {
		out[k] = v
	}
	return out
}

// putWithMetadata stores an object with user metadata, as another tool or an
// earlier upload would have.
func (s *fakeObjectStore) putWithMetadata(key string, data []byte, metadata map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
	s.metadata[key] = copyMetadata(metadata)
}

func (s *fakeObjectStore) metadataOf(key string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyMetadata(s.metadata[key])
}

func (s *fakeObjectStore) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putCalls
}

func (s *fakeObjectStore) put(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
}

func (s *fakeObjectStore) get(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key]
}

func (s *fakeObjectStore) wasDeleted(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted[key]
}

func (s *fakeObjectStore) setDeleteErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteErr = err
}

func (s *fakeObjectStore) GetObject(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func (s *fakeObjectStore) GetObjectRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	data, err := s.GetObject(ctx, key)
	if err != nil {
		return nil, err
	}
	if offset >= int64(len(data)) {
		return nil, nil
	}
	end := offset + length
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return data[offset:end], nil
}

func (s *fakeObjectStore) GetObjectStream(ctx context.Context, key string) (io.ReadCloser, error) {
	data, err := s.GetObject(ctx, key)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	streamErr, partial := s.streamErr, s.streamPartialBytes
	s.mu.Unlock()
	if streamErr != nil {
		if partial > len(data) {
			partial = len(data)
		}
		body := io.MultiReader(bytes.NewReader(data[:partial]), &failingReader{err: streamErr})
		return io.NopCloser(body), nil
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeObjectStore) PutObject(ctx context.Context, key string, data []byte, metadata map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = append([]byte(nil), data...)
	s.metadata[key] = copyMetadata(metadata)
	s.putCalls++
	return nil
}

func (s *fakeObjectStore) DeleteObject(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.objects, key)
	s.deleted[key] = true
	return nil
}

func (s *fakeObjectStore) HeadObject(ctx context.Context, key string) (*types.ObjectMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	data, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &types.ObjectMetadata{Key: key, Size: int64(len(data)), Metadata: copyMetadata(s.metadata[key])}, nil
}

func (s *fakeObjectStore) ListObjects(ctx context.Context, prefix string, maxKeys int) ([]*types.ObjectMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	// Like COS: keys in lexical order, and maxKeys <= 0 means no limit.
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if maxKeys > 0 && len(keys) > maxKeys {
		keys = keys[:maxKeys]
	}
	result := make([]*types.ObjectMetadata, 0, len(keys))
	for _, key := range keys {
		result = append(result, &types.ObjectMetadata{Key: key, Size: int64(len(s.objects[key]))})
	}
	return result, nil
}

func (s *fakeObjectStore) CopyObject(ctx context.Context, sourceKey, destKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[sourceKey]
	if !ok {
		return os.ErrNotExist
	}
	s.objects[destKey] = append([]byte(nil), data...)
	s.metadata[destKey] = copyMetadata(s.metadata[sourceKey])
	return nil
}

func (s *fakeObjectStore) CopyObjectWithMetadata(ctx context.Context, sourceKey, destKey string, metadata map[string]string) error {
	if err := s.CopyObject(ctx, sourceKey, destKey); err != nil {
		return err
	}
	return s.UpdateObjectMetadata(ctx, destKey, metadata)
}

func (s *fakeObjectStore) UpdateObjectMetadata(ctx context.Context, key string, metadata map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[key]; !ok {
		return os.ErrNotExist
	}
	s.metadata[key] = copyMetadata(metadata)
	return nil
}

func newDirtyStagingTestFilesystem(t *testing.T, manager *staging.StagingManager) *Filesystem {
	t.Helper()
	return newDirtyStagingTestFilesystemWithStore(t, manager, nil)
}

func newDirtyStagingTestFilesystemWithStore(t *testing.T, manager *staging.StagingManager, store *fakeObjectStore) *Filesystem {
	t.Helper()

	perfConfig := &config.PerformanceConfig{
		WriteBufferKB:       4096,
		MaxBufferedWriteMB:  config.DefaultMaxBufferedWriteMB,
		MaxDirectoryEntries: config.DefaultMaxDirectoryEntries,
	}
	metadataCache := cache.NewMetadataCache(&config.MetadataCacheConfig{
		Enabled:    true,
		MaxEntries: 10,
		TTLSeconds: 60,
	})
	var objectStore posix.ObjectStore
	if store != nil {
		objectStore = store
	}
	ops := posix.NewOperationsHandler(objectStore, metadataCache, nil, perfConfig)
	if manager != nil {
		ops.SetPendingDeleteCheck(manager.HasPendingDelete)
	}
	return NewFilesystem(
		ops,
		logging.NewKVLogger(zap.NewNop()),
		"/",
		perfConfig,
		manager,
		nil,
		&feature.FeatureFlags{UseStagingPath: true},
	)
}
