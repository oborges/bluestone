package vfs

import (
	"os"
	"testing"
	"time"

	"github.com/oborges/bluestone/internal/posix"
	nfs "github.com/willscott/go-nfs"
)

// A directory's modification time moves forward whenever an entry is added,
// removed or renamed in it, whichever protocol's view made the change, and
// Stat, listings, the SMB attributes and the NFS attributes all report it.
// The stored directory marker never changes, so without this NFSv4 clients
// saw the same change attribute before and after a create and kept serving
// a cached listing that lacked the new file.
func TestDirectoryTimeFollowsEntries(t *testing.T) {
	fs := newDirtyStagingTestFilesystemWithStore(t, newTestStagingManager(t), newFakeObjectStore())
	smb := fs.WithWindowsNames().ForProtocol("smb")
	if err := fs.MkdirAll("dir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.MkdirAll("other", 0o755); err != nil {
		t.Fatal(err)
	}

	last := dirTime(t, fs, "dir")
	step := func(what string, change func()) {
		t.Helper()
		change()
		now := dirTime(t, fs, "dir")
		if !now.After(last) {
			t.Fatalf("after %s: dir modified at %v, not after %v", what, now, last)
		}
		last = now
	}

	step("a create", func() {
		f, err := fs.OpenFile("dir/a.txt", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	})
	step("a create over SMB", func() {
		f, err := smb.OpenFile("dir/b.txt", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	})
	step("a mkdir", func() {
		if err := fs.MkdirAll("dir/sub", 0o755); err != nil {
			t.Fatal(err)
		}
	})
	step("a rename within it", func() {
		if err := fs.Rename("dir/a.txt", "dir/c.txt"); err != nil {
			t.Fatal(err)
		}
	})
	step("a rename out of it", func() {
		if err := fs.Rename("dir/c.txt", "other/c.txt"); err != nil {
			t.Fatal(err)
		}
	})
	step("a rename into it", func() {
		if err := fs.Rename("other/c.txt", "dir/c.txt"); err != nil {
			t.Fatal(err)
		}
	})
	step("a remove", func() {
		if err := fs.Remove("dir/c.txt"); err != nil {
			t.Fatal(err)
		}
	})
	step("a change found in the bucket", func() {
		fs.ReportExternalChange(posix.ObjectChange{Path: "/dir/external.txt", Kind: posix.ObjectAdded})
	})

	// Writing to a file changes the file, not the directory.
	f, err := fs.OpenFile("dir/b.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if now := dirTime(t, fs, "dir"); !now.Equal(last) {
		t.Errorf("a write to a file moved its directory's time from %v to %v", last, now)
	}

	// Every way of looking at the directory agrees.
	entries, err := fs.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "dir" && !e.ModTime().Equal(last) {
			t.Errorf("listing: dir modified at %v, want %v", e.ModTime(), last)
		}
	}
	info, err := smb.Stat("dir")
	if err != nil {
		t.Fatal(err)
	}
	if attrs := FileAttributes(info); !attrs.Mtime.Equal(last) || attrs.Ctime.Before(last) {
		t.Errorf("SMB attributes: mtime %v ctime %v, want both %v", attrs.Mtime, attrs.Ctime, last)
	}
	info, _ = fs.Stat("dir")
	if got := nfs.ToFileAttribute(info, "/dir").Mtime; got.Seconds != uint32(last.Unix()) || got.Nseconds != uint32(last.Nanosecond()) {
		t.Errorf("NFS attributes: mtime %+v, want %v", got, last)
	}

	// The subdirectory created in it is tracked apart.
	if sub := dirTime(t, fs, "dir/sub"); sub.After(last) {
		t.Errorf("dir/sub modified at %v, after its parent's last change at %v", sub, last)
	}

	// Times set explicitly win over the recorded one.
	set := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := fs.Chtimes("dir", set, set); err != nil {
		t.Fatal(err)
	}
	if now := dirTime(t, fs, "dir"); !now.Equal(set) {
		t.Errorf("after Chtimes: dir modified at %v, want %v", now, set)
	}
}

// Changes landing within the clock's resolution still give the directory a
// new time each.
func TestDirTimesAlwaysMoveForward(t *testing.T) {
	var d dirTimes
	var prev time.Time
	for i := 0; i < 1000; i++ {
		d.record(Change{Action: ChangeAdded, Path: "/dir/f"})
		now, ok := d.modTime("/dir")
		if !ok || !now.After(prev) {
			t.Fatalf("change %d: time %v, not after %v", i, now, prev)
		}
		prev = now
	}
}

// Past its bound, the record forgets the directories changed longest ago.
func TestDirTimesForgetsOldest(t *testing.T) {
	var d dirTimes
	d.touch("/oldest")
	for i := 0; i < maxTrackedDirs; i++ {
		d.touch("/d" + string(rune('a'+i%26)) + time.Duration(i).String())
	}
	if len(d.dirs) > maxTrackedDirs {
		t.Fatalf("tracking %d directories, more than %d", len(d.dirs), maxTrackedDirs)
	}
	if _, ok := d.modTime("/oldest"); ok {
		t.Error("the directory changed longest ago is still tracked")
	}
}

func dirTime(t *testing.T, fs *Filesystem, name string) time.Time {
	t.Helper()
	info, err := fs.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", name)
	}
	return info.ModTime()
}
