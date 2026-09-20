package server

import (
	"sync"
	"testing"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// A client cannot make the server hold more open files than the limit
// allows, and the ones it already holds keep working.
func TestOpensPerSessionLimit(t *testing.T) {
	srv := newTestServer(newMemBackend())
	srv.limits = Limits{OpensPerSession: 2}

	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(srv, srvConn)()
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)

	var opened [][16]byte
	for i, name := range []string{"one.txt", "two.txt"} {
		mustWrite(t, fc, buildCreate(sessID, treeID, name, wire.FileOpenIf))
		rh, resp := readReply(t, fc)
		if rh.Status != wire.StatusSuccess {
			t.Fatalf("create %d: status %#x, want success", i, rh.Status)
		}
		var fid [16]byte
		copy(fid[:], resp[64+64:64+80])
		opened = append(opened, fid)
	}

	mustWrite(t, fc, buildCreate(sessID, treeID, "three.txt", wire.FileOpenIf))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusInsufficientResources {
		t.Fatalf("create past the limit: status %#x, want INSUFFICIENT_RESOURCES", rh.Status)
	}

	// Closing one makes room again.
	mustWrite(t, fc, buildClose(sessID, treeID, opened[0]))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
		t.Fatalf("close: %#x", rh.Status)
	}
	mustWrite(t, fc, buildCreate(sessID, treeID, "three.txt", wire.FileOpenIf))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
		t.Fatalf("create after closing one: status %#x, want success", rh.Status)
	}
}

func TestTreesPerSessionLimit(t *testing.T) {
	srv := newTestServer(newMemBackend())
	srv.limits = Limits{TreesPerSession: 1}

	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(srv, srvConn)()
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeConnect(t, fc, sessID)

	mustWrite(t, fc, buildTreeConnect(sessID, `\\server\share`))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusInsufficientResources {
		t.Fatalf("second tree connect: status %#x, want INSUFFICIENT_RESOURCES", rh.Status)
	}
}

func TestSessionsPerConnectionLimit(t *testing.T) {
	srv := newTestServer(newMemBackend())
	srv.limits = Limits{SessionsPerConnection: 1}

	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(srv, srvConn)()
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessionSetup(t, fc)

	// A SESSION_SETUP with no session id asks for a second session.
	mustWrite(t, fc, buildSessionSetup([]byte{0x60, 0x04, 0xbe, 0xef}))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusInsufficientResources {
		t.Fatalf("second session: status %#x, want INSUFFICIENT_RESOURCES", rh.Status)
	}
}

type recordingGate struct {
	mu       sync.Mutex
	allow    bool
	attempts []bool
	addrs    []string
}

func (g *recordingGate) Allow(string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.allow
}

func (g *recordingGate) Attempted(remoteAddr string, ok bool, _ time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.attempts = append(g.attempts, ok)
	g.addrs = append(g.addrs, remoteAddr)
}

func (g *recordingGate) snapshot() ([]bool, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]bool(nil), g.attempts...), append([]string(nil), g.addrs...)
}

// The gate is told about each attempt that reached the authenticator, and a
// blocked client is answered as though its password were wrong.
func TestAuthGate(t *testing.T) {
	gate := &recordingGate{allow: true}
	srv := newTestServer(newMemBackend())
	srv.authGate = gate

	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(srv, srvConn)()
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessionSetup(t, fc)

	attempts, _ := gate.snapshot()
	if len(attempts) != 1 || !attempts[0] {
		t.Fatalf("attempts after one login = %v, want one success", attempts)
	}

	gate.mu.Lock()
	gate.allow = false
	gate.mu.Unlock()

	mustWrite(t, fc, buildSessionSetup([]byte{0x60, 0x04, 0xbe, 0xef}))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusLogonFailure {
		t.Fatalf("blocked login: status %#x, want LOGON_FAILURE", rh.Status)
	}
	if attempts, _ := gate.snapshot(); len(attempts) != 1 {
		t.Fatalf("attempts = %v, want the blocked one not to reach the authenticator", attempts)
	}
}

// Without limits or a gate, a server behaves as it did before they existed.
func TestNoLimitsByDefault(t *testing.T) {
	srv := newTestServer(newMemBackend())

	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(srv, srvConn)()
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)

	for i := 0; i < 8; i++ {
		mustWrite(t, fc, buildCreate(sessID, treeID, "many.txt", wire.FileOpenIf))
		if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
			t.Fatalf("open %d: status %#x, want success", i, rh.Status)
		}
	}
}
