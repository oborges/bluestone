package server

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"sync"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

const (
	FileNotifyChangeFileName    uint32 = 0x00000001
	FileNotifyChangeDirName     uint32 = 0x00000002
	FileNotifyChangeAttributes  uint32 = 0x00000004
	FileNotifyChangeSize        uint32 = 0x00000008
	FileNotifyChangeLastWrite   uint32 = 0x00000010
	FileNotifyChangeLastAccess  uint32 = 0x00000020
	FileNotifyChangeCreation    uint32 = 0x00000040
	FileNotifyChangeEA          uint32 = 0x00000080
	FileNotifyChangeSecurity    uint32 = 0x00000100
	FileNotifyChangeStreamName  uint32 = 0x00000200
	FileNotifyChangeStreamSize  uint32 = 0x00000400
	FileNotifyChangeStreamWrite uint32 = 0x00000800
	SMB2WatchTree               uint16 = 0x0001
)

// FILE_NOTIFY_INFORMATION actions (MS-FSCC section 2.7.1).
const (
	fileActionAdded          uint32 = 0x00000001
	fileActionRemoved        uint32 = 0x00000002
	fileActionModified       uint32 = 0x00000003
	fileActionRenamedOldName uint32 = 0x00000004
	fileActionRenamedNewName uint32 = 0x00000005
)

// Statuses a CHANGE_NOTIFY completes with besides success and cancellation
// (MS-SMB2 section 3.3.5.19).
const (
	// statusNotifyCleanup: the handle the watch was on has closed.
	statusNotifyCleanup uint32 = 0x0000010B
	// statusNotifyEnumDir: more changed than the client's buffer holds, so
	// it re-reads the directory instead.
	statusNotifyEnumDir uint32 = 0x0000010C
)

// maxNotifyBuffer bounds the changes a watch holds between requests.
const maxNotifyBuffer = 64 << 10

type changeNotifyRequest struct {
	Flags              uint16
	OutputBufferLength uint32
	FileId             [16]byte
	CompletionFilter   uint32
}

func (r *changeNotifyRequest) Parse(msg []byte) error {
	if len(msg) < 64+32 {
		return errors.New("wire: change_notify needs 32 bytes")
	}
	b := msg[64:]
	r.Flags = le16(b[2:4])
	r.OutputBufferLength = le32(b[4:8])
	copy(r.FileId[:], b[8:24])
	r.CompletionFilter = le32(b[24:28])
	return nil
}

type notifyEvent struct {
	action uint32
	name   string
}

// A watch is the change notification on one open directory. It is set up by
// the handle's first CHANGE_NOTIFY and lasts until the handle closes, so
// changes that happen between one request completing and the client sending
// the next are kept for it rather than lost (MS-SMB2 section 3.3.5.19).
// Changes arrive as events, from the backend or from the server's own
// handlers, and are routed to the watches that can see them: a watch lists
// nothing, and costs nothing while nothing changes.
type watch struct {
	share    string // lower-cased share name
	dir      string // normalized directory path, "" for the share root
	depth    int    // components in dir
	tree     bool   // SMB2_WATCH_TREE: the whole subtree
	filter   uint32
	maxBytes uint32

	mu       sync.Mutex
	events   []notifyEvent
	size     uint32 // bytes events take in a response
	overflow bool
	waiting  []*notifyWait
	closed   bool
}

// notifyWait is a CHANGE_NOTIFY request waiting for a change.
type notifyWait struct {
	c    *conn
	op   *pendingOp
	hdr  wire.Header
	done chan struct{}
}

// notifyHub routes changes to watches, by share.
type notifyHub struct {
	mu      sync.Mutex
	watches map[string]map[*watch]struct{}
	// feeds are the backend subscriptions, one per share however many
	// watches it has.
	feeds map[string]func()
}

func (s *Server) notifyHub() *notifyHub {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notify == nil {
		s.notify = &notifyHub{watches: map[string]map[*watch]struct{}{}, feeds: map[string]func(){}}
	}
	return s.notify
}

// add registers a watch, subscribing to the share's backend on its first.
func (h *notifyHub) add(share vfs.Share, w *watch) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.watches[w.share]
	if !ok {
		set = map[*watch]struct{}{}
		h.watches[w.share] = set
	}
	set[w] = struct{}{}
	if _, subscribed := h.feeds[w.share]; subscribed {
		return
	}
	if notifier, ok := share.Backend().(vfs.ChangeNotifier); ok {
		name := w.share
		h.feeds[w.share] = notifier.NotifyChanges(func(c vfs.Change) { h.publish(name, c) })
	}
}

// remove forgets a watch, and the backend subscription with the share's
// last one.
func (h *notifyHub) remove(w *watch) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.watches[w.share]
	delete(set, w)
	if len(set) > 0 {
		return
	}
	delete(h.watches, w.share)
	if stop, ok := h.feeds[w.share]; ok {
		stop()
		delete(h.feeds, w.share)
	}
}

// watching reports whether anything watches the share, so the server can
// skip describing changes nobody will see.
func (h *notifyHub) watching(share string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.watches[strings.ToLower(share)]) > 0
}

// publish routes a change on a share to its watches.
func (h *notifyHub) publish(share string, c vfs.Change) {
	h.mu.Lock()
	watches := make([]*watch, 0, len(h.watches[share]))
	for w := range h.watches[share] {
		watches = append(watches, w)
	}
	h.mu.Unlock()
	for _, w := range watches {
		w.offer(c)
	}
}

// selfNotify reports a change made through the server, for backends that
// do not report changes themselves. A backend that does sees these changes
// too, and reports them.
func (s *Server) selfNotify(tr *tree, c vfs.Change) {
	s.selfNotifyShare(tr.share, c)
}

func (s *Server) selfNotifyShare(share vfs.Share, c vfs.Change) {
	if _, ok := share.Backend().(vfs.ChangeNotifier); ok {
		return
	}
	hub := s.notifyHub()
	if !hub.watching(share.Name()) {
		return
	}
	hub.publish(strings.ToLower(share.Name()), c)
}

// wantsSelfNotify reports whether the server should describe its own
// changes on the tree's share: the backend does not, and someone watches.
func (s *Server) wantsSelfNotify(tr *tree) bool {
	if _, ok := tr.share.Backend().(vfs.ChangeNotifier); ok {
		return false
	}
	return s.notifyHub().watching(tr.share.Name())
}

// relative returns p's name relative to the watched directory, and whether
// the watch sees it: a direct child, or anywhere below for a tree watch.
// The directory itself is not reported; its parent's watches see it.
func (w *watch) relative(p string) (string, bool) {
	p = strings.Trim(strings.ReplaceAll(p, "/", `\`), `\`)
	parts := strings.Split(p, `\`)
	if p == "" || len(parts) <= w.depth {
		return "", false
	}
	if w.depth > 0 && strings.ToLower(strings.Join(parts[:w.depth], `\`)) != w.dir {
		return "", false
	}
	rest := parts[w.depth:]
	if len(rest) > 1 && !w.tree {
		return "", false
	}
	return strings.Join(rest, `\`), true
}

// nameFilter is the filter bit an addition, removal or rename matches.
func nameFilter(isDir bool) uint32 {
	if isDir {
		return FileNotifyChangeDirName
	}
	return FileNotifyChangeFileName
}

// offer turns a change into the events this watch reports, if any.
func (w *watch) offer(c vfs.Change) {
	var events []notifyEvent
	switch c.Action {
	case vfs.ChangeAdded, vfs.ChangeRemoved:
		if name, ok := w.relative(c.Path); ok && w.filter&nameFilter(c.IsDir) != 0 {
			events = append(events, notifyEvent{uint32(c.Action), name})
		}
	case vfs.ChangeModified:
		if name, ok := w.relative(c.Path); ok && w.filter&c.Filter != 0 {
			events = append(events, notifyEvent{fileActionModified, name})
		}
	case vfs.ChangeRenamed:
		if w.filter&nameFilter(c.IsDir) == 0 {
			return
		}
		oldName, sawOld := w.relative(c.OldPath)
		newName, sawNew := w.relative(c.Path)
		switch {
		case sawOld && sawNew:
			events = append(events, notifyEvent{fileActionRenamedOldName, oldName},
				notifyEvent{fileActionRenamedNewName, newName})
		case sawOld:
			// Moved out of what this watch sees.
			events = append(events, notifyEvent{fileActionRemoved, oldName})
		case sawNew:
			events = append(events, notifyEvent{fileActionAdded, newName})
		}
	}
	if len(events) > 0 {
		w.add(events)
	}
}

// entrySize is the space an event takes in FILE_NOTIFY_INFORMATION,
// 4-byte aligned.
func entrySize(e notifyEvent) uint32 {
	return (12 + uint32(len(wire.UTF16ToBytes(e.name))) + 3) &^ 3
}

// add buffers events and completes a waiting request with them.
func (w *watch) add(events []notifyEvent) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	for _, e := range events {
		if w.overflow {
			break
		}
		// A file written in several pieces is one change to a client
		// that has not asked again yet.
		if n := len(w.events); n > 0 && w.events[n-1] == e {
			continue
		}
		size := entrySize(e)
		if w.size+size > w.maxBytes {
			// More changed than the client can be told about: it
			// re-reads the directory instead.
			w.overflow = true
			w.events, w.size = nil, 0
			break
		}
		w.events = append(w.events, e)
		w.size += size
	}
	wait, status, body := w.takeLocked()
	w.mu.Unlock()
	if wait != nil {
		// Delivered off the goroutine that made the change, which may be
		// holding a file's lock and must not wait on a connection.
		go wait.finish(status, body)
	}
}

// takeLocked pairs buffered changes with the oldest waiting request.
func (w *watch) takeLocked() (*notifyWait, uint32, []byte) {
	if len(w.waiting) == 0 || (len(w.events) == 0 && !w.overflow) {
		return nil, 0, nil
	}
	wait := w.waiting[0]
	w.waiting = w.waiting[1:]
	status, body := wire.StatusSuccess, changeNotifyBody(w.events)
	if w.overflow {
		status, body = statusNotifyEnumDir, changeNotifyBody(nil)
	}
	w.events, w.size, w.overflow = nil, 0, false
	return wait, status, body
}

// wait queues a request for the next change, answering it at once if
// changes are already buffered.
func (w *watch) wait(wait *notifyWait) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		go wait.finish(statusNotifyCleanup, changeNotifyBody(nil))
		return
	}
	w.waiting = append(w.waiting, wait)
	ready, status, body := w.takeLocked()
	w.mu.Unlock()
	if ready != nil {
		go ready.finish(status, body)
	}
	go func() {
		select {
		case <-wait.op.done:
			// CANCEL, or the connection ending.
			if w.forget(wait) {
				wait.finish(wire.StatusCancelled, changeNotifyBody(nil))
			}
		case <-wait.done:
		}
	}()
}

// forget removes a waiting request, reporting whether it was still waiting.
func (w *watch) forget(wait *notifyWait) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, queued := range w.waiting {
		if queued == wait {
			w.waiting = append(w.waiting[:i], w.waiting[i+1:]...)
			return true
		}
	}
	return false
}

// close ends the watch when its handle closes: waiting requests complete
// with STATUS_NOTIFY_CLEANUP.
func (w *watch) close() {
	w.mu.Lock()
	w.closed = true
	waiting := w.waiting
	w.waiting, w.events = nil, nil
	w.mu.Unlock()
	for _, wait := range waiting {
		go wait.finish(statusNotifyCleanup, changeNotifyBody(nil))
	}
}

// finish sends the request's final response.
func (n *notifyWait) finish(status uint32, body []byte) {
	close(n.done)
	n.c.unregister(n.op)
	n.c.sendAsyncFinal(n.hdr, n.op.asyncID, status, body)
}

// changeNotifyBody builds a CHANGE_NOTIFY response (MS-SMB2 section 2.2.36)
// holding FILE_NOTIFY_INFORMATION entries, each 4-byte aligned and linked
// to the next.
func changeNotifyBody(events []notifyEvent) []byte {
	var info []byte
	prev := -1
	for _, e := range events {
		start := (len(info) + 3) &^ 3
		for len(info) < start {
			info = append(info, 0)
		}
		if prev >= 0 {
			le32Put(info[prev:prev+4], uint32(start-prev))
		}
		prev = start
		name := wire.UTF16ToBytes(e.name)
		entry := make([]byte, 12, 12+len(name))
		le32Put(entry[4:8], e.action)
		le32Put(entry[8:12], uint32(len(name)))
		info = append(info, append(entry, name...)...)
	}
	body := make([]byte, 8, 8+max(len(info), 1))
	binary.LittleEndian.PutUint16(body[0:2], 9)
	if len(info) > 0 {
		binary.LittleEndian.PutUint16(body[2:4], wire.HeaderSize+8)
		binary.LittleEndian.PutUint32(body[4:8], uint32(len(info)))
		return append(body, info...)
	}
	return append(body, 0)
}

func (c *request) handleChangeNotify(ctx context.Context, msg []byte, hdr *wire.Header, tr *tree) (uint32, bool) {
	var req changeNotifyRequest
	if err := req.Parse(msg); err != nil {
		return c.errBody(wire.StatusInvalidParameter), false
	}
	if tr == nil {
		return c.errBody(wire.StatusNetworkNameDeleted), false
	}
	oh, ok := tr.open(req.FileId)
	if !ok {
		return c.errBody(wire.StatusInvalidHandle), false
	}
	if !oh.isDir {
		return c.errBody(wire.StatusInvalidParameter), false
	}

	w := c.srv.watchFor(tr, oh, req)

	op := c.registerPending(hdr.MessageId)
	interim := *hdr
	interim.Credit = c.grantCredits(hdr.Credit)
	c.out = append(c.out, c.buildAsyncInterim(&interim, op.asyncID)...)
	wait := &notifyWait{c: c.conn, op: op, hdr: *hdr, done: make(chan struct{})}
	// The request joins the watch once the interim response is queued, so
	// a change arriving meanwhile cannot be answered ahead of it.
	c.after = append(c.after, func() { w.wait(wait) })
	return wire.StatusPending, true
}

// watchFor returns the handle's watch, setting it up on its first
// CHANGE_NOTIFY with that request's filter, tree flag and buffer size, as
// Windows does.
func (s *Server) watchFor(tr *tree, oh *openHandle, req changeNotifyRequest) *watch {
	oh.watchMu.Lock()
	defer oh.watchMu.Unlock()
	if oh.watch != nil {
		return oh.watch
	}
	dir := strings.ToLower(strings.Trim(strings.ReplaceAll(oh.currentPath(), "/", `\`), `\`))
	depth := 0
	if dir != "" {
		depth = strings.Count(dir, `\`) + 1
	}
	w := &watch{
		share:    strings.ToLower(tr.share.Name()),
		dir:      dir,
		depth:    depth,
		tree:     req.Flags&SMB2WatchTree != 0,
		filter:   req.CompletionFilter,
		maxBytes: min(req.OutputBufferLength, maxNotifyBuffer),
	}
	oh.watch = w
	s.notifyHub().add(tr.share, w)
	return w
}

// endWatch ends a closing handle's watch, if it has one.
func (s *Server) endWatch(oh *openHandle) {
	oh.watchMu.Lock()
	w := oh.watch
	oh.watch = nil
	oh.watchMu.Unlock()
	if w == nil {
		return
	}
	w.close()
	s.notifyHub().remove(w)
}

// handleCancel cancels a request that went async, named by its async id,
// or by its message id when the client has not seen the interim response.
func (c *request) handleCancel(hdr *wire.Header) {
	if hdr.Flags&wire.FlagAsyncCommand != 0 {
		c.cancelPending(hdr.AsyncId)
		return
	}
	c.pendingMu.Lock()
	op := c.pendingByMsg[hdr.MessageId]
	c.pendingMu.Unlock()
	if op != nil {
		op.cancel()
	}
}
