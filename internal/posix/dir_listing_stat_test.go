package posix

import (
	"context"
	"testing"
	"time"
)

// Listing a directory caches its entries under the directory's own path.
// A Stat of the directory served from that entry must still report the
// directory's mode, not a file's 0644: NFS clients showed a directory as
// drw-r--r-- right after listing it and could not search it.
func TestStatAfterListingKeepsDirectoryMode(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("d/", nil, time.Unix(100, 0))
	store.put("d/f", []byte("x"), time.Unix(100, 0))
	ops, _ := newRefreshTestOps(t, store)

	before, err := ops.Stat(ctx, "/d")
	if err != nil {
		t.Fatalf("Stat before listing: %v", err)
	}
	if _, err := ops.ListDirectory(ctx, "/d"); err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	after, err := ops.Stat(ctx, "/d")
	if err != nil {
		t.Fatalf("Stat after listing: %v", err)
	}
	if !after.IsDir() || after.Mode() != before.Mode() {
		t.Fatalf("mode after listing = %v, want %v as before", after.Mode(), before.Mode())
	}
}

// With no stat cached before the listing, the directory still reads as one.
func TestStatAfterListingOnlyReportsDirectoryMode(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("d/", nil, time.Unix(100, 0))
	ops, _ := newRefreshTestOps(t, store)

	if _, err := ops.ListDirectory(ctx, "/d"); err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	info, err := ops.Stat(ctx, "/d")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("mode = %v, want a searchable directory", info.Mode())
	}
}
