package server

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// notifyEvents decodes the FILE_NOTIFY_INFORMATION entries of a
// CHANGE_NOTIFY response, following each entry's link to the next.
func notifyEvents(t *testing.T, resp []byte) []notifyEvent {
	t.Helper()
	body := resp[wire.HeaderSize:]
	if size := binary.LittleEndian.Uint16(body[0:2]); size != 9 {
		t.Fatalf("CHANGE_NOTIFY response StructureSize = %d, want 9", size)
	}
	offset := binary.LittleEndian.Uint16(body[2:4])
	length := binary.LittleEndian.Uint32(body[4:8])
	if length == 0 {
		return nil
	}
	info := resp[offset : uint32(offset)+length]
	var events []notifyEvent
	for {
		next := binary.LittleEndian.Uint32(info[0:4])
		nameLen := binary.LittleEndian.Uint32(info[8:12])
		events = append(events, notifyEvent{binary.LittleEndian.Uint32(info[4:8]), wire.UTF16FromBytes(info[12 : 12+nameLen])})
		if next == 0 {
			return events
		}
		if next%4 != 0 {
			t.Fatalf("NextEntryOffset %d is not 4-byte aligned", next)
		}
		info = info[next:]
	}
}

// watchOn opens dir and sends its first CHANGE_NOTIFY, returning the handle
// and the request's async id.
func (c *deleteClient) watchOn(dir string, filter uint32, tree bool, outLen uint32) ([16]byte, uint64) {
	c.t.Helper()
	fid := c.mustOpen(dir, wire.FileOpen, accessRead, fileDirectory)
	return fid, c.notify(fid, filter, tree, outLen)
}

// notify sends a CHANGE_NOTIFY on fid and returns its async id.
func (c *deleteClient) notify(fid [16]byte, filter uint32, tree bool, outLen uint32) uint64 {
	c.t.Helper()
	msg := buildChangeNotify(c.sessID, c.treeID, fid, filter)
	if tree {
		binary.LittleEndian.PutUint16(msg[wire.HeaderSize+2:wire.HeaderSize+4], SMB2WatchTree)
	}
	binary.LittleEndian.PutUint32(msg[wire.HeaderSize+4:wire.HeaderSize+8], outLen)
	mustWrite(c.t, c.fc, msg)
	rh, resp := readReply(c.t, c.fc)
	switch rh.Status {
	case wire.StatusPending:
		return rh.AsyncId
	case wire.StatusSuccess, statusNotifyEnumDir:
		// Answered at once from buffered changes: hand the response to
		// the next readNotify.
		c.early = append(c.early, resp)
		return 0
	}
	c.t.Fatalf("change_notify: status %#x", rh.Status)
	return 0
}

// readNotify reads a CHANGE_NOTIFY's completion.
func (c *deleteClient) readNotify() (wire.Header, []byte) {
	c.t.Helper()
	if len(c.early) > 0 {
		resp := c.early[0]
		c.early = c.early[1:]
		var h wire.Header
		_ = h.Parse(resp)
		return h, resp
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			c.t.Error("no CHANGE_NOTIFY completion within 5s")
			_ = c.fc.Underlying().Close()
		}
	}()
	return readReply(c.t, c.fc)
}

const allNames = FileNotifyChangeFileName | FileNotifyChangeDirName

// Changes made while no request is waiting are kept for the next one,
// rather than lost between a completion and the client asking again, and
// several changes arrive linked in one response.
func TestChangeNotifyKeepsChangesBetweenRequests(t *testing.T) {
	srv := newTestServer(newMemBackend())
	watcher := connectDeleteClient(t, srv)
	other := connectDeleteClient(t, srv)
	other.close(other.mustOpen("dir", wire.FileCreate, genericAll, fileDirectory))

	fid, _ := watcher.watchOn("dir", allNames|FileNotifyChangeSize, false, 4096)
	other.put(`dir\a.txt`, "a")
	if _, resp := watcher.readNotify(); len(notifyEvents(t, resp)) == 0 {
		t.Fatal("first notification empty")
	}

	// Nothing is waiting now: these must be kept.
	other.put(`dir\b.txt`, "b")
	other.close(other.mustOpen(`dir\sub`, wire.FileCreate, genericAll, fileDirectory))
	b := other.mustOpen(`dir\b.txt`, wire.FileOpen, genericAll, 0)
	other.rename(b, `dir\c.txt`, false)
	other.close(b)
	other.close(other.mustOpen(`dir\a.txt`, wire.FileOpen, genericAll, createDeleteOnCl))

	watcher.notify(fid, allNames|FileNotifyChangeSize, false, 4096)
	_, resp := watcher.readNotify()
	got := notifyEvents(t, resp)
	// a.txt's write came after the first notification completed, so it
	// waited for this one.
	want := []notifyEvent{
		{fileActionModified, "a.txt"},
		{fileActionAdded, "b.txt"},
		{fileActionModified, "b.txt"},
		{fileActionAdded, "sub"},
		{fileActionRenamedOldName, "b.txt"},
		{fileActionRenamedNewName, "c.txt"},
		{fileActionRemoved, "a.txt"},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

// A watch sees only what its filter asks for, and only its own directory
// unless it watches the whole tree, where names are relative to it.
func TestChangeNotifyFilterAndTree(t *testing.T) {
	srv := newTestServer(newMemBackend())
	flat := connectDeleteClient(t, srv)
	tree := connectDeleteClient(t, srv)
	other := connectDeleteClient(t, srv)
	other.close(other.mustOpen("top", wire.FileCreate, genericAll, fileDirectory))
	other.close(other.mustOpen(`top\deep`, wire.FileCreate, genericAll, fileDirectory))

	// Only directory names: a file created is not reported.
	flat.watchOn("top", FileNotifyChangeDirName, false, 4096)
	tree.watchOn("top", FileNotifyChangeFileName, true, 4096)

	other.put(`top\deep\inner.txt`, "x")
	other.close(other.mustOpen(`top\newdir`, wire.FileCreate, genericAll, fileDirectory))

	_, resp := flat.readNotify()
	if got := notifyEvents(t, resp); len(got) != 1 || got[0] != (notifyEvent{fileActionAdded, "newdir"}) {
		t.Errorf("directory-name watch saw %v, want only newdir added", got)
	}
	_, resp = tree.readNotify()
	if got := notifyEvents(t, resp); len(got) == 0 || got[0] != (notifyEvent{fileActionAdded, `deep\inner.txt`}) {
		t.Errorf("tree watch saw %v, want deep\\inner.txt added first", got)
	}
}

// More changes than the client's buffer holds complete the request with
// STATUS_NOTIFY_ENUM_DIR, telling the client to re-read the directory,
// instead of dropping some silently.
func TestChangeNotifyOverflowAsksForEnumeration(t *testing.T) {
	srv := newTestServer(newMemBackend())
	watcher := connectDeleteClient(t, srv)
	other := connectDeleteClient(t, srv)
	fid, _ := watcher.watchOn("", allNames, false, 64)
	other.put("first.txt", "x")
	watcher.readNotify()

	for _, name := range []string{"one-long-name.txt", "two-long-name.txt", "three-long-name.txt"} {
		other.put(name, "x")
	}
	watcher.notify(fid, allNames, false, 64)
	if rh, _ := watcher.readNotify(); rh.Status != statusNotifyEnumDir {
		t.Fatalf("overflowing watch completed with %#x, want STATUS_NOTIFY_ENUM_DIR", rh.Status)
	}
}

// Closing the watched handle completes its waiting request with
// STATUS_NOTIFY_CLEANUP, and CANCEL completes it as cancelled.
func TestChangeNotifyCloseAndCancel(t *testing.T) {
	srv := newTestServer(newMemBackend())
	c := connectDeleteClient(t, srv)
	fid, _ := c.watchOn("", allNames, false, 4096)
	mustWrite(t, c.fc, buildClose(c.sessID, c.treeID, fid))
	// The close's response and the notification's completion can arrive in
	// either order.
	statuses := map[uint16]uint32{}
	for range 2 {
		rh, _ := c.readNotify()
		statuses[rh.Command] = rh.Status
	}
	if statuses[wire.CmdChangeNotify] != statusNotifyCleanup || statuses[wire.CmdClose] != wire.StatusSuccess {
		t.Fatalf("after closing the watched handle: %#x, want the close to succeed and the watch to end with STATUS_NOTIFY_CLEANUP", statuses)
	}
	if hub := srv.notifyHub(); hub.watching("share") {
		t.Fatal("watch still registered after its handle closed")
	}

	_, asyncID := c.watchOn("", allNames, false, 4096)
	cancel := wire.NewHeader(wire.CmdCancel)
	cancel.SessionId = c.sessID
	cancel.Flags = wire.FlagAsyncCommand
	cancel.AsyncId = asyncID
	mustWrite(t, c.fc, append(cancel.Append(nil), 4, 0, 0, 0))
	if rh, _ := c.readNotify(); rh.Status != wire.StatusCancelled || rh.AsyncId != asyncID {
		t.Fatalf("after CANCEL: status %#x async %d, want STATUS_CANCELLED for %d", rh.Status, rh.AsyncId, asyncID)
	}
}

// changeFeed is a backend that reports its own changes, as Bluestone's does
// for changes made through NFS.
type changeFeed struct {
	*memBackend
	fn   func(vfs.Change)
	subs int
}

func (b *changeFeed) NotifyChanges(fn func(vfs.Change)) func() {
	b.fn = fn
	b.subs++
	return func() { b.subs-- }
}

// A backend that reports changes is subscribed to once per share, however
// many watches there are, and its changes reach the watches; the server does
// not report changes made through it a second time.
func TestChangeNotifyFromBackend(t *testing.T) {
	backend := &changeFeed{memBackend: newMemBackend()}
	srv := newTestServer(backend)
	first := connectDeleteClient(t, srv)
	second := connectDeleteClient(t, srv)
	first.watchOn("", allNames, false, 4096)
	second.watchOn("", allNames, false, 4096)
	if backend.subs != 1 {
		t.Fatalf("backend subscribed %d times for two watches, want once", backend.subs)
	}

	// A change made outside SMB.
	backend.fn(vfs.Change{Action: vfs.ChangeAdded, Path: "from-nfs.txt"})
	for _, c := range []*deleteClient{first, second} {
		if _, resp := c.readNotify(); len(notifyEvents(t, resp)) != 1 {
			t.Fatal("backend change not delivered to every watch")
		}
	}
}
