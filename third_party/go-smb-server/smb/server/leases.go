package server

import (
	"encoding/binary"
	"strings"
	"sync"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// Leases let a client cache a file (MS-SMB2 section 3.3.1.4). The server
// grants read caching (R), and handle caching (H) with it, never write
// caching: writes always reach the server, so a client never holds data
// the server has not seen. Clients asking for an oplock rather than a lease
// get a level II oplock, which is read caching too.
//
// A lease is broken, and the client told to drop what it cached, when the
// file changes through anyone else: another SMB client, NFS, or the bucket
// directly, for backends that report such changes (vfs.ChangeNotifier with
// Change.External set). Breaking read caching needs no acknowledgement,
// since there is nothing dirty to write back, so a change is never held up
// waiting for a client. Breaking handle caching does: a client caching a
// handle keeps the file open after the application closes it, so an open
// that would conflict with it waits for the client to let go first.

// DefaultLeaseBreakTimeout is how long a break that needs acknowledging is
// waited for before the server takes it as acknowledged, as Windows does.
const DefaultLeaseBreakTimeout = 35 * time.Second

// leaseID names a lease: the client's GUID and the key it chose.
type leaseID struct {
	client [16]byte
	key    [16]byte
}

type lease struct {
	id   leaseID
	file fileKey
	// oplock is set for a level II oplock, which has no key and belongs to
	// one open.
	oplock bool
	v2     bool
	parent [16]byte
	flags  uint32 // parent-key flag, echoed back

	state uint32
	epoch uint16

	// A break that needs acknowledging: breakTo is the state asked for,
	// acked closes when it is acknowledged or times out, and further lists
	// a lower state wanted meanwhile, sent once this break completes.
	breaking   bool
	breakTo    uint32
	acked      chan struct{}
	timer      *time.Timer
	further    uint32
	hasFurther bool

	opens map[*openHandle]struct{}
	// conn is where breaks are sent: the connection that last opened the
	// file with this lease.
	conn *conn
	// For an oplock, the open it is on.
	sessID uint64
	treeID uint32
	fileID [16]byte
}

type leaseTable struct {
	obs     LeaseObserver
	mu      sync.Mutex
	byID    map[leaseID]*lease
	byFile  map[fileKey]map[*lease]struct{}
	timeout time.Duration
	// feeds are the backend subscriptions that carry changes made outside
	// the server, one per share.
	feeds map[string]func()
}

func (s *Server) leaseTable() *leaseTable {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases == nil {
		timeout := s.leaseBreakTimeout
		if timeout <= 0 {
			timeout = DefaultLeaseBreakTimeout
		}
		s.leases = &leaseTable{
			obs:     s.leaseObs(),
			byID:    map[leaseID]*lease{},
			byFile:  map[fileKey]map[*lease]struct{}{},
			timeout: timeout,
			feeds:   map[string]func(){},
		}
	}
	return s.leases
}

// WithLeases lets clients cache files with read and read-handle leases, and
// level II oplocks. breakTimeout bounds the wait for a client to acknowledge
// a break; 0 selects DefaultLeaseBreakTimeout.
func WithLeases(breakTimeout time.Duration) Option {
	return func(s *Server) {
		s.leasesEnabled = true
		s.leaseBreakTimeout = breakTimeout
	}
}

// leaseConflict reports whether a lease key the client already uses names a
// different file, which the client may not do (MS-SMB2 section 3.3.5.9.8).
func (t *leaseTable) leaseConflict(id leaseID, file fileKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.byID[id]
	return ok && l.file != file
}

// grantCaching decides what caching a new open gets, and returns the
// oplock level and create context for the CREATE response.
func (c *request) grantCaching(req *wire.CreateRequest, sess *session, tr *tree, oh *openHandle, fi vfs.FileInfo) (uint8, []wire.CreateContext) {
	if !c.srv.leasesEnabled || fi.IsDir || oh.stream || sess == nil || sess.requireEncrypt {
		// Break notifications are sent outside any session, so they could
		// not be encrypted.
		return wire.OplockLevelNone, nil
	}
	t := c.srv.leaseTable()
	t.watchShare(c.srv, tr.share)
	key := c.srv.fileTable().keyOf(oh)

	switch req.RequestedOplockLevel {
	case wire.OplockLevelLease:
		data, ok := req.Context(wire.CreateContextLease)
		if !ok {
			return wire.OplockLevelNone, nil
		}
		asked, err := wire.ParseLeaseRequest(data)
		if err != nil {
			return wire.OplockLevelNone, nil
		}
		granted, flags, epoch := t.grantLease(c.conn, oh, key, asked)
		if t.obs != nil && granted != 0 {
			t.obs.LeaseGranted("lease", granted)
		}
		resp := wire.LeaseRequest{Key: asked.Key, State: granted, Flags: flags, V2: asked.V2,
			ParentKey: asked.ParentKey, Epoch: epoch}
		return wire.OplockLevelLease, []wire.CreateContext{{Name: wire.CreateContextLease, Data: resp.Encode()}}
	case 0x01, 0x08, 0x09: // level II, exclusive, batch
		t.grantOplock(c.conn, oh, key)
		if t.obs != nil {
			t.obs.LeaseGranted("oplock", wire.LeaseRead)
		}
		return wire.OplockLevelII, nil
	}
	return wire.OplockLevelNone, nil
}

// grantLease adds the open to its lease, creating or upgrading it. Only
// read and read-handle caching are granted; handle caching alone is not a
// valid state, so a request without read caching gets none.
func (t *leaseTable) grantLease(c *conn, oh *openHandle, file fileKey, asked wire.LeaseRequest) (state, flags uint32, epoch uint16) {
	want := asked.State & (wire.LeaseRead | wire.LeaseHandle)
	if want&wire.LeaseRead == 0 {
		want = 0
	}
	id := leaseID{client: c.clientGUID, key: asked.Key}

	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.byID[id]
	if !ok {
		l = &lease{id: id, file: file, v2: asked.V2, parent: asked.ParentKey, opens: map[*openHandle]struct{}{}}
		if asked.Flags&wire.LeaseFlagParentKeySet != 0 {
			l.flags = wire.LeaseFlagParentKeySet
		}
		t.byID[id] = l
		t.addToFile(l)
	}
	switch {
	case l.breaking:
		// Nothing more while a break is under way; the client is told so.
		flags |= wire.LeaseFlagBreakInProgress
	case want&^l.state != 0:
		l.state |= want
		l.epoch++
	}
	l.opens[oh] = struct{}{}
	l.conn = c
	oh.lease = l
	return l.state, flags | l.flags, l.epoch
}

// grantOplock gives an open a level II oplock: read caching for that open.
func (t *leaseTable) grantOplock(c *conn, oh *openHandle, file fileKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := &lease{oplock: true, file: file, state: wire.LeaseRead, opens: map[*openHandle]struct{}{oh: {}},
		conn: c, sessID: oh.sessionID, fileID: oh.fileId}
	t.addToFile(l)
	oh.lease = l
}

func (t *leaseTable) addToFile(l *lease) {
	set, ok := t.byFile[l.file]
	if !ok {
		set = map[*lease]struct{}{}
		t.byFile[l.file] = set
	}
	set[l] = struct{}{}
}

func (t *leaseTable) removeFromFile(l *lease) {
	set := t.byFile[l.file]
	delete(set, l)
	if len(set) == 0 {
		delete(t.byFile, l.file)
	}
}

// release drops a closing open from its lease, and the lease with its last
// open.
func (t *leaseTable) release(oh *openHandle) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := oh.lease
	if l == nil {
		return
	}
	oh.lease = nil
	delete(l.opens, oh)
	if len(l.opens) > 0 {
		return
	}
	t.removeFromFile(l)
	if !l.oplock {
		delete(t.byID, l.id)
	}
	if l.breaking {
		t.finishLocked(l, l.breakTo)
	}
}

// renamed moves the leases on a file with it.
func (t *leaseTable) renamed(from, to fileKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	set, ok := t.byFile[from]
	if !ok {
		return
	}
	delete(t.byFile, from)
	for l := range set {
		l.file = to
		t.addToFile(l)
	}
}

// breakRead breaks read caching on a file, for every lease but the one of
// the open making the change: the file's data changed, so what clients
// cached is stale. Nothing waits for acknowledgements.
func (t *leaseTable) breakRead(file fileKey, except *openHandle) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for l := range t.byFile[file] {
		if _, own := l.opens[except]; !own || except == nil {
			t.breakLocked(l, 0)
		}
	}
}

// breakHandle breaks handle caching on a file for every lease but the
// requester's, and returns what to wait on: clients caching a handle close
// it when told, which may clear a conflict with the open that asked.
func (t *leaseTable) breakHandle(file fileKey, except leaseID) []chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	var waits []chan struct{}
	for l := range t.byFile[file] {
		if l.oplock || l.id == except || l.state&wire.LeaseHandle == 0 {
			continue
		}
		if ch := t.breakLocked(l, l.state&^wire.LeaseHandle); ch != nil {
			waits = append(waits, ch)
		}
	}
	return waits
}

// holdsHandle reports whether anyone but the requester caches a handle to
// the file.
func (t *leaseTable) holdsHandle(file fileKey, except leaseID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for l := range t.byFile[file] {
		if !l.oplock && l.id != except && l.state&wire.LeaseHandle != 0 {
			return true
		}
	}
	return false
}

// breakLocked asks a lease down to target, returning a channel to wait on
// if the break needs acknowledging.
func (t *leaseTable) breakLocked(l *lease, target uint32) chan struct{} {
	if l.breaking {
		if l.breakTo&^target != 0 {
			// Lower than the break under way: sent once it completes.
			if l.hasFurther {
				l.further &= target
			} else {
				l.further, l.hasFurther = target, true
			}
		}
		return l.acked
	}
	if l.state&^target == 0 {
		return nil
	}
	newState := l.state & target
	if t.obs != nil {
		kind := "lease"
		if l.oplock {
			kind = "oplock"
		}
		t.obs.LeaseBroken(kind, l.state, newState)
	}
	if l.oplock {
		l.state = newState
		l.conn.sendNotification(oplockBreakNotification(l))
		return nil
	}
	l.epoch++
	ackRequired := l.state&(wire.LeaseHandle|wire.LeaseWrite) != 0
	l.conn.sendNotification(leaseBreakNotification(l, newState, ackRequired))
	if !ackRequired {
		l.state = newState
		return nil
	}
	l.breaking, l.breakTo = true, newState
	l.acked = make(chan struct{})
	l.timer = time.AfterFunc(t.timeout, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if l.breaking {
			if t.obs != nil {
				t.obs.LeaseBreakTimedOut()
			}
			t.finishLocked(l, l.breakTo)
		}
	})
	return l.acked
}

// finishLocked completes a break at state, and sends any lower break wanted
// meanwhile.
func (t *leaseTable) finishLocked(l *lease, state uint32) {
	l.state = state
	l.breaking = false
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	close(l.acked)
	if l.hasFurther {
		further := l.further
		l.hasFurther = false
		if len(l.opens) > 0 {
			t.breakLocked(l, further)
		}
	}
}

// acknowledge handles a client's lease break acknowledgement.
func (t *leaseTable) acknowledge(id leaseID, state uint32) (*lease, uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.byID[id]
	if !ok {
		return nil, wire.StatusObjectNameNotFound
	}
	if !l.breaking {
		return nil, statusUnsuccessful
	}
	if state&^l.breakTo != 0 {
		// Acknowledging more than it was left with: the break stands.
		t.finishLocked(l, l.breakTo)
		return nil, statusRequestNotAccepted
	}
	t.finishLocked(l, state)
	return l, wire.StatusSuccess
}

const (
	statusUnsuccessful       uint32 = 0xC0000001
	statusRequestNotAccepted uint32 = 0xC00000D0
)

// notificationHeader is the header of an unsolicited OPLOCK_BREAK: it
// answers no request, so its MessageId is all ones (MS-SMB2 section
// 3.3.4.6).
func notificationHeader(sessID uint64, treeID uint32) []byte {
	hdr := wire.NewHeader(wire.CmdOplockBreak)
	hdr.Flags = wire.FlagServerToRedir
	hdr.MessageId = ^uint64(0)
	hdr.SessionId = sessID
	hdr.TreeId = treeID
	return hdr.Append(nil)
}

// leaseBreakNotification is SMB2_LEASE_BREAK_NOTIFICATION (section
// 2.2.23.2).
func leaseBreakNotification(l *lease, newState uint32, ackRequired bool) []byte {
	b := make([]byte, 44)
	binary.LittleEndian.PutUint16(b[0:2], 44)
	if l.v2 {
		binary.LittleEndian.PutUint16(b[2:4], l.epoch)
	}
	if ackRequired {
		binary.LittleEndian.PutUint32(b[4:8], 0x01) // SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED
	}
	copy(b[8:24], l.id.key[:])
	binary.LittleEndian.PutUint32(b[24:28], l.state)
	binary.LittleEndian.PutUint32(b[28:32], newState)
	return append(notificationHeader(0, 0), b...)
}

// oplockBreakNotification is SMB2_OPLOCK_BREAK_NOTIFICATION (section
// 2.2.23.1), breaking a level II oplock to none, which is not acknowledged.
func oplockBreakNotification(l *lease) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint16(b[0:2], 24)
	b[2] = wire.OplockLevelNone
	copy(b[8:24], l.fileID[:])
	return append(notificationHeader(l.sessID, 0), b...)
}

// sendNotification queues an unsolicited message without blocking the
// caller, which may be making a change on another goroutine and holding
// locks: straight onto the connection's queue when there is room, so the
// break goes out ahead of the change's own reply, and from a goroutine when
// not.
func (c *conn) sendNotification(msg []byte) {
	c.queued.Add(1)
	select {
	case c.outbox <- msg:
		return
	case <-c.connDone:
		c.queued.Add(-1)
		return
	default:
	}
	go func() {
		select {
		case c.outbox <- msg:
		case <-c.connDone:
			c.queued.Add(-1)
		}
	}()
}

// handleOplockBreak answers a client's acknowledgement of a lease break
// (36 bytes) or an oplock break (24 bytes).
func (c *request) handleOplockBreak(msg []byte) uint32 {
	if len(msg) < wire.HeaderSize+24 {
		return c.errBody(wire.StatusInvalidParameter)
	}
	body := msg[wire.HeaderSize:]
	switch binary.LittleEndian.Uint16(body[0:2]) {
	case 36:
		if len(body) < 36 {
			return c.errBody(wire.StatusInvalidParameter)
		}
		var key [16]byte
		copy(key[:], body[8:24])
		state := binary.LittleEndian.Uint32(body[24:28])
		l, status := c.srv.leaseTable().acknowledge(leaseID{client: c.clientGUID, key: key}, state)
		if l == nil {
			return c.errBody(status)
		}
		resp := make([]byte, 36)
		binary.LittleEndian.PutUint16(resp[0:2], 36)
		copy(resp[8:24], key[:])
		binary.LittleEndian.PutUint32(resp[24:28], state)
		c.out = append(c.out, resp...)
		return wire.StatusSuccess
	case 24:
		// Level II oplocks break to none without an acknowledgement, so
		// there is nothing to acknowledge.
		return c.errBody(statusInvalidOplockProtocol)
	}
	return c.errBody(wire.StatusInvalidParameter)
}

const statusInvalidOplockProtocol uint32 = 0xC00000E3

// watchShare subscribes, once per share, to changes its backend reports
// from outside the server, which break the leases on the files they touch.
func (t *leaseTable) watchShare(s *Server, share vfs.Share) {
	notifier, ok := share.Backend().(vfs.ChangeNotifier)
	if !ok {
		return
	}
	name := strings.ToLower(share.Name())
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.feeds[name]; ok {
		return
	}
	t.feeds[name] = notifier.NotifyChanges(func(ch vfs.Change) {
		if !ch.External {
			// Changes made through this server break leases as they are
			// made, sparing the lease of the open that made them.
			return
		}
		switch ch.Action {
		case vfs.ChangeModified:
			if ch.Filter&(FileNotifyChangeSize|FileNotifyChangeLastWrite) == 0 {
				return
			}
		case vfs.ChangeAdded:
			return
		case vfs.ChangeRenamed:
			t.breakRead(fileKey{share: name, path: normalizeOpenPath(ch.OldPath)}, nil)
		}
		t.breakRead(fileKey{share: name, path: normalizeOpenPath(ch.Path)}, nil)
	})
}

// dataChanged breaks the read caching others have on the file an open just
// changed.
func (s *Server) dataChanged(oh *openHandle) {
	if !s.leasesEnabled {
		return
	}
	s.leaseTable().breakRead(s.fileTable().keyOf(oh), oh)
}
