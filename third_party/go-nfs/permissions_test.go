package nfs

import (
	"bytes"
	"os"
	"testing"

	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// authSys encodes AUTH_SYS credentials as a client sends them.
func authSys(uid, gid uint32, groups ...uint32) rpc.Auth {
	body := bytes.NewBuffer(nil)
	w := newNFS4Writer(body)
	w.writeUint32(42) // stamp
	w.writeOpaque([]byte("client"))
	w.writeUint32(uid)
	w.writeUint32(gid)
	w.writeUint32(uint32(len(groups)))
	for _, g := range groups {
		w.writeUint32(g)
	}
	return rpc.Auth{Flavor: authUnix, Body: body.Bytes()}
}

func TestParseAuthUnix(t *testing.T) {
	c, err := parseAuthUnix(authSys(1001, 100, 20, 30).Body)
	if err != nil {
		t.Fatal(err)
	}
	if c.UID != 1001 || c.GID != 100 || len(c.Groups) != 2 || c.Groups[1] != 30 {
		t.Fatalf("parsed %+v", c)
	}
	if _, err := parseAuthUnix([]byte{0, 0, 0, 1}); err == nil {
		t.Error("a truncated body parsed")
	}
}

func TestMay(t *testing.T) {
	file := &FileAttribute{Type: FileTypeRegular, FileMode: 0o640, UID: 1000, GID: 100}
	dir := &FileAttribute{Type: FileTypeDirectory, FileMode: uint32(os.ModeDir | 0o700), UID: 1000, GID: 100}
	for _, tc := range []struct {
		name string
		c    Credentials
		attr *FileAttribute
		want uint32
		ok   bool
	}{
		{"owner reads and writes", Credentials{UID: 1000, GID: 1}, file, mayRead | mayWrite, true},
		{"group reads", Credentials{UID: 1001, GID: 100}, file, mayRead, true},
		{"group may not write", Credentials{UID: 1001, GID: 100}, file, mayWrite, false},
		{"supplementary group reads", Credentials{UID: 1001, GID: 1, Groups: []uint32{100}}, file, mayRead, true},
		{"others may not read", Credentials{UID: 1001, GID: 1}, file, mayRead, false},
		{"owner class is used even when group has more", Credentials{UID: 1000, GID: 100}, &FileAttribute{FileMode: 0o070, UID: 1000, GID: 100}, mayRead, false},
		{"root writes anything", Credentials{}, file, mayWrite, true},
		{"root may not execute a file nobody may", Credentials{}, file, mayExec, false},
		{"root searches any directory", Credentials{}, dir, mayExec, true},
		{"others may not search the directory", Credentials{UID: 1001, GID: 1}, dir, mayExec, false},
	} {
		if got := tc.c.may(tc.attr, tc.want); got != tc.ok {
			t.Errorf("%s: may = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

// permFS is a filesystem with a directory and files of set owners and modes:
//
//	/         0:0       0755 (directory)
//	/f        1000:1000 0644
//	/secret   1000:1000 0600
//	/mine     1001:1001 0444
//	/shared   1000:1000 0777 (directory)
//	/closed   1000:1000 0755 (directory)
func permFS(t *testing.T) *ownedFS {
	fs := newOwnedFS(t)
	for _, name := range []string{"/secret", "/mine"} {
		f, err := fs.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	for _, name := range []string{"/shared", "/closed"} {
		if err := fs.MkdirAll(name, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fs.owners["/secret"] = [2]uint32{1000, 1000}
	fs.owners["/mine"] = [2]uint32{1001, 1001}
	fs.owners["/shared"] = [2]uint32{1000, 1000}
	fs.owners["/closed"] = [2]uint32{1000, 1000}
	fs.modes = map[string]os.FileMode{"/": 0o755, "/f": 0o644, "/secret": 0o600, "/mine": 0o444, "/shared": 0o777, "/closed": 0o755}
	return fs
}

var (
	enforced = &Permissions{}
	bob      = authSys(1001, 1001)
)

// putFile starts a compound at the root and looks name up.
func putFile(req *nfs4Writer, name string) {
	req.writeUint32(uint32(opPutRootFH))
	req.writeUint32(uint32(opLookup))
	req.writeOpaque([]byte(name))
}

func readOp(req *nfs4Writer) {
	req.writeUint32(uint32(opRead))
	req.writeFixedOpaque(make([]byte, 16))
	req.writeUint64(0)
	req.writeUint32(16)
}

func writeOp(req *nfs4Writer) {
	req.writeUint32(uint32(opWrite))
	req.writeFixedOpaque(make([]byte, 16))
	req.writeUint64(0)
	req.writeUint32(uint32(fileSync))
	req.writeOpaque([]byte("x"))
}

// thirdOpStatus runs a three-op compound (PUTROOTFH, LOOKUP name, op) as
// cred and returns the last op's status.
func thirdOpStatus(t *testing.T, fs *ownedFS, perms *Permissions, cred rpc.Auth, name string, op nfs4Op, build func(*nfs4Writer)) nfs4Status {
	t.Helper()
	resp := runNFSv4As(t, fs, perms, cred, 3, func(req *nfs4Writer) {
		putFile(req, name)
		build(req)
	})
	assertOpStatus(t, resp, opPutRootFH)
	assertOpStatus(t, resp, opLookup)
	return opStatus(t, resp, op)
}

func TestNFSv4DataPermissions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		perms *Permissions
		cred  rpc.Auth
		file  string
		op    nfs4Op
		build func(*nfs4Writer)
		want  nfs4Status
	}{
		{"read a world-readable file", enforced, bob, "f", opRead, readOp, nfs4OK},
		{"read another user's private file", enforced, bob, "secret", opRead, readOp, nfs4ErrAccess},
		{"write another user's file", enforced, bob, "f", opWrite, writeOp, nfs4ErrAccess},
		{"owner writes their read-only file", enforced, bob, "mine", opWrite, writeOp, nfs4OK},
		{"root reads anything", enforced, authSys(0, 0), "secret", opRead, readOp, nfs4OK},
		{"squashed root reads as nobody", &Permissions{RootSquash: true}, authSys(0, 0), "secret", opRead, readOp, nfs4ErrAccess},
		{"no credentials act as nobody", enforced, rpc.Auth{}, "secret", opRead, readOp, nfs4ErrAccess},
		{"without enforcement anyone writes", nil, bob, "f", opWrite, writeOp, nfs4OK},
	} {
		if got := thirdOpStatus(t, permFS(t), tc.perms, tc.cred, tc.file, tc.op, tc.build); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// openCreate is an OPEN that creates name in the current directory.
func openCreate(name string) func(*nfs4Writer) {
	return func(req *nfs4Writer) {
		req.writeUint32(uint32(opOpen))
		req.writeUint32(0)                                            // seqid
		req.writeUint32(open4ShareAccessRead | open4ShareAccessWrite) // share_access
		req.writeUint32(open4ShareDenyNone)
		req.writeUint64(1)             // clientid
		req.writeOpaque([]byte("own")) // owner
		req.writeUint32(open4Create)
		req.writeUint32(open4Unchecked)
		writeBitmap(req, nil)
		req.writeOpaque(nil)
		req.writeUint32(claimNull)
		req.writeOpaque([]byte(name))
	}
}

func TestNFSv4CreatePermissions(t *testing.T) {
	fs := permFS(t)
	if got := thirdOpStatus(t, fs, enforced, bob, "closed", opOpen, openCreate("new")); got != nfs4ErrAccess {
		t.Errorf("create in a directory bob may not write: status %d, want NFS4ERR_ACCESS", got)
	}
	if got := thirdOpStatus(t, fs, enforced, bob, "shared", opOpen, openCreate("new")); got != nfs4OK {
		t.Fatalf("create in a world-writable directory: status %d", got)
	}
	if owner := fs.owners["/shared/new"]; owner != [2]uint32{1001, 1001} {
		t.Errorf("new file owned by %v, want its creator 1001:1001", owner)
	}
	remove := func(req *nfs4Writer) {
		req.writeUint32(uint32(opRemove))
		req.writeOpaque([]byte("x"))
	}
	if got := thirdOpStatus(t, fs, enforced, bob, "closed", opRemove, remove); got != nfs4ErrAccess {
		t.Errorf("remove from a directory bob may not write: status %d, want NFS4ERR_ACCESS", got)
	}
	// Opening an existing file checks its mode, with no exception for the
	// owner: bob's read-only file cannot be opened for writing.
	openExisting := func(req *nfs4Writer) {
		req.writeUint32(uint32(opOpen))
		req.writeUint32(0)
		req.writeUint32(open4ShareAccessWrite)
		req.writeUint32(open4ShareDenyNone)
		req.writeUint64(1)
		req.writeOpaque([]byte("own"))
		req.writeUint32(open4NoCreate)
		req.writeUint32(claimNull)
		req.writeOpaque([]byte("mine"))
	}
	resp := runNFSv4As(t, fs, enforced, bob, 2, func(req *nfs4Writer) {
		req.writeUint32(uint32(opPutRootFH))
		openExisting(req)
	})
	assertOpStatus(t, resp, opPutRootFH)
	if got := opStatus(t, resp, opOpen); got != nfs4ErrAccess {
		t.Errorf("open bob's 0444 file for writing: status %d, want NFS4ERR_ACCESS", got)
	}
}

// setAttrOp writes a SETATTR of mode and/or owner.
func setAttrOp(mode *uint32, owner string) func(*nfs4Writer) {
	return func(req *nfs4Writer) {
		req.writeUint32(uint32(opSetAttr))
		req.writeFixedOpaque(make([]byte, 16))
		vals := bytes.NewBuffer(nil)
		vw := newNFS4Writer(vals)
		attrs := []uint32{}
		if mode != nil {
			attrs = append(attrs, fattr4Mode)
			vw.writeUint32(*mode)
		}
		if owner != "" {
			attrs = append(attrs, fattr4Owner)
			vw.writeOpaque([]byte(owner))
		}
		writeBitmap(req, bitmapFromAttrs(attrs...))
		req.writeOpaque(vals.Bytes())
	}
}

func TestNFSv4SetAttrPermissions(t *testing.T) {
	mode := uint32(0o600)
	for _, tc := range []struct {
		name  string
		cred  rpc.Auth
		file  string
		build func(*nfs4Writer)
		want  nfs4Status
	}{
		{"chmod another user's file", bob, "f", setAttrOp(&mode, ""), nfs4ErrPerm},
		{"chmod your own file", bob, "mine", setAttrOp(&mode, ""), nfs4OK},
		{"give your file away", bob, "mine", setAttrOp(nil, "1000"), nfs4ErrPerm},
		{"root gives a file away", authSys(0, 0), "f", setAttrOp(nil, "1001"), nfs4OK},
	} {
		if got := thirdOpStatus(t, permFS(t), enforced, tc.cred, tc.file, opSetAttr, tc.build); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// ACCESS reports what the caller may actually do.
func TestNFSv4AccessReportsPermissions(t *testing.T) {
	const all = access4Read | access4Lookup | access4Modify | access4Extend | access4Delete | access4Execute
	for _, tc := range []struct {
		file string
		want uint32
	}{
		{"f", access4Read},
		{"secret", 0},
		{"mine", access4Read},
		{"shared", access4Read | access4Lookup | access4Modify | access4Extend | access4Delete},
		{"closed", access4Read | access4Lookup},
	} {
		resp := runNFSv4As(t, permFS(t), enforced, bob, 3, func(req *nfs4Writer) {
			putFile(req, tc.file)
			req.writeUint32(uint32(opAccess))
			req.writeUint32(all)
		})
		assertOpStatus(t, resp, opPutRootFH)
		assertOpStatus(t, resp, opLookup)
		assertOpStatus(t, resp, opAccess)
		if _, err := resp.readUint32(); err != nil { // supported
			t.Fatal(err)
		}
		granted, err := resp.readUint32()
		if err != nil {
			t.Fatal(err)
		}
		if granted != tc.want {
			t.Errorf("ACCESS on %s: %#x, want %#x", tc.file, granted, tc.want)
		}
	}
}
