package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// Durable handles let a client's open files survive its connection
// dropping (MS-SMB2 section 3.3.5.9.6 onwards). When a connection is lost
// rather than closed, its durable opens are kept, with their locks, lease
// and share modes, for the timeout agreed when they were opened. A client
// that reconnects in time names each open (DH2C or DHnC) and carries on with
// it; one that does not has its opens closed when the timeout runs out.
//
// An open is made durable only when it also caches a handle (a lease with
// handle caching): durable opens exist so a client can go on caching
// through a reconnect. Handles are not persistent: they do not survive the
// server restarting or failing over.

// Durable handle timeouts: DefaultDurableTimeout when the client leaves it to
// the server, and never more than MaxDurableTimeout.
const (
	DefaultDurableTimeout = 60 * time.Second
	MaxDurableTimeout     = 300 * time.Second
)

// WithDurableHandles makes opens that cache a handle durable. Durable
// handles need leases (WithLeases).
func WithDurableHandles() Option {
	return func(s *Server) { s.durableEnabled = true }
}

// durableInfo is what an open was made durable with: what its reconnect
// must match, and how long it is kept.
type durableInfo struct {
	v2         bool
	createGUID [16]byte
	clientGUID [16]byte
	user       string
	share      string
	timeout    time.Duration
}

// preservedOpen is a durable open whose connection was lost.
type preservedOpen struct {
	oh    *openHandle
	share vfs.Share
	timer *time.Timer
}

// durableTable holds preserved opens by the persistent half of their file
// ids, which is all a reconnect is looked up by (section 3.3.5.9.12).
type durableTable struct {
	mu   sync.Mutex
	byID map[uint64]*preservedOpen
}

func (s *Server) durableTable() *durableTable {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.durable == nil {
		s.durable = &durableTable{byID: map[uint64]*preservedOpen{}}
	}
	return s.durable
}

// sessionUser names a session's user, which a reconnect must match.
func sessionUser(sess *session) string {
	if sess == nil || sess.identity == nil {
		return ""
	}
	return strings.ToLower(sess.identity.Domain + `\` + sess.identity.Username)
}

// grantDurable makes an open durable if it asked and caches a handle,
// returning the create context to answer with.
func (c *request) grantDurable(req *wire.CreateRequest, sess *session, tr *tree, oh *openHandle, leaseState uint32) []wire.CreateContext {
	if !c.srv.durableEnabled || leaseState&wire.LeaseHandle == 0 {
		return nil
	}
	info := &durableInfo{clientGUID: c.clientGUID, user: sessionUser(sess),
		share: strings.ToLower(tr.share.Name()), timeout: DefaultDurableTimeout}
	var ctx wire.CreateContext
	if data, ok := req.Context(wire.CreateContextDurableV2); ok {
		asked, err := wire.ParseDurableV2Request(data)
		if err != nil {
			return nil
		}
		// A persistent handle is not offered: the open is durable only,
		// which a client asking for persistence accepts (section
		// 3.3.5.9.10).
		info.v2, info.createGUID = true, asked.CreateGUID
		if asked.Timeout != 0 {
			info.timeout = min(time.Duration(asked.Timeout)*time.Millisecond, MaxDurableTimeout)
		}
		ctx = wire.CreateContext{Name: wire.CreateContextDurableV2, Data: wire.DurableV2Response(info.timeout)}
	} else if _, ok := req.Context(wire.CreateContextDurableV1); ok {
		ctx = wire.CreateContext{Name: wire.CreateContextDurableV1, Data: wire.DurableV1Response()}
	} else {
		return nil
	}
	oh.durable = info
	return []wire.CreateContext{ctx}
}

// preserve keeps a durable open whose connection was lost, for its timeout.
func (s *Server) preserve(share vfs.Share, oh *openHandle) {
	t := s.durableTable()
	p := &preservedOpen{oh: oh, share: share}
	t.mu.Lock()
	t.byID[persistentID(oh.fileId)] = p
	t.mu.Unlock()
	p.timer = time.AfterFunc(oh.durable.timeout, func() {
		if t.take(oh.fileId) != nil {
			s.log.Debug("durable open not reclaimed in time", "path", oh.currentPath())
			s.closePreserved(p)
		}
	})
	s.log.Debug("keeping durable open for its client to reconnect", "path", oh.currentPath(), "timeout", oh.durable.timeout)
}

// take removes a preserved open, or returns nil if there is none.
func (t *durableTable) take(fid [16]byte) *preservedOpen {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := persistentID(fid)
	p, ok := t.byID[id]
	if !ok {
		return nil
	}
	delete(t.byID, id)
	if p.timer != nil {
		p.timer.Stop()
	}
	return p
}

func (t *durableTable) lookup(fid [16]byte) *preservedOpen {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byID[persistentID(fid)]
}

// closePreserved closes a preserved open for good: its locks, lease and
// share modes go, and a delete-on-close takes effect.
func (s *Server) closePreserved(p *preservedOpen) {
	ctx := context.Background()
	s.lockTable().ReleaseOwner(lockOwner(p.oh.sessionID, p.oh.fileId))
	s.leaseTable().release(p.oh)
	_ = p.oh.h.Close(ctx)
	if err := s.releaseOpen(ctx, p.share, p.oh); err != nil {
		s.log.Debug("delete on close of an expired durable open failed", "path", p.oh.currentPath(), "err", err)
	}
}

// closeAllPreserved closes every preserved open, as the server stops.
func (s *Server) closeAllPreserved() {
	t := s.durableTable()
	t.mu.Lock()
	all := make([]*preservedOpen, 0, len(t.byID))
	for id, p := range t.byID {
		if p.timer != nil {
			p.timer.Stop()
		}
		all = append(all, p)
		delete(t.byID, id)
	}
	t.mu.Unlock()
	for _, p := range all {
		s.closePreserved(p)
	}
}

// closePreservedOpens closes the preserved opens among a lease's opens, for
// a break its disconnected client cannot acknowledge: whoever needs the
// lease broken gets the file instead (section 3.3.4.7). The returned channel
// closes once they are closed.
func (s *Server) closePreservedOpens(opens []*openHandle) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := s.durableTable()
		for _, oh := range opens {
			if p := t.take(oh.fileId); p != nil {
				s.log.Debug("closing durable open of a disconnected client for a lease break", "path", oh.currentPath())
				s.closePreserved(p)
			}
		}
	}()
	return done
}

// durableReconnect returns the reconnect a CREATE asks for, if any.
func durableReconnect(req *wire.CreateRequest) (wire.DurableReconnect, bool) {
	if data, ok := req.Context(wire.CreateContextDurableV2Reconnect); ok {
		if rc, err := wire.ParseDurableV2Reconnect(data); err == nil {
			return rc, true
		}
	}
	if data, ok := req.Context(wire.CreateContextDurableV1Reconnect); ok {
		if rc, err := wire.ParseDurableV1Reconnect(data); err == nil {
			return rc, true
		}
	}
	return wire.DurableReconnect{}, false
}

// reconnectDurable answers a CREATE that reclaims a durable open after its
// client's connection was lost (section 3.3.5.9.7 and 3.3.5.9.12). The open
// is only handed to the same user, on the same share, from the same client,
// naming the same create and lease; anything else is told it does not exist.
func (c *request) reconnectDurable(ctx context.Context, req *wire.CreateRequest, rc wire.DurableReconnect, sess *session, tr *tree, lastFileId *[16]byte) uint32 {
	c.log.Debug("durable reconnect", "v2", rc.V2, "contexts", contextNames(req))
	if !c.srv.durableEnabled {
		return c.errBody(wire.StatusObjectNameNotFound)
	}
	t := c.srv.durableTable()
	p := t.lookup(rc.FileID)
	if p == nil {
		c.log.Debug("durable reconnect: no such open", "v2", rc.V2)
		return c.errBody(wire.StatusObjectNameNotFound)
	}
	info := p.oh.durable
	refuse := func(why string, status uint32) uint32 {
		c.log.Debug("durable reconnect refused", "path", p.oh.currentPath(), "reason", why)
		return c.errBody(status)
	}
	switch {
	case info.v2 != rc.V2:
		return refuse("durable handle version differs", wire.StatusObjectNameNotFound)
	case rc.V2 && info.createGUID != rc.CreateGUID:
		return refuse("create GUID differs", wire.StatusObjectNameNotFound)
	case rc.V2 && info.clientGUID != c.clientGUID:
		return refuse("client GUID differs", wire.StatusObjectNameNotFound)
	case info.share != strings.ToLower(tr.share.Name()):
		return refuse("share differs", wire.StatusObjectNameNotFound)
	case info.user != sessionUser(sess):
		c.log.Debug("durable reconnect by another user", "owner", info.user, "user", sessionUser(sess))
		return refuse("user differs", wire.StatusAccessDenied)
	}
	leases := c.srv.leaseTable()
	if !leases.reconnectMatches(p.oh, c.clientGUID, req) {
		return refuse("lease differs", wire.StatusObjectNameNotFound)
	}
	if t.take(rc.FileID) == nil {
		// Reclaimed or expired meanwhile.
		return c.errBody(wire.StatusObjectNameNotFound)
	}

	oh := p.oh
	fi, err := oh.h.Stat(ctx)
	if err != nil {
		c.srv.closePreserved(p)
		return c.errBody(osErrToStatus(err))
	}
	tr.addOpen(oh)
	*lastFileId = oh.fileId
	oplock, contexts := leases.reattach(oh, c.conn)
	c.log.Debug("durable open reclaimed", "path", oh.currentPath())

	resp := wire.CreateResponse{
		Contexts:       contexts,
		OplockLevel:    oplock,
		CreateAction:   wire.FileOpened,
		CreationTime:   wire.TimeToFiletime(fi.CreationTime),
		LastAccessTime: wire.TimeToFiletime(fi.LastAccess),
		LastWriteTime:  wire.TimeToFiletime(fi.LastWrite),
		ChangeTime:     wire.TimeToFiletime(fi.ChangeTime),
		AllocationSize: uint64(fi.Size),
		EndOfFile:      uint64(fi.Size),
		FileAttributes: toFileAttributes(fi),
		FileId:         oh.fileId,
	}
	c.out = resp.Append(c.out)
	return wire.StatusSuccess
}
