package posix

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A file created through the gateway is listed from its staging session
// until it syncs; the parent's cached COS listing, taken while the file
// existed only in staging, lacks it. Once the upload lands and the session
// is cleaned up, that cached listing must not hide the file.
func TestSyncedFileIsListedAfterStagedListingWasCached(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("vis/", nil, time.Unix(100, 0))
	ops, _ := newRefreshTestOps(t, store)

	// Listed while the file is only staged: COS has just the directory.
	entries, err := ops.ListDirectory(ctx, "/vis")
	if err != nil {
		t.Fatalf("ListDirectory(staged) error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("staged names = %v, want none", entryNames(entries))
	}

	// The sync worker uploads the file and reports it synced.
	store.put("vis/v.txt", []byte("v"), time.Unix(200, 0))
	ops.InvalidateObjectAfterSync("/vis/v.txt")

	entries, err = ops.ListDirectory(ctx, "/vis")
	if err != nil {
		t.Fatalf("ListDirectory(synced) error = %v", err)
	}
	if names := entryNames(entries); strings.Join(names, ",") != "v.txt" {
		t.Fatalf("synced names = %v, want [v.txt]", names)
	}
}

// A listing taken from COS before an upload landed but cached after the sync
// completed would hide the file until the listing expires.
func TestListingRacingSyncIsNotCached(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("vis/", nil, time.Unix(100, 0))
	ops, _ := newRefreshTestOps(t, store)

	store.afterList = func() {
		store.afterList = nil
		store.put("vis/v.txt", []byte("v"), time.Unix(200, 0))
		ops.InvalidateObjectAfterSync("/vis/v.txt")
	}
	if _, err := ops.ListDirectory(ctx, "/vis"); err != nil {
		t.Fatalf("ListDirectory(racing) error = %v", err)
	}

	entries, err := ops.ListDirectory(ctx, "/vis")
	if err != nil {
		t.Fatalf("ListDirectory(after sync) error = %v", err)
	}
	if names := entryNames(entries); strings.Join(names, ",") != "v.txt" {
		t.Fatalf("names after sync = %v, want [v.txt]", names)
	}
}

// Re-syncing a file the cached listing already shows keeps the listing, so
// write churn on existing files does not force COS re-listings.
func TestResyncOfListedFileKeepsCachedListing(t *testing.T) {
	ctx := context.Background()
	store := newFakeObjectStore()
	store.put("vis/v.txt", []byte("v"), time.Unix(100, 0))
	ops, _ := newRefreshTestOps(t, store)

	if _, err := ops.ListDirectory(ctx, "/vis"); err != nil {
		t.Fatalf("ListDirectory(initial) error = %v", err)
	}
	store.put("vis/v.txt", []byte("v2"), time.Unix(200, 0))
	ops.InvalidateObjectAfterSync("/vis/v.txt")

	if entry, ok := ops.metadataCache.Get("/vis"); !ok || entry.ChildEntries == nil {
		t.Fatal("cached listing of /vis was dropped by a re-sync of a listed file")
	}
}
