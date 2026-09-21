package server

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/auth"
	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// userRecorder is a backend that remembers who opened files through it.
type userRecorder struct {
	*memBackend
	mu    sync.Mutex
	users []*auth.Identity
}

func (b *userRecorder) Open(ctx context.Context, opts vfs.OpenOptions) (vfs.Handle, error) {
	b.mu.Lock()
	b.users = append(b.users, opts.User)
	b.mu.Unlock()
	return b.memBackend.Open(ctx, opts)
}

// accessServer serves "share" read-write, "ro" read-only and "secret" to
// nobody.
func accessServer(t *testing.T, backend vfs.Backend) *Server {
	t.Helper()
	srv := newTestServer(backend)
	for _, name := range []string{"ro", "secret"} {
		sh := vfs.NewDiskShare(name, backend)
		srv.shareByName[name] = sh
		srv.shares = append(srv.shares, sh)
	}
	srv.shareAccess = func(id *auth.Identity, share string) ShareAccess {
		if id == nil || id.Username != "guest" {
			t.Errorf("share access asked about %+v, want the session's identity", id)
		}
		switch share {
		case "ro":
			return ShareReadOnly
		case "secret":
			return ShareDenied
		}
		return ShareReadWrite
	}
	return srv
}

// connectTo connects a session to share, returning the tree connect's
// status and the maximal access it reports.
func connectTo(t *testing.T, srv *Server, share string) (*deleteClient, uint32, uint32) {
	t.Helper()
	client, srvConn := newPipeConns()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(serveOn(srv, srvConn))
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	mustWrite(t, fc, buildTreeConnect(sessID, `\\server\`+share))
	rh, resp := readReply(t, fc)
	var maximal uint32
	if rh.Status == wire.StatusSuccess {
		maximal = binary.LittleEndian.Uint32(resp[64+12 : 64+16])
	}
	return &deleteClient{t: t, fc: fc, sessID: sessID, treeID: rh.TreeId}, rh.Status, maximal
}

func TestShareAccessDenied(t *testing.T) {
	srv := accessServer(t, newMemBackend())
	if _, status, _ := connectTo(t, srv, "secret"); status != wire.StatusAccessDenied {
		t.Fatalf("tree connect to a share the user may not use: %#x, want ACCESS_DENIED", status)
	}
	if _, status, maximal := connectTo(t, srv, "share"); status != wire.StatusSuccess || maximal != maximalAccessFull {
		t.Fatalf("tree connect to a read-write share: %#x, maximal access %#x", status, maximal)
	}
}

// On a read-only share a user reads what is there and changes nothing,
// with the answers a Windows server gives.
func TestShareReadOnly(t *testing.T) {
	backend := newMemBackend()
	srv := accessServer(t, backend)
	rw, _, _ := connectTo(t, srv, "share")
	rw.put("doc.txt", "hello")

	ro, status, maximal := connectTo(t, srv, "ro")
	if status != wire.StatusSuccess {
		t.Fatalf("tree connect to a read-only share: %#x", status)
	}
	if maximal != maximalAccessRead {
		t.Errorf("maximal access %#x, want %#x (read and execute)", maximal, maximalAccessRead)
	}

	fid := ro.mustOpen("doc.txt", wire.FileOpen, accessRead, 0)
	mustWrite(t, ro.fc, buildWrite(ro.sessID, ro.treeID, fid, 0, []byte("x")))
	if rh, _ := readReply(t, ro.fc); rh.Status != wire.StatusAccessDenied {
		t.Errorf("write: %#x, want ACCESS_DENIED", rh.Status)
	}
	if status := ro.setDelete(fid, true); status != wire.StatusAccessDenied {
		t.Errorf("set delete disposition: %#x, want ACCESS_DENIED", status)
	}
	if status := ro.rename(fid, "moved.txt", false); status != wire.StatusAccessDenied {
		t.Errorf("rename: %#x, want ACCESS_DENIED", status)
	}
	ro.close(fid)

	// An existing file opened with OPEN_IF for reading is fine.
	fid = ro.mustOpen("doc.txt", wire.FileOpenIf, accessRead, 0)
	ro.close(fid)

	for _, tc := range []struct {
		name                string
		file                string
		disposition, access uint32
		options             uint32
	}{
		{"open for writing", "doc.txt", wire.FileOpen, 0x2, 0},
		{"open with GENERIC_ALL", "doc.txt", wire.FileOpen, genericAll, 0},
		{"open to delete", "doc.txt", wire.FileOpen, accessDelete, 0},
		{"delete on close", "doc.txt", wire.FileOpen, accessRead, createDeleteOnCl},
		{"overwrite", "doc.txt", wire.FileOverwriteIf, accessRead, 0},
		{"create", "new.txt", wire.FileCreate, accessRead, 0},
		{"create with OPEN_IF", "new.txt", wire.FileOpenIf, accessRead, 0},
		{"create a directory", "dir", wire.FileOpenIf, accessRead, fileDirectory},
	} {
		if _, status := ro.open(tc.file, tc.disposition, tc.access, tc.options); status != wire.StatusAccessDenied {
			t.Errorf("%s: %#x, want ACCESS_DENIED", tc.name, status)
		}
	}
	if backend.exists("new.txt") || backend.exists("dir") {
		t.Error("the read-only share created a file")
	}
	if got := backend.contents("doc.txt"); got != "hello" {
		t.Errorf("doc.txt = %q after the read-only share was used", got)
	}
	// A file that is not there is still not found when only opened.
	if _, status := ro.open("missing.txt", wire.FileOpen, accessRead, 0); status != wire.StatusObjectNameNotFound {
		t.Errorf("open a missing file: %#x, want OBJECT_NAME_NOT_FOUND", status)
	}
}

// The backend learns who opened a file.
func TestOpenCarriesIdentity(t *testing.T) {
	backend := &userRecorder{memBackend: newMemBackend()}
	c, _, _ := connectTo(t, accessServer(t, backend), "share")
	c.put("mine.txt", "x")
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.users) == 0 || backend.users[len(backend.users)-1] == nil || backend.users[len(backend.users)-1].Username != "guest" {
		t.Fatalf("opens carried %v, want the session's identity", backend.users)
	}
}

// Closing a handle releases its byte-range locks: another open can then
// lock the same bytes without waiting for the first session to end.
func TestCloseReleasesLocks(t *testing.T) {
	const exclusive = 0x2 | 0x10 // SMB2_LOCKFLAG_EXCLUSIVE_LOCK | FAIL_IMMEDIATELY
	srv := newTestServer(newMemBackend())
	a := connectDeleteClient(t, srv)
	a.put("locked.txt", "0123456789")
	b := connectDeleteClient(t, srv)

	lock := func(c *deleteClient, fid [16]byte) uint32 {
		mustWrite(t, c.fc, buildLock(c.sessID, c.treeID, fid, 0, 10, exclusive))
		rh, _ := readReply(t, c.fc)
		return rh.Status
	}
	held := a.mustOpen("locked.txt", wire.FileOpen, accessRead, 0)
	if status := lock(a, held); status != wire.StatusSuccess {
		t.Fatalf("first lock: %#x", status)
	}
	other := b.mustOpen("locked.txt", wire.FileOpen, accessRead, 0)
	if status := lock(b, other); status != wire.StatusLockNotGranted {
		t.Fatalf("conflicting lock while held: %#x, want LOCK_NOT_GRANTED", status)
	}
	a.close(held)
	if status := lock(b, other); status != wire.StatusSuccess {
		t.Fatalf("lock after the holder closed its handle: %#x, want it granted", status)
	}
}
