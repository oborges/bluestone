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
