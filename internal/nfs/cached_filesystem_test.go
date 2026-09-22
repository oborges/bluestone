package nfs

import (
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

func newTestCachedFS(t *testing.T, fs billy.Filesystem) *CachedFilesystem {
	t.Helper()
	return NewCachedFilesystem(fs, logging.NewKVLogger(zap.NewNop()), time.Minute)
}

func listNames(t *testing.T, fs billy.Filesystem, dir string) string {
	t.Helper()
	entries, err := fs.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func touch(t *testing.T, fs billy.Filesystem, name string) {
	t.Helper()
	f, err := fs.Create(name)
	if err != nil {
		t.Fatalf("Create(%s) error = %v", name, err)
	}
	f.Close()
}

// rm -rf lists a directory and removes what the listing shows. A listing
// cached before files were created in it hid them, so rm left them behind
// and the rmdir failed. Every change must drop the listings it affects.
func TestCachedFilesystemDropsListingsOnChange(t *testing.T) {
	cfs := newTestCachedFS(t, memfs.New())
	if err := cfs.MkdirAll("vpc/sub", 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	steps := []struct {
		name   string
		change func() error
		dir    string
		want   string
	}{
		{"create", func() error { touch(t, cfs, "vpc/f"); return nil }, "vpc", "f,sub"},
		{"open with O_CREATE", func() error {
			f, err := cfs.OpenFile("/vpc/noexec", os.O_CREATE|os.O_WRONLY, 0644)
			if err == nil {
				f.Close()
			}
			return err
		}, "vpc", "f,noexec,sub"},
		{"mkdir", func() error { return cfs.MkdirAll("vpc/new/deeper", 0755) }, "vpc", "f,new,noexec,sub"},
		{"symlink", func() error { return cfs.Symlink("f", "vpc/link") }, "vpc", "f,link,new,noexec,sub"},
		{"remove", func() error { return cfs.Remove("vpc/link") }, "vpc", "f,new,noexec,sub"},
		{"rename", func() error { return cfs.Rename("vpc/f", "vpc/sub/f") }, "vpc/sub", "f"},
		{"rename source", func() error { return nil }, "vpc", "new,noexec,sub"},
		{"rename directory", func() error { return cfs.Rename("vpc/sub", "vpc/moved") }, "vpc/moved", "f"},
		{"renamed directory's old listing", func() error { return nil }, "vpc", "moved,new,noexec"},
	}
	for _, step := range steps {
		// Cache the listings first, as a client that listed the
		// directories earlier would have.
		for _, dir := range []string{"", "vpc", "vpc/sub", "vpc/moved"} {
			_, _ = cfs.ReadDir(dir)
		}
		if err := step.change(); err != nil {
			t.Fatalf("%s: error = %v", step.name, err)
		}
		if got := listNames(t, cfs, step.dir); got != step.want {
			t.Fatalf("%s: ReadDir(%s) = %q, want %q", step.name, step.dir, got, step.want)
		}
	}

	// The old name of a renamed directory must not list its former contents.
	if entries, err := cfs.ReadDir("vpc/sub"); err == nil && len(entries) > 0 {
		t.Fatalf("ReadDir(vpc/sub) after rename lists %d entries, want none", len(entries))
	}
}

// changingFS makes a change in the middle of a ReadDir, the way a create
// racing a listing would.
type changingFS struct {
	billy.Filesystem
	during func()
}

func (c *changingFS) ReadDir(path string) ([]os.FileInfo, error) {
	entries, err := c.Filesystem.ReadDir(path)
	if c.during != nil {
		during := c.during
		c.during = nil
		during()
	}
	return entries, err
}

func TestCachedFilesystemDoesNotCacheListingTakenDuringChange(t *testing.T) {
	inner := &changingFS{Filesystem: memfs.New()}
	cfs := newTestCachedFS(t, inner)
	if err := cfs.MkdirAll("dir", 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	inner.during = func() { touch(t, cfs, "dir/late") }
	if got := listNames(t, cfs, "dir"); got != "" {
		t.Fatalf("first ReadDir = %q, want the listing from before the create", got)
	}
	if got := listNames(t, cfs, "dir"); got != "late" {
		t.Fatalf("ReadDir after racing create = %q, want %q", got, "late")
	}
}
