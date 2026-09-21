package server

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// leaseClient is a client with its own GUID, which scopes its lease keys.
type leaseClient struct {
	*deleteClient
	// pending holds messages read while waiting for something else.
	pending [][]byte
}

func connectLeaseClient(t *testing.T, srv *Server, guid byte) *leaseClient {
	t.Helper()
	client, srvConn := newPipeConns()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(serveOn(srv, srvConn))
	fc := transport.NewFramedConn(client)

	body := make([]byte, 38)
	binary.LittleEndian.PutUint16(body[0:2], 36)
	binary.LittleEndian.PutUint16(body[2:4], 1)
	binary.LittleEndian.PutUint32(body[8:12], wire.CapLeasing)
	body[12] = guid
	binary.LittleEndian.PutUint16(body[36:38], wire.DialectSMB302)
	hdr := wire.NewHeader(wire.CmdNegotiate)
	hdr.Credit = 1
	mustWrite(t, fc, append(hdr.Append(nil), body...))
	rh, resp := readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("negotiate: %#x", rh.Status)
	}
	if caps := binary.LittleEndian.Uint32(resp[64+24 : 64+28]); caps&wire.CapLeasing == 0 {
		t.Fatalf("server capabilities %#x lack leasing", caps)
	}
	sessID := sessionSetup(t, fc)
	return &leaseClient{deleteClient: &deleteClient{t: t, fc: fc, sessID: sessID, treeID: treeConnect(t, fc, sessID)}}
}

func newLeaseServer(backend vfs.Backend, timeout time.Duration) *Server {
	srv := newTestServer(backend)
	srv.leasesEnabled = true
	srv.leaseBreakTimeout = timeout
	return srv
}

func leaseKey(b byte) [16]byte {
	var k [16]byte
	for i := range k {
		k[i] = b
	}
	return k
}

// buildLeaseCreate opens name asking for a V2 lease in state, sharing
// share with other opens.
func buildLeaseCreate(sessID uint64, treeID uint32, name string, disposition, share uint32, key [16]byte, state uint32) []byte {
	msg := buildCreateWith(sessID, treeID, name, disposition, genericAll, 0)
	binary.LittleEndian.PutUint32(msg[wire.HeaderSize+32:wire.HeaderSize+36], share)
	msg[wire.HeaderSize+3] = wire.OplockLevelLease
	for len(msg)%8 != 0 {
		msg = append(msg, 0)
	}
	ctxStart := len(msg)
	lease := wire.LeaseRequest{Key: key, State: state, V2: true}
	msg = append(msg, make([]byte, 24)...)
	ctx := msg[ctxStart:]
	binary.LittleEndian.PutUint16(ctx[4:6], 16) // NameOffset
	binary.LittleEndian.PutUint16(ctx[6:8], 4)  // NameLength
	binary.LittleEndian.PutUint16(ctx[10:12], 24)
	data := lease.Encode()
	binary.LittleEndian.PutUint32(ctx[12:16], uint32(len(data)))
	copy(ctx[16:20], wire.CreateContextLease)
	msg = append(msg, data...)
	binary.LittleEndian.PutUint32(msg[wire.HeaderSize+48:wire.HeaderSize+52], uint32(ctxStart))
	binary.LittleEndian.PutUint32(msg[wire.HeaderSize+52:wire.HeaderSize+56], uint32(len(msg)-ctxStart))
	return msg
}

// isNotification reports whether a message is an unsolicited break.
func isNotification(msg []byte) bool {
	return binary.LittleEndian.Uint64(msg[24:32]) == ^uint64(0)
}

// reply reads the next response to a request, keeping any break
// notifications that arrive first.
func (c *leaseClient) reply() (wire.Header, []byte) {
	c.t.Helper()
	for {
		rh, msg := readReply(c.t, c.fc)
		if isNotification(msg) {
			c.pending = append(c.pending, msg)
			continue
		}
		if rh.Status == wire.StatusPending {
			continue // an interim response: the final one follows
		}
		return rh, msg
	}
}

// notification returns the next break notification, waiting up to 2s.
func (c *leaseClient) notification() []byte {
	c.t.Helper()
	if len(c.pending) > 0 {
		msg := c.pending[0]
		c.pending = c.pending[1:]
		return msg
	}
	_ = c.fc.Underlying().SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = c.fc.Underlying().SetReadDeadline(time.Time{}) }()
	msg, err := c.fc.ReadMessage()
	if err != nil {
		c.t.Fatalf("no break notification: %v", err)
	}
	if !isNotification(msg) {
		c.t.Fatalf("expected a break notification, got command %d", binary.LittleEndian.Uint16(msg[12:14]))
	}
	return msg
}

// noNotification checks that no break arrives within a moment.
func (c *leaseClient) noNotification() {
	c.t.Helper()
	if len(c.pending) > 0 {
		c.t.Fatal("unexpected break notification")
	}
	_ = c.fc.Underlying().SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	defer func() { _ = c.fc.Underlying().SetReadDeadline(time.Time{}) }()
	if msg, err := c.fc.ReadMessage(); err == nil {
		c.t.Fatalf("unexpected message, command %d", binary.LittleEndian.Uint16(msg[12:14]))
	}
}

// openLease opens name with a lease, returning the handle, oplock level and
// granted lease state.
func (c *leaseClient) openLease(name string, disposition, share uint32, key [16]byte, state uint32) ([16]byte, uint8, uint32, uint32) {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildLeaseCreate(c.sessID, c.treeID, name, disposition, share, key, state))
	rh, resp := c.reply()
	var fid [16]byte
	if rh.Status != wire.StatusSuccess {
		return fid, 0, 0, rh.Status
	}
	copy(fid[:], resp[64+64:64+80])
	level := resp[64+2]
	var granted uint32
	if off := binary.LittleEndian.Uint32(resp[64+80 : 64+84]); off != 0 {
		ctx := resp[off:]
		dataOff := binary.LittleEndian.Uint16(ctx[10:12])
		lease, err := wire.ParseLeaseRequest(ctx[dataOff:])
		if err != nil {
			c.t.Fatal(err)
		}
		if lease.Key != key {
			c.t.Fatal("lease response names another key")
		}
		granted = lease.State
	}
	return fid, level, granted, rh.Status
}

func (c *leaseClient) writeFile(fid [16]byte, data string) {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildWrite(c.sessID, c.treeID, fid, 0, []byte(data)))
	if rh, _ := c.reply(); rh.Status != wire.StatusSuccess {
		c.t.Fatalf("write: %#x", rh.Status)
	}
}

func (c *leaseClient) closeFile(fid [16]byte) {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildClose(c.sessID, c.treeID, fid))
	if rh, _ := c.reply(); rh.Status != wire.StatusSuccess {
		c.t.Fatalf("close: %#x", rh.Status)
	}
}

// ack acknowledges a lease break at state.
func (c *leaseClient) ack(key [16]byte, state uint32) uint32 {
	c.t.Helper()
	hdr := wire.NewHeader(wire.CmdOplockBreak)
	hdr.SessionId = c.sessID
	hdr.TreeId = c.treeID
	hdr.MessageId = 60
	hdr.Credit = 1
	body := make([]byte, 36)
	binary.LittleEndian.PutUint16(body[0:2], 36)
	copy(body[8:24], key[:])
	binary.LittleEndian.PutUint32(body[24:28], state)
	mustWrite(c.t, c.fc, append(hdr.Append(nil), body...))
	rh, _ := c.reply()
	return rh.Status
}

type leaseBreak struct {
	flags, current, next uint32
	key                  [16]byte
}

func parseLeaseBreak(t *testing.T, msg []byte) leaseBreak {
	t.Helper()
	body := msg[wire.HeaderSize:]
	if size := binary.LittleEndian.Uint16(body[0:2]); size != 44 {
		t.Fatalf("break notification StructureSize %d, want a lease break (44)", size)
	}
	var b leaseBreak
	b.flags = binary.LittleEndian.Uint32(body[4:8])
	copy(b.key[:], body[8:24])
	b.current = binary.LittleEndian.Uint32(body[24:28])
	b.next = binary.LittleEndian.Uint32(body[28:32])
	return b
}

const shareAll uint32 = 7

// Read and handle caching are granted; write caching never is, so a client
// never holds writes the server has not seen.
func TestLeaseGrantsReadHandleNotWrite(t *testing.T) {
	srv := newLeaseServer(newMemBackend(), 0)
	a := connectLeaseClient(t, srv, 1)
	all := wire.LeaseRead | wire.LeaseHandle | wire.LeaseWrite
	_, level, granted, status := a.openLease("f.txt", wire.FileOverwriteIf, shareAll, leaseKey(1), all)
	if status != wire.StatusSuccess || level != wire.OplockLevelLease || granted != wire.LeaseRead|wire.LeaseHandle {
		t.Fatalf("status %#x level %#x granted %#x; want a read-handle lease", status, level, granted)
	}
	// Handle caching alone is not a state a lease can be in.
	_, _, granted, _ = a.openLease("g.txt", wire.FileOverwriteIf, shareAll, leaseKey(2), wire.LeaseHandle)
	if granted != 0 {
		t.Fatalf("handle-only request granted %#x, want none", granted)
	}
}

// Another client's write breaks read caching. A read-only lease is broken
// without an acknowledgement; a read-handle one needs one.
func TestLeaseBrokenByAnotherClientsWrite(t *testing.T) {
	srv := newLeaseServer(newMemBackend(), 0)
	a := connectLeaseClient(t, srv, 1)
	b := connectLeaseClient(t, srv, 2)

	a.openLease("r.txt", wire.FileOverwriteIf, shareAll, leaseKey(1), wire.LeaseRead)
	a.openLease("rh.txt", wire.FileOverwriteIf, shareAll, leaseKey(2), wire.LeaseRead|wire.LeaseHandle)

	bf := b.mustOpen("r.txt", wire.FileOpen, genericAll, 0)
	b.writeFile(bf, "changed")
	got := parseLeaseBreak(t, a.notification())
	if got.key != leaseKey(1) || got.current != wire.LeaseRead || got.next != 0 || got.flags != 0 {
		t.Fatalf("break = %+v, want R to none without acknowledgement", got)
	}

	bf = b.mustOpen("rh.txt", wire.FileOpen, genericAll, 0)
	b.writeFile(bf, "changed")
	got = parseLeaseBreak(t, a.notification())
	if got.key != leaseKey(2) || got.current != wire.LeaseRead|wire.LeaseHandle || got.next != 0 || got.flags != 1 {
		t.Fatalf("break = %+v, want RH to none, acknowledgement required", got)
	}
	if status := a.ack(leaseKey(2), 0); status != wire.StatusSuccess {
		t.Fatalf("acknowledging the break: %#x", status)
	}
	// Nothing is breaking any more.
	if status := a.ack(leaseKey(2), 0); status == wire.StatusSuccess {
		t.Fatal("a second acknowledgement was accepted")
	}
}

// A client's writes through its own lease do not break it, nor do another
// open's with the same lease key.
func TestLeaseNotBrokenByItsOwnWrites(t *testing.T) {
	srv := newLeaseServer(newMemBackend(), 0)
	a := connectLeaseClient(t, srv, 1)
	fid, _, _, _ := a.openLease("own.txt", wire.FileOverwriteIf, shareAll, leaseKey(1), wire.LeaseRead|wire.LeaseHandle)
	a.writeFile(fid, "mine")
	second, _, _, _ := a.openLease("own.txt", wire.FileOpen, shareAll, leaseKey(1), wire.LeaseRead|wire.LeaseHandle)
	a.writeFile(second, "mine again")
	a.noNotification()
}

// An open that would conflict with a handle another client only caches
// waits for that client to be told: it closes the handle, and the open
// succeeds. One that still conflicts once the break is acknowledged fails as
// it would have.
func TestSharingViolationWaitsForHandleBreak(t *testing.T) {
	srv := newLeaseServer(&sharingBackend{memBackend: newMemBackend()}, 0)
	a := connectLeaseClient(t, srv, 1)
	b := connectLeaseClient(t, srv, 2)

	held, _, _, _ := a.openLease("held.txt", wire.FileOverwriteIf, 0, leaseKey(1), wire.LeaseRead|wire.LeaseHandle)
	mustWrite(t, b.fc, buildCreateWith(b.sessID, b.treeID, "held.txt", wire.FileOpen, genericAll, 0))
	if rh, _ := readReply(t, b.fc); rh.Status != wire.StatusPending {
		t.Fatalf("conflicting open: %#x, want it to wait (STATUS_PENDING)", rh.Status)
	}
	got := parseLeaseBreak(t, a.notification())
	if got.current != wire.LeaseRead|wire.LeaseHandle || got.next != wire.LeaseRead || got.flags != 1 {
		t.Fatalf("break = %+v, want RH to R, acknowledgement required", got)
	}
	// As Windows does: close the cached handle, then acknowledge.
	a.closeFile(held)
	a.ack(leaseKey(1), wire.LeaseRead)
	if rh, _ := b.reply(); rh.Status != wire.StatusSuccess {
		t.Fatalf("open after the holder let go: %#x, want success", rh.Status)
	}

	// This time the holder keeps its handle open.
	a.openLease("kept.txt", wire.FileOverwriteIf, 0, leaseKey(2), wire.LeaseRead|wire.LeaseHandle)
	mustWrite(t, b.fc, buildCreateWith(b.sessID, b.treeID, "kept.txt", wire.FileOpen, genericAll, 0))
	a.notification()
	a.ack(leaseKey(2), wire.LeaseRead)
	if rh, _ := b.reply(); rh.Status != wire.StatusSharingViolation {
		t.Fatalf("open while the holder kept its handle: %#x, want SHARING_VIOLATION", rh.Status)
	}
}

// A client that never acknowledges does not hold others up for long.
func TestHandleBreakTimesOut(t *testing.T) {
	srv := newLeaseServer(&sharingBackend{memBackend: newMemBackend()}, 200*time.Millisecond)
	a := connectLeaseClient(t, srv, 1)
	b := connectLeaseClient(t, srv, 2)
	a.openLease("slow.txt", wire.FileOverwriteIf, 0, leaseKey(1), wire.LeaseRead|wire.LeaseHandle)
	start := time.Now()
	mustWrite(t, b.fc, buildCreateWith(b.sessID, b.treeID, "slow.txt", wire.FileOpen, genericAll, 0))
	if rh, _ := b.reply(); rh.Status != wire.StatusSharingViolation {
		t.Fatalf("open: %#x, want SHARING_VIOLATION once the break times out", rh.Status)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("waited %s for an unacknowledged break", waited)
	}
}

// Changes made outside the server, as over NFS, break leases when the
// backend reports them; changes the server made itself are not broken a
// second time.
func TestLeaseBrokenByExternalChange(t *testing.T) {
	backend := &changeFeed{memBackend: newMemBackend()}
	srv := newLeaseServer(backend, 0)
	a := connectLeaseClient(t, srv, 1)
	a.openLease(`dir\f.txt`, wire.FileOverwriteIf, shareAll, leaseKey(1), wire.LeaseRead)
	if backend.fn == nil {
		t.Fatal("the lease table did not subscribe to the backend's changes")
	}

	backend.fn(vfs.Change{Action: vfs.ChangeModified, Path: `DIR\F.txt`, Filter: FileNotifyChangeSize | FileNotifyChangeLastWrite})
	a.noNotification()

	backend.fn(vfs.Change{Action: vfs.ChangeModified, Path: `DIR\F.txt`, Filter: FileNotifyChangeAttributes, External: true})
	a.noNotification()

	backend.fn(vfs.Change{Action: vfs.ChangeModified, Path: `DIR\F.txt`, Filter: FileNotifyChangeSize | FileNotifyChangeLastWrite, External: true})
	if got := parseLeaseBreak(t, a.notification()); got.key != leaseKey(1) || got.next != 0 {
		t.Fatalf("break = %+v, want the lease broken to none", got)
	}
}

// A client asking for an oplock gets level II, read caching, and loses it
// to another client's write without being asked to acknowledge.
func TestLevelIIOplock(t *testing.T) {
	srv := newLeaseServer(newMemBackend(), 0)
	a := connectLeaseClient(t, srv, 1)
	b := connectLeaseClient(t, srv, 2)
	msg := buildCreateWith(a.sessID, a.treeID, "op.txt", wire.FileOverwriteIf, genericAll, 0)
	msg[wire.HeaderSize+3] = 0x09 // batch
	mustWrite(t, a.fc, msg)
	rh, resp := a.reply()
	if rh.Status != wire.StatusSuccess || resp[64+2] != wire.OplockLevelII {
		t.Fatalf("status %#x oplock %#x, want level II", rh.Status, resp[64+2])
	}
	var fid [16]byte
	copy(fid[:], resp[64+64:64+80])

	bf := b.mustOpen("op.txt", wire.FileOpen, genericAll, 0)
	b.writeFile(bf, "x")
	note := a.notification()
	body := note[wire.HeaderSize:]
	if size := binary.LittleEndian.Uint16(body[0:2]); size != 24 || body[2] != wire.OplockLevelNone {
		t.Fatalf("oplock break size %d level %d, want 24 and none", size, body[2])
	}
	var got [16]byte
	copy(got[:], body[8:24])
	if got != fid {
		t.Fatal("oplock break names another handle")
	}
}

// A lease key names one file; using it for another is refused, until the
// lease is gone with its last open.
func TestLeaseKeyNamesOneFile(t *testing.T) {
	srv := newLeaseServer(newMemBackend(), 0)
	a := connectLeaseClient(t, srv, 1)
	fid, _, _, _ := a.openLease("one.txt", wire.FileOverwriteIf, shareAll, leaseKey(1), wire.LeaseRead)
	if _, _, _, status := a.openLease("two.txt", wire.FileOverwriteIf, shareAll, leaseKey(1), wire.LeaseRead); status != wire.StatusInvalidParameter {
		t.Fatalf("same key on another file: %#x, want INVALID_PARAMETER", status)
	}
	a.closeFile(fid)
	if _, _, granted, status := a.openLease("two.txt", wire.FileOverwriteIf, shareAll, leaseKey(1), wire.LeaseRead); status != wire.StatusSuccess || granted != wire.LeaseRead {
		t.Fatalf("key reused after its lease ended: %#x granted %#x", status, granted)
	}
}

// sharingBackend enforces share modes the simple way: an open that shares
// nothing excludes every other open of the file while it lasts.
type sharingBackend struct {
	*memBackend
	held map[string]int
}

func (b *sharingBackend) Open(ctx context.Context, opts vfs.OpenOptions) (vfs.Handle, error) {
	b.memBackend.mu.Lock()
	if b.held == nil {
		b.held = map[string]int{}
	}
	if b.held[opts.Path] > 0 {
		b.memBackend.mu.Unlock()
		return nil, vfs.ErrSharingViolation
	}
	b.memBackend.mu.Unlock()
	h, err := b.memBackend.Open(ctx, opts)
	if err != nil || opts.ShareAccess != 0 {
		return h, err
	}
	b.memBackend.mu.Lock()
	b.held[opts.Path]++
	b.memBackend.mu.Unlock()
	return &releasingHandle{Handle: h, release: func() {
		b.memBackend.mu.Lock()
		b.held[opts.Path]--
		b.memBackend.mu.Unlock()
	}}, nil
}

type releasingHandle struct {
	vfs.Handle
	release func()
}

func (h *releasingHandle) Close(ctx context.Context) error {
	h.release()
	return h.Handle.Close(ctx)
}
