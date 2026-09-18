package vfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Clients make a directory with a FILE_CREATE open (mkdir on macOS, Windows
// and Linux): it must succeed once and then fail as already existing.
func TestLocalBackendCreatesDirectory(t *testing.T) {
	root := t.TempDir()
	b, err := NewLocalBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	create := OpenOptions{Path: "dir1", Disposition: DispositionCreate, CreateDir: true}

	h, err := b.Open(ctx, create)
	if err != nil {
		t.Fatalf("creating a new directory: %v", err)
	}
	_ = h.Close(ctx)
	if fi, err := os.Stat(filepath.Join(root, "dir1")); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}

	if _, err := b.Open(ctx, create); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("creating it again: err = %v, want it to already exist", err)
	}

	openIf := OpenOptions{Path: "dir1", Disposition: DispositionOpenIf, CreateDir: true}
	h, err = b.Open(ctx, openIf)
	if err != nil {
		t.Fatalf("open-if on an existing directory: %v", err)
	}
	_ = h.Close(ctx)
}

// SMB paths separate components with backslashes; outside Windows they must
// still reach the subdirectory rather than name a file in the share root.
func TestLocalBackendBackslashPaths(t *testing.T) {
	root := t.TempDir()
	b, err := NewLocalBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(root, "dir1"), 0o755); err != nil {
		t.Fatal(err)
	}

	h, err := b.Open(ctx, OpenOptions{Path: `dir1\a.txt`, Disposition: DispositionCreate})
	if err != nil {
		t.Fatalf("create in a subdirectory: %v", err)
	}
	_ = h.Close(ctx)
	if _, err := os.Stat(filepath.Join(root, "dir1", "a.txt")); err != nil {
		t.Fatalf(`dir1\a.txt did not land in dir1: %v`, err)
	}

	// Rename takes a path from the share root, so it can move a file.
	h, err = b.Open(ctx, OpenOptions{Path: `dir1\a.txt`, Disposition: DispositionOpen})
	if err != nil {
		t.Fatal(err)
	}
	rn, ok := h.(Renamer)
	if !ok {
		t.Fatal("local handle does not implement Renamer")
	}
	if err := rn.Rename(ctx, `moved.txt`, false); err != nil {
		t.Fatalf("rename to the share root: %v", err)
	}
	_ = h.Close(ctx)
	if _, err := os.Stat(filepath.Join(root, "moved.txt")); err != nil {
		t.Fatalf("file not moved to the share root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dir1", "a.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("old name still present: %v", err)
	}
}
