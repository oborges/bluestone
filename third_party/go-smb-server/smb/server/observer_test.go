package server

import (
	"sync"
	"testing"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

type countingObserver struct {
	mu          sync.Mutex
	conns       int
	sessions    int
	maxSessions int
	requests    []observed
}

type observed struct {
	command uint16
	status  uint32
}

func (o *countingObserver) ConnectionOpened() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.conns++
}

func (o *countingObserver) ConnectionClosed() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.conns--
}

func (o *countingObserver) SessionOpened() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sessions++
	if o.sessions > o.maxSessions {
		o.maxSessions = o.sessions
	}
}

func (o *countingObserver) SessionClosed() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sessions--
}

func (o *countingObserver) RequestCompleted(command uint16, status uint32, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, observed{command, status})
}

func (o *countingObserver) snapshot() (conns, sessions, maxSessions int, requests []observed) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.conns, o.sessions, o.maxSessions, append([]observed(nil), o.requests...)
}

// An observer sees each connection, each authenticated session, and every
// command, including the commands of a compound request.
func TestObserverReportsConnectionsSessionsAndRequests(t *testing.T) {
	obs := &countingObserver{}
	srv := newTestServer(newMemBackend())
	srv.observer = obs

	client, srvConn := newPipeConns()
	stop := serveOn(srv, srvConn)

	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)
	mustWrite(t, fc, buildCreate(sessID, treeID, "seen.txt", wire.FileOpenIf))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
		t.Fatalf("create: %#x", rh.Status)
	}
	// A command that fails is reported with the status the client saw.
	var missing [16]byte
	mustWrite(t, fc, buildClose(sessID, treeID, missing))
	if rh, _ := readReply(t, fc); rh.Status == wire.StatusSuccess {
		t.Fatal("close of an unknown handle succeeded")
	}

	_, sessions, maxSessions, requests := obs.snapshot()
	if maxSessions != 1 || sessions != 1 {
		t.Errorf("sessions = %d (max %d), want 1 open", sessions, maxSessions)
	}
	byCommand := map[uint16]uint32{}
	for _, r := range requests {
		byCommand[r.command] = r.status
	}
	for _, cmd := range []uint16{wire.CmdNegotiate, wire.CmdSessionSetup, wire.CmdTreeConnect, wire.CmdCreate} {
		if status, ok := byCommand[cmd]; !ok || status != wire.StatusSuccess {
			t.Errorf("command %d: status %#x, reported %v; want success", cmd, status, ok)
		}
	}
	if status, ok := byCommand[wire.CmdClose]; !ok || status == wire.StatusSuccess {
		t.Errorf("failed close: status %#x, reported %v; want the failure", status, ok)
	}

	// Ending the connection ends the session and the connection count.
	stop()
	_ = client.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conns, sessions, _, _ := obs.snapshot()
		if conns == 0 && sessions == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the connection ended: %d connections, %d sessions; want 0 and 0", conns, sessions)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A session the client logs off from is reported closed once, not twice when
// the connection later goes away.
func TestObserverCountsLogoffOnce(t *testing.T) {
	obs := &countingObserver{}
	srv := newTestServer(newMemBackend())
	srv.observer = obs

	client, srvConn := newPipeConns()
	stop := serveOn(srv, srvConn)
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)

	hdr := wire.NewHeader(wire.CmdLogoff)
	hdr.SessionId = sessID
	hdr.MessageId = 9
	hdr.Credit = 1
	body := make([]byte, 4)
	body[0] = 4
	mustWrite(t, fc, append(hdr.Append(nil), body...))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
		t.Fatalf("logoff: %#x", rh.Status)
	}

	stop()
	_ = client.Close()
	time.Sleep(100 * time.Millisecond)
	if _, sessions, maxSessions, _ := obs.snapshot(); sessions != 0 || maxSessions != 1 {
		t.Fatalf("sessions = %d (max %d), want 0 open and 1 seen", sessions, maxSessions)
	}
}
