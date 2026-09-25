package nfs

import (
	"bytes"
	"os"
	"testing"

	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// authSys encodes AUTH_SYS credentials as a client sends them.
func authSys(uid, gid uint32, groups ...uint32) rpc.Auth {
	var body bytes.Buffer
	if groups == nil {
		groups = []uint32{}
	}
	if err := xdr.Write(&body, authSysParms{Stamp: 42, Machine: "client", UID: uid, GID: gid, Groups: groups}); err != nil {
		panic(err)
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

var (
	readOp  = nfs4TestOp{nfs4OpRead, nfs4ReadArgs{Count: 16}}
	writeOp = nfs4TestOp{nfs4OpWrite, nfs4WriteArgs{Stable: uint32(fileSync), Data: []byte("x")}}
)

// thirdOpStatus runs a three-op compound (PUTROOTFH, LOOKUP name, op) as
// cred and returns the last op's status.
func thirdOpStatus(t *testing.T, fs *ownedFS, perms *Permissions, cred rpc.Auth, name string, op nfs4TestOp) nfs4Status {
	t.Helper()
	_, resp := runNFSv4As(t, fs, perms, cred, append(putFile(name), op)...)
	nfs4ExpectOp(t, resp, nfs4OpPutRootFH, nfs4OK, nil)
	nfs4ExpectOp(t, resp, nfs4OpLookup, nfs4OK, nil)
	var header nfs4ResultHeader
	if err := xdr.Read(resp, &header); err != nil || header.Op != op.op {
		t.Fatalf("result = %+v, %v; want op %d", header, err, op.op)
	}
	return header.Status
}

func TestNFSv4DataPermissions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		perms *Permissions
		cred  rpc.Auth
		file  string
		op    nfs4TestOp
		want  nfs4Status
	}{
		{"read a world-readable file", enforced, bob, "f", readOp, nfs4OK},
		{"read another user's private file", enforced, bob, "secret", readOp, nfs4ErrAccess},
		{"write another user's file", enforced, bob, "f", writeOp, nfs4ErrAccess},
		{"owner writes their read-only file", enforced, bob, "mine", writeOp, nfs4OK},
		{"root reads anything", enforced, authSys(0, 0), "secret", readOp, nfs4OK},
		{"squashed root reads as nobody", &Permissions{RootSquash: true}, authSys(0, 0), "secret", readOp, nfs4ErrAccess},
		{"no credentials act as nobody", enforced, rpc.Auth{}, "secret", readOp, nfs4ErrAccess},
		{"without enforcement anyone writes", nil, bob, "f", writeOp, nfs4OK},
	} {
		if got := thirdOpStatus(t, permFS(t), tc.perms, tc.cred, tc.file, tc.op); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// openOp is an OPEN of name in the current directory, creating it if
// create is set.
func openOp(name string, access uint32, create bool) nfs4TestOp {
	args := nfs4OpenArgs{
		ShareAccess: access,
		Owner:       nfs4Owner{ClientID: 1, Owner: "own"},
		Claim:       nfs4ClaimNull,
		File:        name,
	}
	if create {
		args.OpenType = nfs4OpenCreate
		args.How = nfs4CreateHow{Mode: nfs4CreateUnchecked}
	}
	return nfs4TestOp{nfs4OpOpen, args}
}

func TestNFSv4CreatePermissions(t *testing.T) {
	fs := permFS(t)
	readWrite := nfs4ShareAccessRead | nfs4ShareAccessWrite
	if got := thirdOpStatus(t, fs, enforced, bob, "closed", openOp("new", readWrite, true)); got != nfs4ErrAccess {
		t.Errorf("create in a directory bob may not write: status %d, want NFS4ERR_ACCESS", got)
	}
	if got := thirdOpStatus(t, fs, enforced, bob, "shared", openOp("new", readWrite, true)); got != nfs4OK {
		t.Fatalf("create in a world-writable directory: status %d", got)
	}
	if owner := fs.owners["/shared/new"]; owner != [2]uint32{1001, 1001} {
		t.Errorf("new file owned by %v, want its creator 1001:1001", owner)
	}
	remove := nfs4TestOp{nfs4OpRemove, nfs4RemoveArgs{Name: "x"}}
	if got := thirdOpStatus(t, fs, enforced, bob, "closed", remove); got != nfs4ErrAccess {
		t.Errorf("remove from a directory bob may not write: status %d, want NFS4ERR_ACCESS", got)
	}
	// Opening an existing file checks its mode, with no exception for the
	// owner: bob's read-only file cannot be opened for writing.
	status, _ := runNFSv4As(t, fs, enforced, bob, nfs4TestOp{nfs4OpPutRootFH, nil}, openOp("mine", nfs4ShareAccessWrite, false))
	if status != nfs4ErrAccess {
		t.Errorf("open bob's 0444 file for writing: status %d, want NFS4ERR_ACCESS", status)
	}
}

// setAttrOp is a SETATTR of mode and/or owner.
func setAttrOp(t *testing.T, mode *uint32, owner string) nfs4TestOp {
	var vals bytes.Buffer
	var mask nfs4Bitmap
	if mode != nil {
		mask.set(nfs4AttrMode)
		if err := xdr.Write(&vals, *mode); err != nil {
			t.Fatal(err)
		}
	}
	if owner != "" {
		mask.set(nfs4AttrOwner)
		if err := xdr.Write(&vals, owner); err != nil {
			t.Fatal(err)
		}
	}
	return nfs4TestOp{nfs4OpSetAttr, nfs4SetAttrArgs{Attrs: nfs4FAttr{Mask: mask, Vals: vals.Bytes()}}}
}

func TestNFSv4SetAttrPermissions(t *testing.T) {
	mode := uint32(0o600)
	for _, tc := range []struct {
		name string
		cred rpc.Auth
		file string
		op   nfs4TestOp
		want nfs4Status
	}{
		{"chmod another user's file", bob, "f", setAttrOp(t, &mode, ""), nfs4ErrPerm},
		{"chmod your own file", bob, "mine", setAttrOp(t, &mode, ""), nfs4OK},
		{"give your file away", bob, "mine", setAttrOp(t, nil, "1000"), nfs4ErrPerm},
		{"root gives a file away", authSys(0, 0), "f", setAttrOp(t, nil, "1001"), nfs4OK},
	} {
		if got := thirdOpStatus(t, permFS(t), enforced, tc.cred, tc.file, tc.op); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// ACCESS reports what the caller may actually do.
func TestNFSv4AccessReportsPermissions(t *testing.T) {
	const all = nfs4AccessRead | nfs4AccessLookup | nfs4AccessModify | nfs4AccessExtend | nfs4AccessDelete | nfs4AccessExecute
	for _, tc := range []struct {
		file string
		want uint32
	}{
		{"f", nfs4AccessRead},
		{"secret", 0},
		{"mine", nfs4AccessRead},
		{"shared", nfs4AccessRead | nfs4AccessLookup | nfs4AccessModify | nfs4AccessExtend | nfs4AccessDelete},
		{"closed", nfs4AccessRead | nfs4AccessLookup},
	} {
		_, resp := runNFSv4As(t, permFS(t), enforced, bob, append(putFile(tc.file), nfs4TestOp{nfs4OpAccess, nfs4AccessArgs{Access: all}})...)
		nfs4ExpectOp(t, resp, nfs4OpPutRootFH, nfs4OK, nil)
		nfs4ExpectOp(t, resp, nfs4OpLookup, nfs4OK, nil)
		var res nfs4AccessRes
		nfs4ExpectOp(t, resp, nfs4OpAccess, nfs4OK, &res)
		if res.Access != tc.want {
			t.Errorf("ACCESS on %s: %#x, want %#x", tc.file, res.Access, tc.want)
		}
	}
}
