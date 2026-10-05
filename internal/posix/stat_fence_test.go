package posix

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// A lookup can be overtaken by the change it is asking about: the bucket is
// asked before an upload lands and answers after the sync has cleared the
// cache. What it saw must not be cached. It was, as a "does not exist" good
// for five seconds, and a delete arriving in that time took the file for
// gone and removed nothing: one file in several thousand survived its
// delete when files were removed while they synced.
func TestStatDoesNotCacheAMissOvertakenByAnUpload(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	ops, _ := newRefreshTestOps(t, store)

	var once sync.Once
	store.afterHead = func(key string) {
		if key != "f" {
			return
		}
		once.Do(func() {
			// The upload lands and the sync worker reports it, while the
			// lookup that just missed it is still on its way back.
			store.put("f", []byte("uploaded"), time.Unix(100, 0))
			ops.InvalidateObjectAfterSync("/f")
		})
	}

	if _, err := ops.Stat(ctx, "/f"); !os.IsNotExist(err) {
		t.Fatalf("Stat() overtaken by the upload error = %v, want not exist (it was asked first)", err)
	}
	info, err := ops.Stat(ctx, "/f")
	if err != nil {
		t.Fatalf("Stat() after the upload error = %v: the overtaken miss was cached", err)
	}
	if info.Size() != int64(len("uploaded")) {
		t.Fatalf("Stat() size = %d, want the uploaded file", info.Size())
	}
}

// The other way round: a lookup that saw a file just before its delete must
// not leave the file in the cache after it.
func TestStatDoesNotCacheAHitOvertakenByADelete(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("f", []byte("doomed"), time.Unix(100, 0))
	ops, _ := newRefreshTestOps(t, store)

	var once sync.Once
	store.afterHead = func(key string) {
		if key != "f" {
			return
		}
		once.Do(func() {
			if err := ops.DeleteFile(ctx, "/f"); err != nil {
				t.Errorf("DeleteFile() error = %v", err)
			}
		})
	}

	if _, err := ops.Stat(ctx, "/f"); err != nil {
		t.Fatalf("Stat() overtaken by the delete error = %v, want the file (it was asked first)", err)
	}
	if _, err := ops.Stat(ctx, "/f"); !os.IsNotExist(err) {
		t.Fatalf("Stat() after the delete error = %v, want not exist: the overtaken hit was cached", err)
	}
}

// A lookup nothing interferes with is still cached, hit or miss.
func TestStatStillCachesUndisturbedLookups(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("here", []byte("x"), time.Unix(100, 0))
	ops, _ := newRefreshTestOps(t, store)

	for _, path := range []string{"/here", "/missing"} {
		_, _ = ops.Stat(ctx, path)
		store.mu.Lock()
		before := store.headCalls
		store.mu.Unlock()
		_, _ = ops.Stat(ctx, path)
		store.mu.Lock()
		after := store.headCalls
		store.mu.Unlock()
		if after != before {
			t.Errorf("second Stat(%s) asked the bucket again (%d more HEADs), want it answered from the cache", path, after-before)
		}
	}
}
