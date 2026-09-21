package server

import (
	"encoding/binary"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// relatedChain joins requests into one compound, each after the first
// related to the one before it, as macOS sends CREATE, READ and CLOSE.
func relatedChain(requests ...[]byte) []byte {
	var out []byte
	for i, req := range requests {
		req = append([]byte(nil), req...)
		if i > 0 {
			flags := binary.LittleEndian.Uint32(req[16:20])
			binary.LittleEndian.PutUint32(req[16:20], flags|wire.FlagRelatedOps)
		}
		if i < len(requests)-1 {
			for len(req)%8 != 0 {
				req = append(req, 0)
			}
			binary.LittleEndian.PutUint32(req[20:24], uint32(len(req)))
		}
		out = append(out, req...)
	}
	return out
}

// chainStatuses reads the status of each response in a compound reply.
func chainStatuses(t *testing.T, fc *transport.FramedConn) []uint32 {
	t.Helper()
	_, msg := readReply(t, fc)
	var statuses []uint32
	for {
		statuses = append(statuses, binary.LittleEndian.Uint32(msg[8:12]))
		next := binary.LittleEndian.Uint32(msg[20:24])
		if next == 0 {
			return statuses
		}
		msg = msg[next:]
	}
}

// A READ that reaches the end of the file does not fail the CLOSE after it
// in the same chain: macOS reads a new, empty AFP_AfpInfo stream this way
// and reports an I/O error when the CLOSE fails. The file really is closed.
func TestCompoundReadAtEndStillCloses(t *testing.T) {
	backend := newMemBackend()
	srv := newTestServer(backend)
	c := connectDeleteClient(t, srv)
	c.put("empty.txt", "")

	var related [16]byte
	for i := range related {
		related[i] = 0xFF
	}
	mustWrite(t, c.fc, relatedChain(
		buildCreateWith(c.sessID, c.treeID, "empty.txt", wire.FileOpen, genericAll, 0),
		buildRead(c.sessID, c.treeID, related, 0, 60),
		buildClose(c.sessID, c.treeID, related),
	))
	got := chainStatuses(t, c.fc)
	want := []uint32{wire.StatusSuccess, wire.StatusEndOfFile, wire.StatusSuccess}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("chain statuses = %#x, want %#x", got, want)
	}
	if opens := srv.fileTable().files; len(opens) != 0 {
		t.Fatalf("%d files still open after the chain's CLOSE", len(opens))
	}
}

// A CREATE that fails fails the related requests after it, which name the
// file it would have opened.
func TestCompoundFailedCreateFailsRelated(t *testing.T) {
	c := connectDeleteClient(t, newTestServer(newMemBackend()))
	var related [16]byte
	for i := range related {
		related[i] = 0xFF
	}
	mustWrite(t, c.fc, relatedChain(
		buildCreateWith(c.sessID, c.treeID, "missing.txt", wire.FileOpen, genericAll, 0),
		buildRead(c.sessID, c.treeID, related, 0, 60),
		buildClose(c.sessID, c.treeID, related),
	))
	for i, status := range chainStatuses(t, c.fc) {
		if status != wire.StatusObjectNameNotFound {
			t.Errorf("request %d: status %#x, want OBJECT_NAME_NOT_FOUND", i, status)
		}
	}
}
