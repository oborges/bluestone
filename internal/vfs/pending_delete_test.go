package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-cos-sdk-go/service/s3"
	"github.com/oborges/bluestone/internal/staging"
)

// gatedUploadStore is a sync worker object store over fakeObjectStore whose
// uploads wait for release, holding an upload in flight.
type gatedUploadStore struct {
	*fakeObjectStore
	started chan string
	release chan struct{}
}

func (s *gatedUploadStore) PutObjectStream(ctx context.Context, key string, body io.ReadSeeker, metadata map[string]string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	// The worker uploads by path; the COS client drops the leading slash.
	key = strings.TrimPrefix(key, "/")
	s.started <- key
	<-s.release
	s.putWithMetadata(key, data, metadata)
	return nil
}

var errNoMultipart = errors.New("multipart uploads not supported by test store")

func (s *gatedUploadStore) CreateMultipartUpload(ctx context.Context, key string, metadata map[string]string) (string, error) {
	return "", errNoMultipart
}

func (s *gatedUploadStore) UploadPart(ctx context.Context, key, uploadID string, partNumber int64, body io.ReadSeeker) (string, error) {
	return "", errNoMultipart
}

func (s *gatedUploadStore) CompleteMultipartUpload(ctx context.Context, key, uploadID string, completedParts []*s3.CompletedPart) error {
	return errNoMultipart
}

func (s *gatedUploadStore) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	return errNoMultipart
}

// TestRemoveDirectoryWhileDeletedFileUploadInFlight reproduces rm -rf of a
// directory holding a dirty staged file whose upload is in flight when the
// file is deleted: the upload lands after the delete was accepted, so the
// object reappears in COS until the tombstone removes it. The directory (and
// its parent) must still be removable, and the tombstone must still delete
// the object once the upload is done.
func TestRemoveDirectoryWhileDeletedFileUploadInFlight(t *testing.T) {
	for _, tc := range []struct {
		name string
		// rmdirBeforeUploadLands removes the directories while the upload
		// is still in flight rather than after it lands.
		rmdirBeforeUploadLands bool
	}{
		{name: "rmdir after upload lands"},
		{name: "rmdir while upload in flight", rmdirBeforeUploadLands: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testStagingConfig(t)
			cfg.SyncInterval = "20ms"
			manager, err := staging.NewStagingManager(cfg)
			if err != nil {
				t.Fatalf("NewStagingManager() error = %v", err)
			}
			defer manager.Shutdown()

			store := newFakeObjectStore()
			fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)
			if err := fs.MkdirAll("permtest/vpc", 0755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}

			const path = "/permtest/vpc/noexec"
			const key = "permtest/vpc/noexec"
			session, err := manager.GetOrCreateSession(path)
			if err != nil {
				t.Fatalf("GetOrCreateSession() error = %v", err)
			}
			if _, err := session.Write([]byte("staged data"), 0); err != nil {
				t.Fatalf("session.Write() error = %v", err)
			}
			manager.MarkDirty(path, session.Size)
			manager.ReleaseSession(path)

			gated := &gatedUploadStore{
				fakeObjectStore: store,
				started:         make(chan string, 1),
				release:         make(chan struct{}),
			}
			worker := staging.NewSyncWorker(manager, gated, cfg)
			worker.SetObjectMutatedCallback(fs.ops.InvalidateFileMutation)
			worker.SetObjectSyncedCallback(fs.ops.InvalidateObjectAfterSync)

			// Start the upload the way a sync worker does: claim the path,
			// then sync it.
			if !manager.TryLockSync(path) {
				t.Fatal("failed to claim sync lock")
			}
			synced := make(chan error, 1)
			go func() {
				err := worker.TriggerSync(path)
				manager.UnlockSync(path)
				synced <- err
			}()
			select {
			case <-gated.started:
			case <-time.After(5 * time.Second):
				t.Fatal("upload never started")
			}

			// rm deletes the file mid-upload: the delete is accepted with a
			// tombstone and completed once the upload is done.
			if err := fs.Remove("permtest/vpc/noexec"); err != nil {
				t.Fatalf("Remove(file) error = %v", err)
			}
			if !manager.HasPendingDelete(path) {
				t.Fatal("delete during an in-flight upload should leave a pending delete")
			}

			landUpload := func() {
				close(gated.release)
				select {
				case err := <-synced:
					if err != nil {
						t.Fatalf("TriggerSync() error = %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("upload never finished")
				}
				if store.get(key) == nil {
					t.Fatal("the in-flight upload should have landed in COS")
				}
			}

			if !tc.rmdirBeforeUploadLands {
				landUpload()
			}
			for _, dir := range []string{"permtest/vpc", "permtest"} {
				if err := fs.Remove(dir); err != nil {
					t.Fatalf("Remove(%s) error = %v", dir, err)
				}
			}
			if tc.rmdirBeforeUploadLands {
				landUpload()
			}

			// The object lingering in COS must not bring the tree back.
			for _, dir := range []string{"permtest/vpc", "permtest"} {
				if _, err := fs.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("Stat(%s) after rmdir = %v, want not-exist", dir, err)
				}
			}
			entries, err := fs.ReadDir("/")
			if err != nil {
				t.Fatalf("ReadDir(/) error = %v", err)
			}
			for _, entry := range entries {
				if entry.Name() == "permtest" {
					t.Fatal("ReadDir(/) lists a removed directory")
				}
			}

			// The tombstone still deletes the object the upload left.
			worker.Start()
			defer worker.Stop()
			deadline := time.Now().Add(5 * time.Second)
			for manager.HasPendingDelete(path) {
				if time.Now().After(deadline) {
					t.Fatal("pending delete was never completed")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if store.get(key) != nil || !store.wasDeleted(key) {
				t.Fatal("the tombstone should delete the object the in-flight upload left")
			}
		})
	}
}

// A directory rename must not carry a file whose delete is pending to the new
// name; the file's object would otherwise outlive its tombstone there.
func TestRenameDirectorySkipsPendingDeleteObjects(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	store := newFakeObjectStore()
	store.put("dir/", nil)
	store.put("dir/doomed.txt", []byte("doomed"))
	store.put("dir/kept.txt", []byte("kept"))
	if _, err := manager.RegisterPendingDelete("/dir/doomed.txt"); err != nil {
		t.Fatalf("RegisterPendingDelete() error = %v", err)
	}
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	if err := fs.Rename("dir", "moved"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if store.get("moved/kept.txt") == nil {
		t.Fatal("rename should move the other files")
	}
	if store.get("moved/doomed.txt") != nil {
		t.Fatal("rename must not copy a file whose delete is pending")
	}
	if store.get("dir/doomed.txt") != nil {
		t.Fatal("rename should remove the source objects, including the pending delete's")
	}
}

// When the COS delete of an unlinked dirty file fails, the file's object is
// still in COS and a listing cached before the unlink still names it; its
// directory must still be removable while the sync worker retries.
func TestRemoveDirectoryAfterFailedCOSDeleteOfDirtyFile(t *testing.T) {
	cfg := testStagingConfig(t)
	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	store := newFakeObjectStore()
	store.put("dir/", nil)
	// An earlier version of the file is already in COS.
	store.put("dir/file", []byte("old"))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	// Cache the directory's listing, as a client listing it would.
	if _, err := fs.ops.ListDirectory(fs.requestContext(), "/dir"); err != nil {
		t.Fatalf("ListDirectory() error = %v", err)
	}

	const path = "/dir/file"
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write([]byte("new data"), 0); err != nil {
		t.Fatalf("session.Write() error = %v", err)
	}
	manager.MarkDirty(path, session.Size)
	manager.ReleaseSession(path)

	store.setDeleteErr(errors.New("dial tcp: connection refused"))
	if err := fs.Remove("dir/file"); err != nil {
		t.Fatalf("Remove(file) error = %v", err)
	}
	if !manager.HasPendingDelete(path) {
		t.Fatal("a failed COS delete should leave the delete pending")
	}
	store.setDeleteErr(nil)

	if err := fs.Remove("dir"); err != nil {
		t.Fatalf("Remove(dir) with only a pending-delete file error = %v", err)
	}
}
