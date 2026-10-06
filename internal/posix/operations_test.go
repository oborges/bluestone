package posix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRenameFileCopiesDeletesAndInvalidatesCaches(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("old.txt", []byte("payload"), time.Unix(100, 0))

	ops, dataCache := newRefreshTestOps(t, store)
	if _, err := ops.ListDirectory(ctx, "/"); err != nil {
		t.Fatalf("ListDirectory(before) error = %v", err)
	}
	if _, err := ops.Stat(ctx, "/old.txt"); err != nil {
		t.Fatalf("Stat(before) error = %v", err)
	}
	if data, err := ops.ReadFile(ctx, "/old.txt", 0, 0); err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	} else if string(data) != "payload" {
		t.Fatalf("ReadFile(before) = %q, want payload", data)
	}

	if err := ops.RenameFile(ctx, "/old.txt", "/new.txt"); err != nil {
		t.Fatalf("RenameFile() error = %v", err)
	}

	if _, err := store.HeadObject(ctx, "old.txt"); !os.IsNotExist(err) {
		t.Fatalf("old object HeadObject error = %v, want not exist", err)
	}
	if data, err := store.GetObject(ctx, "new.txt"); err != nil {
		t.Fatalf("new object GetObject error = %v", err)
	} else if string(data) != "payload" {
		t.Fatalf("new object data = %q, want payload", data)
	}
	if _, err := dataCache.Read("/old.txt", 0, 0); err == nil {
		t.Fatal("old data cache entry still exists after rename")
	}

	entries, err := ops.ListDirectory(ctx, "/")
	if err != nil {
		t.Fatalf("ListDirectory(after) error = %v", err)
	}
	if names := strings.Join(entryNames(entries), ","); names != "new.txt" {
		t.Fatalf("ListDirectory(after) names = %s, want new.txt", names)
	}
}

func TestRenameDirectoryCopiesTreeDeletesSourcesAndInvalidatesCaches(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("old/", nil, time.Unix(100, 0))
	store.put("old/a.txt", []byte("a"), time.Unix(101, 0))
	store.put("old/sub/b.txt", []byte("b"), time.Unix(102, 0))

	ops, dataCache := newRefreshTestOps(t, store)
	if _, err := ops.ListDirectory(ctx, "/"); err != nil {
		t.Fatalf("ListDirectory(root before) error = %v", err)
	}
	if _, err := ops.ListDirectory(ctx, "/old"); err != nil {
		t.Fatalf("ListDirectory(old before) error = %v", err)
	}
	if _, err := ops.ReadFile(ctx, "/old/a.txt", 0, 0); err != nil {
		t.Fatalf("ReadFile(old/a before) error = %v", err)
	}

	if err := ops.RenameFile(ctx, "/old", "/new"); err != nil {
		t.Fatalf("RenameFile(directory) error = %v", err)
	}

	for _, key := range []string{"old/", "old/a.txt", "old/sub/b.txt"} {
		if _, err := store.HeadObject(ctx, key); !os.IsNotExist(err) {
			t.Fatalf("source key %q HeadObject error = %v, want not exist", key, err)
		}
	}
	for _, key := range []string{"new/", "new/a.txt", "new/sub/b.txt"} {
		if _, err := store.HeadObject(ctx, key); err != nil {
			t.Fatalf("dest key %q HeadObject error = %v", key, err)
		}
	}
	if _, err := dataCache.Read("/old/a.txt", 0, 0); err == nil {
		t.Fatal("old directory data cache entry still exists after rename")
	}

	rootEntries, err := ops.ListDirectory(ctx, "/")
	if err != nil {
		t.Fatalf("ListDirectory(root after) error = %v", err)
	}
	if names := strings.Join(entryNames(rootEntries), ","); names != "new" {
		t.Fatalf("root names after directory rename = %s, want new", names)
	}

	newEntries, err := ops.ListDirectory(ctx, "/new")
	if err != nil {
		t.Fatalf("ListDirectory(new after) error = %v", err)
	}
	if names := strings.Join(entryNames(newEntries), ","); names != "a.txt,sub" {
		t.Fatalf("new directory names = %s, want a.txt,sub", names)
	}
}

func TestRenameFileDeleteFailureLeavesCopiedDestination(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("old.txt", []byte("payload"), time.Unix(100, 0))
	store.failDelete("old.txt", fmt.Errorf("injected delete failure"))

	ops, _ := newRefreshTestOps(t, store)
	err := ops.RenameFile(ctx, "/old.txt", "/new.txt")
	if err == nil {
		t.Fatal("RenameFile() error = nil, want delete failure")
	}

	if _, headErr := store.HeadObject(ctx, "old.txt"); headErr != nil {
		t.Fatalf("old object should remain after delete failure: %v", headErr)
	}
	if data, headErr := store.GetObject(ctx, "new.txt"); headErr != nil {
		t.Fatalf("new copied object should remain after delete failure: %v", headErr)
	} else if string(data) != "payload" {
		t.Fatalf("new copied object data = %q, want payload", data)
	}
}

func TestRenameDirectoryCopyFailureLeavesSourceAndCopiedDestinations(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("old/a.txt", []byte("a"), time.Unix(100, 0))
	store.put("old/b.txt", []byte("b"), time.Unix(101, 0))
	store.failCopy("old/b.txt", "new/b.txt", fmt.Errorf("injected copy failure"))

	ops, _ := newRefreshTestOps(t, store)
	err := ops.RenameFile(ctx, "/old", "/new")
	if err == nil {
		t.Fatal("RenameFile(directory) error = nil, want copy failure")
	}

	for _, key := range []string{"old/a.txt", "old/b.txt"} {
		if _, headErr := store.HeadObject(ctx, key); headErr != nil {
			t.Fatalf("source key %q should remain after copy failure: %v", key, headErr)
		}
	}
	if _, headErr := store.HeadObject(ctx, "new/a.txt"); headErr != nil {
		t.Fatalf("already copied destination should remain after copy failure: %v", headErr)
	}
	if _, headErr := store.HeadObject(ctx, "new/b.txt"); !os.IsNotExist(headErr) {
		t.Fatalf("failed destination HeadObject error = %v, want not exist", headErr)
	}
}

// An object deleted between the listing and its copy has nothing to carry
// over: the sync worker retires pending deletes while a rename runs.
func TestRenameDirectorySkipsObjectsDeletedSinceTheListing(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("old/a.txt", []byte("a"), time.Unix(100, 0))
	store.put("old/gone.txt", []byte("gone"), time.Unix(101, 0))
	store.put("old/z.txt", []byte("z"), time.Unix(102, 0))
	store.failCopy("old/gone.txt", "new/gone.txt", fmt.Errorf("failed to copy object old/gone.txt: %w", os.ErrNotExist))

	ops, _ := newRefreshTestOps(t, store)
	if err := ops.RenameFile(ctx, "/old", "/new"); err != nil {
		t.Fatalf("RenameFile(directory) error = %v, want the vanished object skipped", err)
	}
	for _, key := range []string{"new/a.txt", "new/z.txt"} {
		if _, err := store.HeadObject(ctx, key); err != nil {
			t.Errorf("%s was not carried over: %v", key, err)
		}
	}
	if _, err := store.HeadObject(ctx, "new/gone.txt"); !os.IsNotExist(err) {
		t.Errorf("new/gone.txt HeadObject error = %v, want not exist", err)
	}
	for _, key := range []string{"old/a.txt", "old/gone.txt", "old/z.txt"} {
		if _, err := store.HeadObject(ctx, key); !os.IsNotExist(err) {
			t.Errorf("%s is still under the old name: %v", key, err)
		}
	}
}

// A directory's objects are copied and deleted several at a time. One at a
// time, a directory of 1,500 took over a minute against COS, longer than an
// NFS client waits before sending the rename again; the gateway then ran it
// twice and the client was told a rename that had worked had failed.
func TestRenameDirectoryCopiesAndDeletesConcurrently(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	const files = 60
	store.put("old/", nil, time.Unix(100, 0))
	for i := 0; i < files; i++ {
		store.put(fmt.Sprintf("old/sub/f%03d", i), []byte{byte(i)}, time.Unix(100, 0))
	}
	store.writeDelay = 5 * time.Millisecond

	ops, _ := newRefreshTestOps(t, store)
	const limit = 12
	ops.perfConfig.MaxConcurrentWrites = limit
	if err := ops.RenameFile(ctx, "/old", "/new"); err != nil {
		t.Fatalf("RenameFile(directory) error = %v", err)
	}

	store.mu.RLock()
	most := store.maxWritesInFlight
	store.mu.RUnlock()
	if most < 2 || most > limit {
		t.Errorf("at most %d bucket writes ran at once, want several and no more than max_concurrent_writes (%d)", most, limit)
	}
	for i := 0; i < files; i++ {
		data, err := store.GetObject(ctx, fmt.Sprintf("new/sub/f%03d", i))
		if err != nil || len(data) != 1 || data[0] != byte(i) {
			t.Fatalf("new/sub/f%03d = %v, %v; want its own byte", i, data, err)
		}
		if _, err := store.HeadObject(ctx, fmt.Sprintf("old/sub/f%03d", i)); !os.IsNotExist(err) {
			t.Fatalf("old/sub/f%03d is still there: %v", i, err)
		}
	}
	if _, err := store.HeadObject(ctx, "new/"); err != nil {
		t.Errorf("the directory marker was not carried over: %v", err)
	}
}

// After a copy fails no further copies are started, and nothing is deleted.
func TestRenameDirectoryStopsStartingCopiesAfterAFailure(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	const files = 80
	for i := 0; i < files; i++ {
		store.put(fmt.Sprintf("old/f%03d", i), []byte("x"), time.Unix(100, 0))
	}
	store.failCopy("old/f000", "new/f000", fmt.Errorf("injected copy failure"))
	store.writeDelay = 5 * time.Millisecond

	ops, _ := newRefreshTestOps(t, store)
	const limit = 4
	ops.perfConfig.MaxConcurrentWrites = limit
	if err := ops.RenameFile(ctx, "/old", "/new"); err == nil {
		t.Fatal("RenameFile(directory) error = nil, want the copy failure")
	}

	store.mu.RLock()
	attempts := store.copyCalls
	store.mu.RUnlock()
	if attempts > 3*limit {
		t.Errorf("%d copies were attempted after the first failed, want it to stop within a few (limit %d)", attempts, limit)
	}
	for i := 0; i < files; i++ {
		if _, err := store.HeadObject(ctx, fmt.Sprintf("old/f%03d", i)); err != nil {
			t.Fatalf("source old/f%03d was deleted after a failed copy: %v", i, err)
		}
	}
}

// rm -r unlinks a directory's files and removes it straight after. A file
// whose delete was deferred behind its upload can complete while the removal
// lists the directory: the bucket's listing has the object, and by the time
// the listing is read nothing marks it as going. The directory is empty all
// the same, and its removal must not fail with "not empty".
func TestDeleteDirectoryIgnoresFilesDeletedWhileItListed(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("dir/", nil, time.Unix(100, 0))
	store.put("dir/going.txt", []byte("payload"), time.Unix(100, 0))
	store.afterList = func() {
		if err := store.DeleteObject(ctx, "dir/going.txt"); err != nil {
			t.Errorf("DeleteObject() error = %v", err)
		}
	}

	ops, _ := newRefreshTestOps(t, store)
	if err := ops.DeleteDirectory(ctx, "/dir"); err != nil {
		t.Fatalf("DeleteDirectory(emptied while listing) error = %v", err)
	}
	if _, err := store.HeadObject(ctx, "dir/"); !os.IsNotExist(err) {
		t.Fatalf("directory marker HeadObject error = %v, want it removed", err)
	}
}

func TestDeleteDirectoryNotEmptyReportsENOTEMPTY(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("dir/", nil, time.Unix(100, 0))
	store.put("dir/child.txt", []byte("payload"), time.Unix(100, 0))

	ops, _ := newRefreshTestOps(t, store)
	err := ops.DeleteDirectory(ctx, "/dir")
	if !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("DeleteDirectory(non-empty) error = %v, want ENOTEMPTY", err)
	}
	if _, err := store.HeadObject(ctx, "dir/"); err != nil {
		t.Fatalf("directory marker HeadObject error = %v, want it kept", err)
	}
}
