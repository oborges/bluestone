package server

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// These follow what Windows' own SMB server does, checked against it with
// scripts/smb-interop/windows-delete-test.ps1.

const (
	accessRead       uint32 = 0x00000001
	fileDirectory    uint32 = 0x00000001 // FILE_DIRECTORY_FILE
	createDeleteOnCl uint32 = wire.FileDeleteOnClose
)

// deleteClient is one connected session on a tree.
type deleteClient struct {
	t      *testing.T
	fc     *transport.FramedConn
	sessID uint64
	treeID uint32
	// early holds CHANGE_NOTIFY responses answered at once.
	early [][]byte
}

func connectDeleteClient(t *testing.T, srv *Server) *deleteClient {
	t.Helper()
	client, srvConn := newPipeConns()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(serveOn(srv, srvConn))
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	return &deleteClient{t: t, fc: fc, sessID: sessID, treeID: treeConnect(t, fc, sessID)}
}

func (c *deleteClient) open(name string, disposition, access, options uint32) ([16]byte, uint32) {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildCreateWith(c.sessID, c.treeID, name, disposition, access, options))
	rh, resp := readReply(c.t, c.fc)
	var fid [16]byte
	if rh.Status == wire.StatusSuccess {
		copy(fid[:], resp[64+64:64+80])
	}
	return fid, rh.Status
}

func (c *deleteClient) mustOpen(name string, disposition, access, options uint32) [16]byte {
	c.t.Helper()
	fid, status := c.open(name, disposition, access, options)
	if status != wire.StatusSuccess {
		c.t.Fatalf("open %s: status %#x", name, status)
	}
	return fid
}

func (c *deleteClient) setInfo(fid [16]byte, class uint8, buf []byte) uint32 {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildSetInfo(c.sessID, c.treeID, fid, class, buf))
	rh, _ := readReply(c.t, c.fc)
	return rh.Status
}

func (c *deleteClient) setDelete(fid [16]byte, pending bool) uint32 {
	c.t.Helper()
	return c.setInfo(fid, wire.FileDispositionInformation, []byte{boolToU8(pending)})
}

// rename sends FILE_RENAME_INFORMATION: ReplaceIfExists, seven reserved
// bytes, RootDirectory, FileNameLength, then the name.
func (c *deleteClient) rename(fid [16]byte, newName string, replace bool) uint32 {
	c.t.Helper()
	name := wire.UTF16ToBytes(newName)
	buf := make([]byte, 20+len(name))
	buf[0] = boolToU8(replace)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(name)))
	copy(buf[20:], name)
	return c.setInfo(fid, wire.FileRenameInformation, buf)
}

func (c *deleteClient) setReadOnly(fid [16]byte) {
	c.t.Helper()
	buf := make([]byte, 40)
	binary.LittleEndian.PutUint32(buf[32:36], 0x1) // FILE_ATTRIBUTE_READONLY
	if status := c.setInfo(fid, wire.FileBasicInfoClass, buf); status != wire.StatusSuccess {
		c.t.Fatalf("set read-only: %#x", status)
	}
}

// deletePending reads FileStandardInformation.DeletePending through fid.
func (c *deleteClient) deletePending(fid [16]byte) bool {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildQueryInfo(c.sessID, c.treeID, fid, wire.FileStandardInfoClass))
	rh, resp := readReply(c.t, c.fc)
	if rh.Status != wire.StatusSuccess {
		c.t.Fatalf("query standard info: %#x", rh.Status)
	}
	return resp[64+8+20] != 0
}

func (c *deleteClient) close(fid [16]byte) uint32 {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildClose(c.sessID, c.treeID, fid))
	rh, _ := readReply(c.t, c.fc)
	return rh.Status
}

func (c *deleteClient) write(fid [16]byte, data string) {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildWrite(c.sessID, c.treeID, fid, 0, []byte(data)))
	if rh, _ := readReply(c.t, c.fc); rh.Status != wire.StatusSuccess {
		c.t.Fatalf("write: %#x", rh.Status)
	}
}

// put creates name holding data.
func (c *deleteClient) put(name, data string) {
	c.t.Helper()
	fid := c.mustOpen(name, wire.FileOverwriteIf, genericAll, 0)
	c.write(fid, data)
	c.close(fid)
}

// lookup finds the node at a client path.
func (b *memBackend) lookup(name string) (*memNode, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.root
	for _, part := range strings.Split(strings.Trim(memClean(name), "/"), "/") {
		c, ok := n.children[part]
		if !ok {
			return nil, false
		}
		n = c
	}
	return n, true
}

func (b *memBackend) exists(name string) bool {
	_, ok := b.lookup(name)
	return ok
}

func (b *memBackend) contents(name string) string {
	n, ok := b.lookup(name)
	if !ok {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(n.data)
}

// A pending delete waits for the file's last handle, not the handle that
// asked for it; meanwhile every handle reports it and new opens are refused.
func TestDeletePendingWaitsForLastHandle(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	c.put("two.txt", "two handles")

	a := c.mustOpen("two.txt", wire.FileOpen, accessDelete, 0)
	b := c.mustOpen("two.txt", wire.FileOpen, accessRead, 0)
	if status := c.setDelete(a, true); status != wire.StatusSuccess {
		t.Fatalf("set disposition: %#x", status)
	}
	if !c.deletePending(b) {
		t.Error("the other handle does not report the pending delete")
	}
	c.close(a)
	if !backend.exists("two.txt") {
		t.Fatal("file deleted while another handle still had it open")
	}
	if _, status := c.open("two.txt", wire.FileOpen, accessRead, 0); status != wire.StatusDeletePending {
		t.Errorf("open while delete pending: %#x, want STATUS_DELETE_PENDING", status)
	}
	if _, status := c.open("TWO.TXT", wire.FileOverwriteIf, genericAll, 0); status != wire.StatusDeletePending {
		t.Errorf("overwrite while delete pending: %#x, want STATUS_DELETE_PENDING", status)
	}
	if backend.contents("two.txt") != "two handles" {
		t.Error("a refused overwrite truncated the file")
	}
	c.close(b)
	if backend.exists("two.txt") {
		t.Fatal("file survived its last handle closing")
	}
	// Once gone, the name is free again.
	c.close(c.mustOpen("two.txt", wire.FileCreate, genericAll, 0))
}

// The pending delete is shared by every client with the file open, not
// only handles on one connection.
func TestDeletePendingAcrossConnections(t *testing.T) {
	backend := newMemBackend()
	srv := newTestServer(backend)
	first := connectDeleteClient(t, srv)
	second := connectDeleteClient(t, srv)
	first.put("shared.txt", "x")

	a := first.mustOpen("shared.txt", wire.FileOpen, accessDelete, 0)
	b := second.mustOpen("shared.txt", wire.FileOpen, accessRead, 0)
	first.setDelete(a, true)
	first.close(a)
	if !second.deletePending(b) {
		t.Error("the other client's handle does not report the pending delete")
	}
	if _, status := second.open("shared.txt", wire.FileOpen, accessRead, 0); status != wire.StatusDeletePending {
		t.Errorf("other client's open: %#x, want STATUS_DELETE_PENDING", status)
	}
	second.close(b)
	if backend.exists("shared.txt") {
		t.Fatal("file survived its last handle closing")
	}
}

// Clearing the disposition before the last close keeps the file.
func TestDeleteDispositionCanBeCleared(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	c.put("undo.txt", "keep me")
	a := c.mustOpen("undo.txt", wire.FileOpen, accessDelete, 0)
	c.setDelete(a, true)
	if status := c.setDelete(a, false); status != wire.StatusSuccess {
		t.Fatalf("clear disposition: %#x", status)
	}
	c.close(a)
	if !backend.exists("undo.txt") {
		t.Fatal("file deleted after its disposition was cleared")
	}
}

// Delete-on-close takes effect when that open closes. The handle itself does
// not report the delete as pending before then, as on Windows.
func TestDeleteOnCloseFromCreate(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	fid := c.mustOpen("temp.txt", wire.FileOverwriteIf, genericAll, createDeleteOnCl)
	if c.deletePending(fid) {
		t.Error("delete-on-close handle reports the delete pending before it closes")
	}
	if !backend.exists("temp.txt") {
		t.Fatal("file missing while its delete-on-close handle is open")
	}
	c.close(fid)
	if backend.exists("temp.txt") {
		t.Fatal("delete-on-close file survived its close")
	}
}

// Deleting on close is deleting, so the open has to ask for delete access.
func TestDeleteNeedsDeleteAccess(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	c.put("guarded.txt", "x")
	if _, status := c.open("guarded.txt", wire.FileOpen, accessRead, createDeleteOnCl); status != wire.StatusAccessDenied {
		t.Errorf("delete-on-close without delete access: %#x, want ACCESS_DENIED", status)
	}
	fid := c.mustOpen("guarded.txt", wire.FileOpen, accessRead, 0)
	if status := c.setDelete(fid, true); status != wire.StatusAccessDenied {
		t.Errorf("disposition without delete access: %#x, want ACCESS_DENIED", status)
	}
	c.close(fid)
	if !backend.exists("guarded.txt") {
		t.Fatal("file deleted by an open without delete access")
	}
}

// A read-only file cannot be deleted until the attribute is cleared.
func TestDeleteRefusesReadOnlyFile(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	c.put("ro.txt", "x")
	fid := c.mustOpen("ro.txt", wire.FileOpen, genericAll, 0)
	c.setReadOnly(fid)
	if status := c.setDelete(fid, true); status != wire.StatusCannotDelete {
		t.Errorf("disposition on read-only file: %#x, want STATUS_CANNOT_DELETE", status)
	}
	c.close(fid)
	if _, status := c.open("ro.txt", wire.FileOpen, genericAll, createDeleteOnCl); status != wire.StatusCannotDelete {
		t.Errorf("delete-on-close of read-only file: %#x, want STATUS_CANNOT_DELETE", status)
	}
	if !backend.exists("ro.txt") {
		t.Fatal("read-only file deleted")
	}
}

// A directory with anything in it refuses a disposition outright, and one
// opened delete-on-close stays, with its handle closing cleanly.
func TestDeleteRefusesNonEmptyDirectory(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	c.close(c.mustOpen("full", wire.FileCreate, genericAll, fileDirectory))
	c.put(`full\child.txt`, "x")

	d := c.mustOpen("full", wire.FileOpen, genericAll, fileDirectory)
	if status := c.setDelete(d, true); status != wire.StatusDirectoryNotEmpty {
		t.Errorf("disposition on non-empty directory: %#x, want DIRECTORY_NOT_EMPTY", status)
	}
	c.close(d)

	d = c.mustOpen("full", wire.FileOpen, genericAll, fileDirectory|createDeleteOnCl)
	if status := c.close(d); status != wire.StatusSuccess {
		t.Errorf("closing a delete-on-close non-empty directory: %#x, want success", status)
	}
	if !backend.exists("full") || !backend.exists(`full\child.txt`) {
		t.Fatal("non-empty directory or its contents deleted")
	}

	// Once empty it goes.
	c.close(c.mustOpen("empty", wire.FileCreate, genericAll, fileDirectory))
	d = c.mustOpen("empty", wire.FileOpen, genericAll, fileDirectory)
	if status := c.setDelete(d, true); status != wire.StatusSuccess {
		t.Fatalf("disposition on empty directory: %#x", status)
	}
	c.close(d)
	if backend.exists("empty") {
		t.Fatal("empty directory survived its delete")
	}
}

// Renaming an open file and then deleting it through the same handle
// deletes the renamed file, not a new file that has taken the old name.
// This is how an application saves by renaming the old version aside.
func TestDeleteAfterRenameRemovesRenamedFile(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	c.put("saved.txt", "old version")

	old := c.mustOpen("saved.txt", wire.FileOpen, genericAll, 0)
	if status := c.rename(old, "backup.txt", false); status != wire.StatusSuccess {
		t.Fatalf("rename: %#x", status)
	}
	c.put("saved.txt", "new version")
	if status := c.setDelete(old, true); status != wire.StatusSuccess {
		t.Fatalf("set disposition: %#x", status)
	}
	c.close(old)

	if got := backend.contents("saved.txt"); got != "new version" {
		t.Fatalf("saved.txt = %q after deleting the renamed old version, want the new version", got)
	}
	if backend.exists("backup.txt") {
		t.Fatal("the renamed old version survived its delete")
	}
}

// Windows refuses to rename a directory with a file open inside it, to
// replace a file someone has open, and to rename a file waiting to be
// deleted.
func TestRenameRefusedAroundOpenHandles(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))

	c.close(c.mustOpen("busy", wire.FileCreate, genericAll, fileDirectory))
	c.put(`busy\open.txt`, "x")
	child := c.mustOpen(`busy\open.txt`, wire.FileOpen, accessRead, 0)
	dir := c.mustOpen("busy", wire.FileOpen, genericAll, fileDirectory)
	if status := c.rename(dir, "moved", false); status != wire.StatusAccessDenied {
		t.Errorf("rename directory with open child: %#x, want ACCESS_DENIED", status)
	}
	c.close(dir)
	c.close(child)

	c.put("target.txt", "target")
	c.put("incoming.txt", "incoming")
	target := c.mustOpen("target.txt", wire.FileOpen, accessRead, 0)
	incoming := c.mustOpen("incoming.txt", wire.FileOpen, genericAll, 0)
	if status := c.rename(incoming, "Target.txt", true); status != wire.StatusAccessDenied {
		t.Errorf("replace an open file: %#x, want ACCESS_DENIED", status)
	}
	c.close(incoming)
	c.close(target)
	if backend.contents("target.txt") != "target" {
		t.Fatal("an open file was replaced")
	}

	c.put("pend.txt", "x")
	pend := c.mustOpen("pend.txt", wire.FileOpen, genericAll, 0)
	c.setDelete(pend, true)
	if status := c.rename(pend, "pend2.txt", false); status != wire.StatusDeletePending {
		t.Errorf("rename a delete-pending file: %#x, want STATUS_DELETE_PENDING", status)
	}
	c.close(pend)
	if backend.exists("pend.txt") || backend.exists("pend2.txt") {
		t.Fatal("delete-pending file survived, or was renamed")
	}
}

// A client that disconnects with a delete-on-close file open still has it
// deleted, as when a process holding a temporary file exits.
func TestDeleteOnCloseAppliesOnDisconnect(t *testing.T) {
	backend := newMemBackend()
	srv := newTestServer(backend)
	c := connectDeleteClient(t, srv)
	c.mustOpen("scratch.tmp", wire.FileOverwriteIf, genericAll, createDeleteOnCl)

	mustWrite(t, c.fc, buildTreeDisconnect(c.sessID, c.treeID))
	if rh, _ := readReply(t, c.fc); rh.Status != wire.StatusSuccess {
		t.Fatalf("tree disconnect: %#x", rh.Status)
	}
	if backend.exists("scratch.tmp") {
		t.Fatal("delete-on-close file survived its client disconnecting")
	}
}

func buildTreeDisconnect(sessID uint64, treeID uint32) []byte {
	hdr := wire.NewHeader(wire.CmdTreeDisconnect)
	hdr.SessionId = sessID
	hdr.TreeId = treeID
	hdr.MessageId = 40
	hdr.Credit = 1
	body := make([]byte, 4)
	binary.LittleEndian.PutUint16(body[0:2], 4)
	return append(hdr.Append(nil), body...)
}
