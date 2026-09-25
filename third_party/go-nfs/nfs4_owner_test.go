package nfs

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
	"github.com/willscott/go-nfs/helpers/memfs"
)

// ownedFS is a memfs that keeps owners, as a filesystem storing them in
// metadata does, and reports them through file.Owner.
type ownedFS struct {
	billy.Filesystem
	owners map[string][2]uint32
	// modes overrides the permission bits memfs reports.
	modes map[string]os.FileMode
}

type ownedFileInfo struct {
	os.FileInfo
	uid, gid uint32
	perm     *os.FileMode
}

func (i ownedFileInfo) NFSOwner() (uint32, uint32) { return i.uid, i.gid }

func (i ownedFileInfo) Mode() os.FileMode {
	if i.perm == nil {
		return i.FileInfo.Mode()
	}
	return i.FileInfo.Mode()&^os.ModePerm | *i.perm
}

func (o *ownedFS) owned(name string, info os.FileInfo) os.FileInfo {
	key := o.Join("/", name)
	ids := o.owners[key]
	out := ownedFileInfo{FileInfo: info, uid: ids[0], gid: ids[1]}
	if m, ok := o.modes[key]; ok {
		out.perm = &m
	}
	return out
}

func (o *ownedFS) Lstat(name string) (os.FileInfo, error) {
	info, err := o.Filesystem.Lstat(name)
	if err != nil {
		return nil, err
	}
	return o.owned(name, info), nil
}

func (o *ownedFS) Stat(name string) (os.FileInfo, error) {
	info, err := o.Filesystem.Stat(name)
	if err != nil {
		return nil, err
	}
	return o.owned(name, info), nil
}

func (o *ownedFS) Chmod(name string, mode os.FileMode) error {
	if o.modes == nil {
		o.modes = map[string]os.FileMode{}
	}
	o.modes[o.Join("/", name)] = mode & os.ModePerm
	return nil
}
func (o *ownedFS) Chtimes(string, time.Time, time.Time) error { return nil }
func (o *ownedFS) Chown(name string, uid, gid int) error      { return o.Lchown(name, uid, gid) }
func (o *ownedFS) Lchown(name string, uid, gid int) error {
	o.owners[o.Join("/", name)] = [2]uint32{uint32(uid), uint32(gid)}
	return nil
}

func newOwnedFS(t *testing.T) *ownedFS {
	t.Helper()
	fs := memfs.New()
	if err := fs.MkdirAll("/", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := fs.Create("/f")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return &ownedFS{Filesystem: fs, owners: map[string][2]uint32{"/f": {1000, 1000}}}
}

// runNFSv4As sends ops as one compound with the given credentials to a
// server enforcing perms (nil for none), and returns the compound status and
// a reader at its first result.
func runNFSv4As(t *testing.T, fs billy.Filesystem, perms *Permissions, cred rpc.Auth, ops ...nfs4TestOp) (nfs4Status, io.Reader) {
	t.Helper()
	handler := newNFSv4TestHandler(fs)
	srv := &Server{Handler: handler, ID: [8]byte{1}, Permissions: perms}
	req := &request{xid: 1, Header: rpc.Header{Cred: cred}, Body: bytes.NewReader(nfs4CompoundRequest(t, ops...))}
	return nfs4CompoundReply(t, srv, handler, req)
}

// ownerAttrs is an fattr4 setting owner and owner_group.
func ownerAttrs(t *testing.T, owner, group string) nfs4FAttr {
	t.Helper()
	var vals bytes.Buffer
	if err := xdr.Write(&vals, [2]string{owner, group}); err != nil {
		t.Fatal(err)
	}
	return nfs4FAttr{Mask: nfs4BitmapOf(nfs4AttrOwner, nfs4AttrOwnerGroup), Vals: vals.Bytes()}
}

// putFile starts a compound at the root and looks name up.
func putFile(name string) []nfs4TestOp {
	return []nfs4TestOp{{nfs4OpPutRootFH, nil}, {nfs4OpLookup, nfs4LookupArgs{Name: name}}}
}

// SETATTR changes a file's owner and group, given as numeric ids, and
// GETATTR then reports them.
func TestNFSv4SetAttrOwner(t *testing.T) {
	fs := newOwnedFS(t)
	ops := append(putFile("f"),
		nfs4TestOp{nfs4OpSetAttr, nfs4SetAttrArgs{Attrs: ownerAttrs(t, "1234", "5678")}},
		nfs4TestOp{nfs4OpGetAttr, nfs4GetAttrArgs{Request: nfs4BitmapOf(nfs4AttrOwner, nfs4AttrOwnerGroup)}},
	)
	status, resp := runNFSv4As(t, fs, nil, rpc.Auth{}, ops...)
	if status != nfs4OK {
		t.Fatalf("status %d", status)
	}
	nfs4ExpectOp(t, resp, nfs4OpPutRootFH, nfs4OK, nil)
	nfs4ExpectOp(t, resp, nfs4OpLookup, nfs4OK, nil)
	var set nfs4Bitmap
	nfs4ExpectOp(t, resp, nfs4OpSetAttr, nfs4OK, &set)
	if !set.has(nfs4AttrOwner) || !set.has(nfs4AttrOwnerGroup) {
		t.Fatalf("attributes set = %v, want owner and owner_group", set)
	}
	if got := fs.owners["/f"]; got != [2]uint32{1234, 5678} {
		t.Fatalf("stored owner %v, want [1234 5678]", got)
	}
	var attrs nfs4FAttr
	nfs4ExpectOp(t, resp, nfs4OpGetAttr, nfs4OK, &attrs)
	var got [2]string
	if err := xdr.Read(bytes.NewReader(attrs.Vals), &got); err != nil {
		t.Fatal(err)
	}
	if got != [2]string{"1234", "5678"} {
		t.Fatalf("GETATTR owner and group %q, want 1234 and 5678", got)
	}
}

// Owners that are not numeric ids, which the server cannot map, are refused
// with NFS4ERR_BADOWNER and change nothing.
func TestNFSv4SetAttrBadOwner(t *testing.T) {
	for _, owner := range []string{"alice@example.com", "", "-1", "4294967296", "12a"} {
		fs := newOwnedFS(t)
		ops := append(putFile("f"), nfs4TestOp{nfs4OpSetAttr, nfs4SetAttrArgs{Attrs: ownerAttrs(t, owner, "5678")}})
		if status, _ := runNFSv4As(t, fs, nil, rpc.Auth{}, ops...); status != nfs4ErrBadOwner {
			t.Errorf("owner %q: status %d, want NFS4ERR_BADOWNER", owner, status)
		}
		if got := fs.owners["/f"]; got != [2]uint32{1000, 1000} {
			t.Errorf("owner %q: stored owner changed to %v", owner, got)
		}
	}
}

// A directory created with an owner gets it, as the reply says.
func TestNFSv4CreateWithOwner(t *testing.T) {
	fs := newOwnedFS(t)
	status, _ := runNFSv4As(t, fs, nil, rpc.Auth{},
		nfs4TestOp{nfs4OpPutRootFH, nil},
		nfs4TestOp{nfs4OpCreate, nfs4CreateArgs{Type: FileTypeDirectory, Name: "d", Attrs: ownerAttrs(t, strconv.Itoa(4321), "8765")}},
	)
	if status != nfs4OK {
		t.Fatalf("CREATE: status %d", status)
	}
	if got := fs.owners["/d"]; got != [2]uint32{4321, 8765} {
		t.Fatalf("new directory owner %v, want [4321 8765]", got)
	}
}
