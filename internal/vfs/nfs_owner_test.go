package vfs

import (
	"os"
	"path"
	"testing"

	nfs "github.com/willscott/go-nfs"
)

// nfsOwner is the owner the NFS server reports for info.
func nfsOwner(info os.FileInfo, p string) (uint32, uint32) {
	attr := nfs.ToFileAttribute(info, p)
	return attr.UID, attr.GID
}

// NFS reports each file's stored owner, whatever kind of entry the
// filesystem answers with. It used to report every file as root's.
func TestNFSReportsStoredOwner(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	store.putWithMetadata("stored.txt", []byte("data"), existingObjectMetadata(0o644, 42, 7))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	info, err := fs.Stat("stored.txt")
	if err != nil {
		t.Fatal(err)
	}
	if uid, gid := nfsOwner(info, "/stored.txt"); uid != 42 || gid != 7 {
		t.Errorf("stored object: owner %d:%d, want 42:7", uid, gid)
	}
	// The file id is still the path's, as before.
	plain := nfs.ToFileAttribute(struct{ os.FileInfo }{info}, "/stored.txt")
	if got := nfs.ToFileAttribute(info, "/stored.txt").Fileid; got != plain.Fileid {
		t.Errorf("file id changed: %d, want %d", got, plain.Fileid)
	}
	if plain.UID != 0 {
		t.Errorf("an entry that keeps no owner reports uid %d, want 0 as before", plain.UID)
	}

	// A new file, staged, is owned by the default until chowned.
	f, err := fs.OpenFile("new.txt", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	info, err = fs.Stat("new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if uid, gid := nfsOwner(info, "/new.txt"); uid != 1000 || gid != 1000 {
		t.Errorf("new file: owner %d:%d, want the default 1000:1000", uid, gid)
	}
	if err := fs.Chown("new.txt", 2001, 2002); err != nil {
		t.Fatal(err)
	}
	info, err = fs.Stat("new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if uid, gid := nfsOwner(info, "/new.txt"); uid != 2001 || gid != 2002 {
		t.Errorf("staged file after chown: owner %d:%d, want 2001:2002", uid, gid)
	}

	// Listings report owners too, through the Windows-named view as well.
	for _, view := range []*Filesystem{fs, fs.WithWindowsNames()} {
		entries, err := view.ReadDir("/")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range entries {
			if e.Name() != "new.txt" {
				continue
			}
			found = true
			if uid, gid := nfsOwner(e, path.Join("/", e.Name())); uid != 2001 || gid != 2002 {
				t.Errorf("listing (windows names %v): owner %d:%d, want 2001:2002", view.windowsNames, uid, gid)
			}
		}
		if !found {
			t.Errorf("listing (windows names %v) has no new.txt", view.windowsNames)
		}
	}
}

// Chowning a file to root over NFS sticks. The NFS server compares the new
// owner with the current one, which it used to read as root for every file,
// so the change was skipped.
func TestNFSChownToRoot(t *testing.T) {
	manager := newTestStagingManager(t)
	store := newFakeObjectStore()
	store.putWithMetadata("owned.txt", []byte("data"), existingObjectMetadata(0o644, 42, 7))
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	root := uint32(0)
	set := nfs.SetFileAttributes{SetUID: &root, SetGID: &root}
	if err := set.Apply(fs, fs, "owned.txt"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := fs.Stat("owned.txt")
	if err != nil {
		t.Fatal(err)
	}
	if attrs := FileAttributes(info); attrs.UID != 0 || attrs.GID != 0 {
		t.Errorf("after chown to root: stored owner %d:%d, want 0:0", attrs.UID, attrs.GID)
	}
}

// A new file keeps the mode it was created with, rather than the 0600 a
// staged file starts at.
func TestNewFileKeepsCreationMode(t *testing.T) {
	fs := newDirtyStagingTestFilesystemWithStore(t, newTestStagingManager(t), newFakeObjectStore())
	f, err := fs.OpenFile("new.txt", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	info, err := fs.Stat("new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("new file mode %o, want 644", got)
	}
}
