package posix

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// A directory of exactly max_directory_entries lists. The objects under its
// prefix include the directory's own marker, which is not an entry, and
// counting that made a directory at the limit fail — and fail as "not a
// directory", since that is what the NFS layer made of the error.
func TestListDirectoryAtTheLimit(t *testing.T) {
	const limit = 100
	store := newFakeObjectStore()
	store.put("big/", nil, time.Now()) // the directory's own marker
	for i := 0; i < limit; i++ {
		store.put(fmt.Sprintf("big/f%03d", i), []byte("x"), time.Now())
	}
	ops := newAttributeTestOps(store) // MaxDirectoryEntries: 100

	entries, err := ops.ListDirectory(context.Background(), "/big")
	if err != nil {
		t.Fatalf("listing a directory of %d entries: %v", limit, err)
	}
	if len(entries) != limit {
		t.Fatalf("listed %d entries, want %d", len(entries), limit)
	}
}

// One entry past the limit is refused, and says so.
func TestListDirectoryOverTheLimit(t *testing.T) {
	const limit = 100
	store := newFakeObjectStore()
	store.put("big/", nil, time.Now())
	for i := 0; i <= limit; i++ {
		store.put(fmt.Sprintf("big/f%03d", i), []byte("x"), time.Now())
	}
	ops := newAttributeTestOps(store)

	_, err := ops.ListDirectory(context.Background(), "/big")
	if !errors.Is(err, ErrDirectoryTooLarge) {
		t.Fatalf("listing %d entries with a limit of %d: %v, want ErrDirectoryTooLarge", limit+1, limit, err)
	}
}
