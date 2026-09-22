package helpers

import (
	"testing"

	"github.com/willscott/go-nfs/helpers/memfs"
)

// An export's root handle must keep working however many other handles the
// server hands out: an NFSv3 client holds it for the life of the mount, so
// losing it to the cache makes every path stale and the mount unusable.
func TestCachingHandlerKeepsRootHandle(t *testing.T) {
	fs := memfs.New()
	if err := fs.MkdirAll("/", 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewCachingHandler(NewNullAuthHandler(fs), 8).(*CachingHandler)

	root := h.ToHandle(fs, []string{})
	if same := h.ToHandle(fs, []string{}); string(same) != string(root) {
		t.Fatal("the root got a second handle")
	}

	for i := 0; i < 100; i++ {
		h.ToHandle(fs, []string{"dir", string(rune('a' + i%26)), string(rune('a' + i/26))})
	}

	gotFS, path, err := h.FromHandle(root)
	if err != nil {
		t.Fatalf("root handle went stale after 100 other handles: %v", err)
	}
	if gotFS != fs || len(path) != 0 {
		t.Fatalf("root handle resolved to %v, want the export root", path)
	}
}

// Ordinary handles are still evicted, and report themselves as stale.
func TestCachingHandlerEvictsOrdinaryHandles(t *testing.T) {
	fs := memfs.New()
	if err := fs.MkdirAll("/", 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewCachingHandler(NewNullAuthHandler(fs), 4).(*CachingHandler)

	first := h.ToHandle(fs, []string{"a", "one"})
	for i := 0; i < 20; i++ {
		h.ToHandle(fs, []string{"a", string(rune('b' + i))})
	}
	if _, _, err := h.FromHandle(first); err == nil {
		t.Fatal("an evicted handle resolved; the cache is unbounded")
	}
}
