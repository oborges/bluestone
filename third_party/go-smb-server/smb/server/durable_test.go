package server

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

func newDurableServer(backend *memBackend, sharing bool) *Server {
	var srv *Server
	if sharing {
		srv = newLeaseServer(&sharingBackend{memBackend: backend}, 0)
	} else {
		srv = newLeaseServer(backend, 0)
	}
	srv.durableEnabled = true
	return srv
}

// encodeContexts writes create contexts as a chain, as a client does.
func encodeContexts(contexts []wire.CreateContext) []byte {
	var out []byte
	for i, c := range contexts {
		start := len(out)
		dataOff := (16 + len(c.Name) + 7) &^ 7
		entry := make([]byte, dataOff+len(c.Data))
		binary.LittleEndian.PutUint16(entry[4:6], 16)
		binary.LittleEndian.PutUint16(entry[6:8], uint16(len(c.Name)))
		binary.LittleEndian.PutUint16(entry[10:12], uint16(dataOff))
		binary.LittleEndian.PutUint32(entry[12:16], uint32(len(c.Data)))
		copy(entry[16:], c.Name)
		copy(entry[dataOff:], c.Data)
		out = append(out, entry...)
		if i < len(contexts)-1 {
			for (len(out)-start)%8 != 0 {
				out = append(out, 0)
			}
			binary.LittleEndian.PutUint32(out[start:start+4], uint32(len(out)-start))
		}
	}
	return out
}

// buildCreateContexts opens name with the given oplock level and contexts.
func buildCreateContexts(sessID uint64, treeID uint32, name string, disposition, share uint32, oplock uint8, contexts []wire.CreateContext) []byte {
	msg := buildCreateWith(sessID, treeID, name, disposition, genericAll, 0)
	binary.LittleEndian.PutUint32(msg[wire.HeaderSize+32:wire.HeaderSize+36], share)
	msg[wire.HeaderSize+3] = oplock
	for len(msg)%8 != 0 {
		msg = append(msg, 0)
	}
	start := len(msg)
	msg = append(msg, encodeContexts(contexts)...)
	binary.LittleEndian.PutUint32(msg[wire.HeaderSize+48:wire.HeaderSize+52], uint32(start))
	binary.LittleEndian.PutUint32(msg[wire.HeaderSize+52:wire.HeaderSize+56], uint32(len(msg)-start))
	return msg
}

// responseContexts reads a CREATE response's contexts by name.
func responseContexts(t *testing.T, resp []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	off := binary.LittleEndian.Uint32(resp[64+80 : 64+84])
	length := binary.LittleEndian.Uint32(resp[64+84 : 64+88])
	if off == 0 || length == 0 {
		return out
	}
	buf := resp[off : off+length]
	for {
		next := binary.LittleEndian.Uint32(buf[0:4])
		nameOff := binary.LittleEndian.Uint16(buf[4:6])
		nameLen := binary.LittleEndian.Uint16(buf[6:8])
		dataOff := binary.LittleEndian.Uint16(buf[10:12])
		dataLen := binary.LittleEndian.Uint32(buf[12:16])
		out[string(buf[nameOff:nameOff+nameLen])] = buf[dataOff : uint32(dataOff)+dataLen]
		if next == 0 {
			return out
		}
		buf = buf[next:]
	}
}

func leaseContext(key [16]byte, state uint32) wire.CreateContext {
	return wire.CreateContext{Name: wire.CreateContextLease, Data: wire.LeaseRequest{Key: key, State: state, V2: true}.Encode()}
}

func durableV2Context(guid [16]byte, timeoutMs uint32) wire.CreateContext {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint32(data[0:4], timeoutMs)
	copy(data[16:32], guid[:])
	return wire.CreateContext{Name: wire.CreateContextDurableV2, Data: data}
}

func reconnectV2Context(fid, guid [16]byte) wire.CreateContext {
	data := make([]byte, 36)
	copy(data[0:16], fid[:])
	copy(data[16:32], guid[:])
	return wire.CreateContext{Name: wire.CreateContextDurableV2Reconnect, Data: data}
}

const rh = wire.LeaseRead | wire.LeaseHandle

// openDurable opens name durably with a read-handle lease, returning the
// handle and the timeout granted (0 if the open was not made durable).
func (c *leaseClient) openDurable(name string, share uint32, key, guid [16]byte, timeoutMs uint32, state uint32) ([16]byte, uint32) {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildCreateContexts(c.sessID, c.treeID, name, wire.FileOverwriteIf, share, wire.OplockLevelLease,
		[]wire.CreateContext{leaseContext(key, state), durableV2Context(guid, timeoutMs)}))
	rh, resp := c.reply()
	if rh.Status != wire.StatusSuccess {
		c.t.Fatalf("durable open: %#x", rh.Status)
	}
	var fid [16]byte
	copy(fid[:], resp[64+64:64+80])
	granted := uint32(0)
	if data, ok := responseContexts(c.t, resp)[wire.CreateContextDurableV2]; ok {
		granted = binary.LittleEndian.Uint32(data[0:4])
	}
	return fid, granted
}

// reconnect reclaims a durable open, returning its status, handle and the
// lease state reported.
func (c *leaseClient) reconnect(name string, fid, guid, key [16]byte, withLease bool) (uint32, [16]byte, uint32) {
	c.t.Helper()
	contexts := []wire.CreateContext{reconnectV2Context(fid, guid)}
	oplock := uint8(0)
	if withLease {
		contexts = append(contexts, leaseContext(key, rh))
		oplock = wire.OplockLevelLease
	}
	mustWrite(c.t, c.fc, buildCreateContexts(c.sessID, c.treeID, name, wire.FileOpen, shareAll, oplock, contexts))
	rh, resp := c.reply()
	var got [16]byte
	var state uint32
	if rh.Status == wire.StatusSuccess {
		copy(got[:], resp[64+64:64+80])
		if data, ok := responseContexts(c.t, resp)[wire.CreateContextLease]; ok {
			lease, _ := wire.ParseLeaseRequest(data)
			state = lease.State
		}
	}
	return rh.Status, got, state
}

// drop cuts the client's connection without logging off, as a network
// failure does.
func (c *leaseClient) drop() {
	_ = c.fc.Underlying().Close()
}

// waitPreserved waits for the server to notice the connection is gone.
func waitPreserved(t *testing.T, srv *Server, fid [16]byte) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for srv.durableTable().lookup(fid) == nil {
		if time.Now().After(deadline) {
			t.Fatal("durable open was not kept when its connection dropped")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *leaseClient) lock(fid [16]byte, offset, length uint64, flags uint32) uint32 {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildLock(c.sessID, c.treeID, fid, offset, length, flags))
	rh, _ := c.reply()
	return rh.Status
}

// A durable open survives its connection dropping: the client reconnects,
// reclaims the same handle with its lease, and goes on using it, and its
// byte-range lock held throughout.
func TestDurableOpenSurvivesDisconnect(t *testing.T) {
	backend := newMemBackend()
	srv := newDurableServer(backend, false)
	a := connectLeaseClient(t, srv, 1)
	b := connectLeaseClient(t, srv, 2)
	guid := leaseKey(9)

	fid, timeout := a.openDurable("d.txt", shareAll, leaseKey(1), guid, 0, rh)
	if timeout != uint32(DefaultDurableTimeout/time.Millisecond) {
		t.Fatalf("durable timeout granted = %d ms, want the default", timeout)
	}
	a.writeFile(fid, "before the drop")
	if status := a.lock(fid, 0, 10, wire.LockFlagExclusiveLock|wire.LockFlagFailImmediately); status != wire.StatusSuccess {
		t.Fatalf("lock: %#x", status)
	}

	a.drop()
	waitPreserved(t, srv, fid)
	if status := b.lock(b.mustOpen("d.txt", wire.FileOpen, genericAll, 0), 0, 10, wire.LockFlagExclusiveLock|wire.LockFlagFailImmediately); status != wire.StatusLockNotGranted {
		t.Fatalf("another client locking the range while the owner is away: %#x, want LOCK_NOT_GRANTED", status)
	}

	again := connectLeaseClient(t, srv, 1)
	status, got, state := again.reconnect("d.txt", fid, guid, leaseKey(1), true)
	if status != wire.StatusSuccess || got != fid {
		t.Fatalf("reconnect: %#x, handle %x; want success with the same handle", status, got)
	}
	if state != rh {
		t.Fatalf("lease after reconnect = %#x, want RH", state)
	}
	again.writeFile(fid, "after the drop")
	if status := again.lock(fid, 0, 10, wire.LockFlagUnlock); status != wire.StatusSuccess {
		t.Fatalf("unlocking the lock taken before the drop: %#x", status)
	}
	again.closeFile(fid)
	if got := backend.contents("d.txt"); got != "after the dropp" {
		// Written over "before the drop" from the start.
		t.Fatalf("file holds %q", got)
	}
}

// Only the client that made the open gets it back: the same create GUID,
// client, lease key and user.
func TestDurableReconnectMustMatch(t *testing.T) {
	srv := newDurableServer(newMemBackend(), false)
	a := connectLeaseClient(t, srv, 1)
	guid := leaseKey(9)
	fid, _ := a.openDurable("m.txt", shareAll, leaseKey(1), guid, 0, rh)
	a.drop()
	waitPreserved(t, srv, fid)

	same := connectLeaseClient(t, srv, 1)
	if status, _, _ := same.reconnect("m.txt", fid, leaseKey(8), leaseKey(1), true); status != wire.StatusObjectNameNotFound {
		t.Errorf("wrong create GUID: %#x, want OBJECT_NAME_NOT_FOUND", status)
	}
	if status, _, _ := same.reconnect("m.txt", fid, guid, leaseKey(2), true); status != wire.StatusObjectNameNotFound {
		t.Errorf("wrong lease key: %#x, want OBJECT_NAME_NOT_FOUND", status)
	}
	if status, _, _ := same.reconnect("m.txt", fid, guid, leaseKey(1), false); status != wire.StatusObjectNameNotFound {
		t.Errorf("no lease: %#x, want OBJECT_NAME_NOT_FOUND", status)
	}
	other := connectLeaseClient(t, srv, 3)
	if status, _, _ := other.reconnect("m.txt", fid, guid, leaseKey(1), true); status != wire.StatusObjectNameNotFound {
		t.Errorf("another client: %#x, want OBJECT_NAME_NOT_FOUND", status)
	}
	// Another user: every test session is the same guest, so the open is
	// recorded as someone else's.
	p := srv.durableTable().lookup(fid)
	p.oh.durable.user = `other\someone`
	if status, _, _ := same.reconnect("m.txt", fid, guid, leaseKey(1), true); status != wire.StatusAccessDenied {
		t.Errorf("another user: %#x, want ACCESS_DENIED", status)
	}
	p.oh.durable.user = `\guest`
	if status, got, _ := same.reconnect("m.txt", fid, guid, leaseKey(1), true); status != wire.StatusSuccess || got != fid {
		t.Fatalf("the right client, after the wrong ones: %#x", status)
	}
}

// A client that does not come back in time loses the open, and the file is
// free for others.
func TestDurableOpenExpires(t *testing.T) {
	srv := newDurableServer(newMemBackend(), true)
	a := connectLeaseClient(t, srv, 1)
	b := connectLeaseClient(t, srv, 2)
	guid := leaseKey(9)
	fid, timeout := a.openDurable("x.txt", 0, leaseKey(1), guid, 200, rh)
	if timeout != 200 {
		t.Fatalf("timeout granted = %d ms, want the 200 asked for", timeout)
	}
	a.drop()
	waitPreserved(t, srv, fid)
	time.Sleep(400 * time.Millisecond)

	if _, status := b.open("x.txt", wire.FileOpen, genericAll, 0); status != wire.StatusSuccess {
		t.Fatalf("open after the durable open expired: %#x, want the file free", status)
	}
	again := connectLeaseClient(t, srv, 1)
	if status, _, _ := again.reconnect("x.txt", fid, guid, leaseKey(1), true); status != wire.StatusObjectNameNotFound {
		t.Fatalf("reconnect after expiry: %#x, want OBJECT_NAME_NOT_FOUND", status)
	}
}

// An open that conflicts with a disconnected client's durable open does not
// wait for a break that client cannot acknowledge: the durable open is
// closed and the file is the new opener's.
func TestConflictClosesDisconnectedDurableOpen(t *testing.T) {
	srv := newDurableServer(newMemBackend(), true)
	a := connectLeaseClient(t, srv, 1)
	b := connectLeaseClient(t, srv, 2)
	guid := leaseKey(9)
	fid, _ := a.openDurable("c.txt", 0, leaseKey(1), guid, 0, rh)
	a.drop()
	waitPreserved(t, srv, fid)

	start := time.Now()
	mustWrite(t, b.fc, buildCreateWith(b.sessID, b.treeID, "c.txt", wire.FileOpen, genericAll, 0))
	if rh, _ := b.reply(); rh.Status != wire.StatusSuccess {
		t.Fatalf("conflicting open: %#x, want success once the durable open is closed", rh.Status)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("conflicting open waited %s", waited)
	}
	again := connectLeaseClient(t, srv, 1)
	if status, _, _ := again.reconnect("c.txt", fid, guid, leaseKey(1), true); status != wire.StatusObjectNameNotFound {
		t.Fatalf("reconnect after the open was given away: %#x", status)
	}
}

// Logging off is not a lost connection: durable opens close with it.
func TestLogoffClosesDurableOpens(t *testing.T) {
	srv := newDurableServer(newMemBackend(), false)
	a := connectLeaseClient(t, srv, 1)
	guid := leaseKey(9)
	fid, _ := a.openDurable("l.txt", shareAll, leaseKey(1), guid, 0, rh)
	hdr := wire.NewHeader(wire.CmdLogoff)
	hdr.SessionId = a.sessID
	hdr.Credit = 1
	mustWrite(t, a.fc, append(hdr.Append(nil), 4, 0, 0, 0))
	a.reply()
	a.drop()
	time.Sleep(100 * time.Millisecond)
	if srv.durableTable().lookup(fid) != nil {
		t.Fatal("durable open kept after its session logged off")
	}
}

// An open is made durable only if it caches a handle.
func TestDurableNeedsHandleCaching(t *testing.T) {
	srv := newDurableServer(newMemBackend(), false)
	a := connectLeaseClient(t, srv, 1)
	if _, timeout := a.openDurable("r.txt", shareAll, leaseKey(1), leaseKey(9), 0, wire.LeaseRead); timeout != 0 {
		t.Fatal("an open with only read caching was made durable")
	}
}

// Two clients' byte-range locks conflict. Session ids were numbered per
// connection, so both clients' first opens got the same file id and lock
// owner, and each could lock what the other held.
func TestLocksConflictAcrossConnections(t *testing.T) {
	srv := newLeaseServer(newMemBackend(), 0)
	a := connectLeaseClient(t, srv, 1)
	b := connectLeaseClient(t, srv, 2)
	// Each client's first open, as when two users open one document.
	af := a.mustOpen("shared.txt", wire.FileOverwriteIf, genericAll, 0)
	bf := b.mustOpen("shared.txt", wire.FileOpen, genericAll, 0)
	if af == bf {
		t.Fatal("two clients' opens got the same file id")
	}
	if status := a.lock(af, 0, 10, wire.LockFlagExclusiveLock|wire.LockFlagFailImmediately); status != wire.StatusSuccess {
		t.Fatalf("first lock: %#x", status)
	}
	if status := b.lock(bf, 0, 10, wire.LockFlagExclusiveLock|wire.LockFlagFailImmediately); status != wire.StatusLockNotGranted {
		t.Fatalf("second client's lock on the same range: %#x, want LOCK_NOT_GRANTED", status)
	}
}

// A reconnect is looked up by the persistent half of the file id alone:
// the volatile half belonged to the lost connection.
func TestDurableReconnectByPersistentID(t *testing.T) {
	srv := newDurableServer(newMemBackend(), false)
	a := connectLeaseClient(t, srv, 1)
	guid := leaseKey(9)
	fid, _ := a.openDurable("p.txt", shareAll, leaseKey(1), guid, 0, rh)
	a.drop()
	waitPreserved(t, srv, fid)

	named := fid
	for i := 8; i < 16; i++ {
		named[i] = 0
	}
	again := connectLeaseClient(t, srv, 1)
	if status, got, _ := again.reconnect("p.txt", named, guid, leaseKey(1), true); status != wire.StatusSuccess || got != fid {
		t.Fatalf("reconnect naming the persistent id: %#x, handle %x; want success with the open's handle", status, got)
	}
}
