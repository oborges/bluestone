package nfs

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/rpc"
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

// runNFSv4 sends one compound and returns a reader at its first result.
func runNFSv4(t *testing.T, fs billy.Filesystem, ops uint32, build func(req *nfs4Writer)) *nfs4Reader {
	t.Helper()
	return runNFSv4As(t, fs, nil, rpc.Auth{}, ops, build)
}

// runNFSv4As sends one compound with the given credentials to a server
// enforcing perms (nil for none).
func runNFSv4As(t *testing.T, fs billy.Filesystem, perms *Permissions, cred rpc.Auth, ops uint32, build func(req *nfs4Writer)) *nfs4Reader {
	t.Helper()
	handler := newNFSv4TestHandler(fs)
	srv := &Server{Handler: handler, ID: [8]byte{1}, Permissions: perms}
	body := bytes.NewBuffer(nil)
	req := newNFS4Writer(body)
	req.writeOpaque(nil)
	req.writeUint32(0)
	req.writeUint32(ops)
	build(req)
	w := &response{
		conn:     &conn{Server: srv},
		req:      &request{xid: 1, Header: rpc.Header{Cred: cred}, Body: bytes.NewReader(body.Bytes())},
		errorFmt: basicErrorFormatter,
		writer:   bytes.NewBuffer(nil),
	}
	if err := onNFSv4Compound(context.Background(), w, handler); err != nil {
		t.Fatalf("compound: %v", err)
	}
	resp := newNFS4Reader(bytes.NewReader(w.writer.Bytes()))
	for i := 0; i < 4; i++ { // xid, message type, reply state, auth flavor
		if _, err := resp.readUint32(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := resp.readOpaque(nfs4OpaqueLimit); err != nil { // verifier
		t.Fatal(err)
	}
	if _, err := resp.readUint32(); err != nil { // accept status
		t.Fatal(err)
	}
	if _, err := resp.readUint32(); err != nil { // compound status
		t.Fatal(err)
	}
	if _, err := resp.readOpaque(nfs4OpaqueLimit); err != nil { // tag
		t.Fatal(err)
	}
	if _, err := resp.readUint32(); err != nil { // result count
		t.Fatal(err)
	}
	return resp
}

// writeOwnerAttrs writes an fattr4 setting owner and owner_group.
func writeOwnerAttrs(req *nfs4Writer, owner, group string) {
	writeBitmap(req, bitmapFromAttrs(fattr4Owner, fattr4OwnerGroup))
	vals := bytes.NewBuffer(nil)
	vw := newNFS4Writer(vals)
	vw.writeOpaque([]byte(owner))
	vw.writeOpaque([]byte(group))
	req.writeOpaque(vals.Bytes())
}

func opStatus(t *testing.T, resp *nfs4Reader, want nfs4Op) nfs4Status {
	t.Helper()
	op, err := resp.readUint32()
	if err != nil || nfs4Op(op) != want {
		t.Fatalf("op = %d, %v; want %d", op, err, want)
	}
	status, err := resp.readUint32()
	if err != nil {
		t.Fatal(err)
	}
	return nfs4Status(status)
}

// SETATTR changes a file's owner and group, given as numeric ids, and
// GETATTR then reports them.
func TestNFSv4SetAttrOwner(t *testing.T) {
	fs := newOwnedFS(t)
	resp := runNFSv4(t, fs, 4, func(req *nfs4Writer) {
		req.writeUint32(uint32(opPutRootFH))
		req.writeUint32(uint32(opLookup))
		req.writeOpaque([]byte("f"))
		req.writeUint32(uint32(opSetAttr))
		req.writeFixedOpaque(make([]byte, 16))
		writeOwnerAttrs(req, "1234", "5678")
		req.writeUint32(uint32(opGetAttr))
		writeBitmap(req, bitmapFromAttrs(fattr4Owner, fattr4OwnerGroup))
	})
	assertOpStatus(t, resp, opPutRootFH)
	assertOpStatus(t, resp, opLookup)
	assertOpStatus(t, resp, opSetAttr)
	set, err := resp.readBitmap()
	if err != nil || !bitmapHas(set, fattr4Owner) || !bitmapHas(set, fattr4OwnerGroup) {
		t.Fatalf("attributes set = %v, %v; want owner and owner_group", set, err)
	}
	if got := fs.owners["/f"]; got != [2]uint32{1234, 5678} {
		t.Fatalf("stored owner %v, want [1234 5678]", got)
	}
	assertOpStatus(t, resp, opGetAttr)
	if _, err := resp.readBitmap(); err != nil {
		t.Fatal(err)
	}
	vals, err := resp.readOpaque(1024)
	if err != nil {
		t.Fatal(err)
	}
	vr := newNFS4Reader(bytes.NewReader(vals))
	owner, _ := vr.readOpaque(64)
	group, _ := vr.readOpaque(64)
	if string(owner) != "1234" || string(group) != "5678" {
		t.Fatalf("GETATTR owner %q group %q, want 1234 and 5678", owner, group)
	}
}

// Owners that are not numeric ids, which the server cannot map, are refused
// with NFS4ERR_BADOWNER and change nothing.
func TestNFSv4SetAttrBadOwner(t *testing.T) {
	for _, owner := range []string{"alice@example.com", "", "-1", "4294967296", "12a"} {
		fs := newOwnedFS(t)
		resp := runNFSv4(t, fs, 3, func(req *nfs4Writer) {
			req.writeUint32(uint32(opPutRootFH))
			req.writeUint32(uint32(opLookup))
			req.writeOpaque([]byte("f"))
			req.writeUint32(uint32(opSetAttr))
			req.writeFixedOpaque(make([]byte, 16))
			writeOwnerAttrs(req, owner, "5678")
		})
		assertOpStatus(t, resp, opPutRootFH)
		assertOpStatus(t, resp, opLookup)
		if status := opStatus(t, resp, opSetAttr); status != nfs4ErrBadOwner {
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
	resp := runNFSv4(t, fs, 2, func(req *nfs4Writer) {
		req.writeUint32(uint32(opPutRootFH))
		req.writeUint32(uint32(opCreate))
		req.writeUint32(uint32(nf4Dir))
		req.writeOpaque([]byte("d"))
		writeOwnerAttrs(req, strconv.Itoa(4321), "8765")
	})
	assertOpStatus(t, resp, opPutRootFH)
	assertOpStatus(t, resp, opCreate)
	if got := fs.owners["/d"]; got != [2]uint32{4321, 8765} {
		t.Fatalf("new directory owner %v, want [4321 8765]", got)
	}
}
