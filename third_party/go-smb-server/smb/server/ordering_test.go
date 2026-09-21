package server

import (
	"runtime"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// A client may send a WRITE and then CLOSE the file without waiting for the
// write's response. Writes run alongside other requests, but the CLOSE must
// not overtake one received before it: the write would find its handle gone
// and fail with STATUS_INVALID_HANDLE, which macOS reports as "Bad file
// descriptor" partway through a copy.
func TestCloseWaitsForWritesReceivedBeforeIt(t *testing.T) {
	// One scheduler thread makes the race certain: the read loop reads and
	// handles the CLOSE, already in its buffer, before the goroutine
	// handling the WRITE gets to run.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	backend := newMemBackend()
	srv := newTestServer(backend)
	srv.maxConcurrent = 8
	c := connectDeleteClient(t, srv)

	for i := range 20 {
		fid := c.mustOpen("pipelined.txt", wire.FileOverwriteIf, genericAll, 0)
		write := buildWrite(c.sessID, c.treeID, fid, 0, []byte("written before the close"))
		closeReq := buildClose(c.sessID, c.treeID, fid)
		// Both in one send, as a client pipelining them does.
		if err := c.fc.WriteMessage(write); err != nil {
			t.Fatal(err)
		}
		if err := c.fc.WriteMessage(closeReq); err != nil {
			t.Fatal(err)
		}
		statuses := map[uint16]uint32{}
		for range 2 {
			rh, _ := readReply(t, c.fc)
			statuses[rh.Command] = rh.Status
		}
		if statuses[wire.CmdWrite] != wire.StatusSuccess || statuses[wire.CmdClose] != wire.StatusSuccess {
			t.Fatalf("round %d: WRITE %#x, CLOSE %#x; want both to succeed", i, statuses[wire.CmdWrite], statuses[wire.CmdClose])
		}
	}
	if got := backend.contents("pipelined.txt"); got != "written before the close" {
		t.Fatalf("file holds %q", got)
	}
}

// A truncate sent right behind a write lands after it, not before: run the
// other way round the file would keep the bytes the truncate removed.
func TestTruncateWaitsForWritesReceivedBeforeIt(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	backend := newMemBackend()
	srv := newTestServer(backend)
	srv.maxConcurrent = 8
	c := connectDeleteClient(t, srv)

	for i := range 20 {
		fid := c.mustOpen("ordered.txt", wire.FileOverwriteIf, genericAll, 0)
		eof := make([]byte, 8)
		eof[0] = 4
		if err := c.fc.WriteMessage(buildWrite(c.sessID, c.treeID, fid, 0, []byte("0123456789"))); err != nil {
			t.Fatal(err)
		}
		if err := c.fc.WriteMessage(buildSetInfo(c.sessID, c.treeID, fid, wire.FileEndOfFileInformation, eof)); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if rh, _ := readReply(t, c.fc); rh.Status != wire.StatusSuccess {
				t.Fatalf("round %d: command %d status %#x", i, rh.Command, rh.Status)
			}
		}
		c.close(fid)
		if got := backend.contents("ordered.txt"); got != "0123" {
			t.Fatalf("round %d: file holds %q, want the write truncated to 4 bytes", i, got)
		}
	}
}
