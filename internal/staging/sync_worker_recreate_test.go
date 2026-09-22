package staging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// midPartCOSClient runs duringPart halfway through reading the first part's
// body, so the test can change the staged file while an upload is reading
// it, and checks that every byte it uploads matches want.
type midPartCOSClient struct {
	*multipartLifecycleCOSClient
	want       []byte
	duringPart func()
	ran        bool
	mismatch   error
}

func (m *midPartCOSClient) UploadPart(ctx context.Context, key, uploadID string, partNumber int64, body io.ReadSeeker) (string, error) {
	start, err := body.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return "", err
	}
	if !m.ran && m.duringPart != nil {
		m.ran = true
		m.duringPart()
	}
	rest, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	data = append(data, rest...)

	offset := int64(partNumber-1)*20*1024*1024 + start
	if end := offset + int64(len(data)); end > int64(len(m.want)) || !bytes.Equal(data, m.want[offset:end]) {
		m.mismatch = fmt.Errorf("part %d: upload read %d bytes that differ from the snapshot", partNumber, len(data))
	}
	if _, err := body.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	return m.multipartLifecycleCOSClient.UploadPart(ctx, key, uploadID, partNumber, body)
}

func stagedPattern(size int, seed byte) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i%251) + seed
	}
	return data
}

func newRecreateTestManager(t *testing.T) *StagingManager {
	t.Helper()
	cfg := createTestConfig(t)
	cfg.CleanAfterSync = false
	cfg.RetryBackoffInit = "1ms"
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(func() { manager.Shutdown() })
	return manager
}

func writeDirty(t *testing.T, manager *StagingManager, session *WriteSession, path string, data []byte) {
	t.Helper()
	if _, err := session.Write(data, 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	manager.MarkDirty(path, int64(len(data)))
}

// TestRecreateWithTruncateDuringMultipartUpload reproduces a client deleting
// a file whose multipart upload is in flight and then recreating it with
// O_TRUNC: the recreated file reuses the staging session, and truncating the
// file the upload reads used to fault the upload's mmap (SIGBUS). The upload
// must keep reading the bytes it started with, abort as out of date, and the
// next sync must upload the recreated file.
func TestRecreateWithTruncateDuringMultipartUpload(t *testing.T) {
	manager := newRecreateTestManager(t)
	path := "/d/big"
	original := stagedPattern(45<<20, 1)
	recreated := stagedPattern(3<<20, 7)

	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	writeDirty(t, manager, session, path, original)
	manager.ReleaseSession(path)

	cosClient := &midPartCOSClient{multipartLifecycleCOSClient: newMultipartLifecycleCOSClient(), want: original}
	cosClient.duringPart = func() {
		// rm: the upload holds the path, so the delete is deferred.
		immediate, err := manager.RegisterPendingDelete(path)
		if err != nil || immediate {
			t.Errorf("RegisterPendingDelete = %v, %v; want deferred", immediate, err)
		}
		// head -c > d/big: recreate, O_TRUNC, write the new bytes.
		again, err := manager.GetOrCreateSession(path)
		if err != nil {
			t.Errorf("recreate session: %v", err)
			return
		}
		if err := again.Truncate(0); err != nil {
			t.Errorf("truncate recreated file: %v", err)
		}
		manager.MarkDirty(path, 0)
		writeDirty(t, manager, again, path, recreated)
		manager.ReleaseSession(path)
	}

	worker := NewSyncWorker(manager, cosClient, manager.config)
	err = syncClaimed(t, manager, worker, path)
	if !cosClient.ran {
		t.Fatal("the upload never reached its first part")
	}
	if cosClient.mismatch != nil {
		t.Fatal(cosClient.mismatch)
	}
	if !errors.Is(err, errMultipartSnapshotChanged) {
		t.Fatalf("sync of the replaced bytes = %v, want %v", err, errMultipartSnapshotChanged)
	}
	if _, ok := cosClient.objectSize(path); ok {
		t.Fatal("the replaced bytes were published")
	}
	if manager.HasPendingDelete(path) {
		t.Fatal("recreating the path should cancel its pending delete")
	}

	if err := syncClaimed(t, manager, worker, path); err != nil {
		t.Fatalf("sync of the recreated file: %v", err)
	}
	if size, ok := cosClient.objectSize(path); !ok || size != int64(len(recreated)) {
		t.Fatalf("object size = %d (exists %v), want %d", size, ok, len(recreated))
	}
	onDisk, err := os.ReadFile(manager.stagingFilePath(path))
	if err != nil || !bytes.Equal(onDisk, recreated) {
		t.Fatalf("staging file does not hold the recreated bytes (err %v)", err)
	}
	assertNoDetachLeftovers(t, manager)
}

// TestWriteDuringMultipartUploadLeavesUploadBytesAlone overwrites and then
// shrinks a file while its upload is reading it. The upload keeps its bytes;
// the session continues with the changes.
func TestWriteDuringMultipartUploadLeavesUploadBytesAlone(t *testing.T) {
	manager := newRecreateTestManager(t)
	path := "/d/rewrite"
	original := stagedPattern(45<<20, 1)

	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	writeDirty(t, manager, session, path, original)

	patch := bytes.Repeat([]byte{0xAA}, 4096)
	cosClient := &midPartCOSClient{multipartLifecycleCOSClient: newMultipartLifecycleCOSClient(), want: original}
	cosClient.duringPart = func() {
		if _, err := session.Write(patch, 1<<20); err != nil {
			t.Errorf("write during upload: %v", err)
		}
		if err := session.Truncate(30 << 20); err != nil {
			t.Errorf("truncate during upload: %v", err)
		}
		manager.MarkDirty(path, 30<<20)
	}

	worker := NewSyncWorker(manager, cosClient, manager.config)
	if err := syncClaimed(t, manager, worker, path); !errors.Is(err, errMultipartSnapshotChanged) {
		t.Fatalf("sync = %v, want %v", err, errMultipartSnapshotChanged)
	}
	if cosClient.mismatch != nil {
		t.Fatal(cosClient.mismatch)
	}

	want := append([]byte(nil), original[:30<<20]...)
	copy(want[1<<20:], patch)
	got := make([]byte, len(want)+1)
	n, err := session.Read(got, 0)
	for err == nil && n < len(got) {
		var m int
		m, err = session.Read(got[n:], int64(n))
		n += m
	}
	if n != len(want) || !bytes.Equal(got[:n], want) {
		t.Fatalf("session reads %d bytes that differ from what was written", n)
	}

	cosClient.want = want
	if err := syncClaimed(t, manager, worker, path); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if cosClient.mismatch != nil {
		t.Fatal(cosClient.mismatch)
	}
	if size, _ := cosClient.objectSize(path); size != int64(len(want)) {
		t.Fatalf("object size = %d, want %d", size, len(want))
	}
	assertNoDetachLeftovers(t, manager)
}

// syncClaimed syncs path holding the sync claim, as the worker loop does, so
// a delete meanwhile is deferred until the upload finishes.
func syncClaimed(t *testing.T, manager *StagingManager, worker *SyncWorker, path string) error {
	t.Helper()
	if !manager.TryLockSync(path) {
		t.Fatalf("could not claim %s for sync", path)
	}
	defer manager.UnlockSync(path)
	return worker.syncFile(path)
}

func assertNoDetachLeftovers(t *testing.T, manager *StagingManager) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(manager.stagingRoot, "active"))
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".detach-") {
			t.Fatalf("leftover detach file %s", entry.Name())
		}
	}
}
