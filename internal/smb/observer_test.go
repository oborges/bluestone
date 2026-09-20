package smb

import (
	"net"
	"testing"
	"time"

	client "github.com/hirochachacha/go-smb2"
	"github.com/oborges/bluestone/internal/lock"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// Stats is what the health check reports, so it has to follow clients
// connecting, authenticating and opening files, and come back to zero.
func TestServerStatsFollowClients(t *testing.T) {
	opens := lock.NewShareTable(lock.ShareOptions{})
	g := startGateway(t, func(opts *ServerOptions) { opts.Opens = opens })

	if stats := g.server.Stats(); stats.Connections != 0 || stats.Sessions != 0 || stats.OpenFiles != 0 {
		t.Fatalf("before any client: %+v, want zeros", stats)
	}
	if !g.server.Running() {
		t.Fatal("Running() = false for a started server")
	}

	share, err := g.mount(t, "alice", "secret", "BLUESTONE")
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if stats := waitForStats(t, g, func(s Stats) bool { return s.Connections == 1 && s.Sessions == 1 }); stats.Connections != 1 {
		t.Fatalf("with one client: %+v, want 1 connection and 1 session", stats)
	}

	f, err := share.Create("counted.txt")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if stats := waitForStats(t, g, func(s Stats) bool { return s.OpenFiles > 0 }); stats.OpenFiles != 1 {
		t.Fatalf("with a file open: %+v, want 1 open file", stats)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if stats := waitForStats(t, g, func(s Stats) bool { return s.OpenFiles == 0 }); stats.OpenFiles != 0 {
		t.Fatalf("after closing the file: %+v, want no open files", stats)
	}

	// A second client is counted alongside the first.
	if _, err := g.mount(t, "alice", "secret", "BLUESTONE"); err != nil {
		t.Fatalf("second mount: %v", err)
	}
	if stats := waitForStats(t, g, func(s Stats) bool { return s.Sessions == 2 }); stats.Connections != 2 {
		t.Fatalf("with two clients: %+v, want 2 connections", stats)
	}
}

// A stopped server is not serving, which is what makes the health check fail.
func TestServerRunningFollowsStop(t *testing.T) {
	g := startGateway(t, func(opts *ServerOptions) {})
	if !g.server.Running() {
		t.Fatal("Running() = false for a started server")
	}
	if err := g.server.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if g.server.Running() {
		t.Fatal("Running() = true after Stop()")
	}
}

// A client that drops its connection without logging off still stops being
// counted, which is what keeps the gauges honest after a client crashes.
func TestServerStatsReturnToZeroOnDisconnect(t *testing.T) {
	g := startGateway(t, func(opts *ServerOptions) {})

	conn, err := net.DialTimeout("tcp", g.server.Address(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	dialer := &client.Dialer{Initiator: &client.NTLMInitiator{User: "alice", Password: "secret", Domain: "BLUESTONE"}}
	session, err := dialer.Dial(conn)
	if err != nil {
		conn.Close()
		t.Fatalf("dial session: %v", err)
	}
	if _, err := session.Mount("share"); err != nil {
		conn.Close()
		t.Fatalf("mount: %v", err)
	}
	waitForStats(t, g, func(s Stats) bool { return s.Sessions == 1 })

	// Drop the connection without logging off, as a crashed client does.
	conn.Close()

	if stats := waitForStats(t, g, func(s Stats) bool { return s.Sessions == 0 && s.Connections == 0 }); stats.Sessions != 0 || stats.Connections != 0 {
		t.Fatalf("after the client vanished: %+v, want no sessions or connections", stats)
	}
}

func waitForStats(t *testing.T, g *testGateway, want func(Stats) bool) Stats {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stats := g.server.Stats()
		if want(stats) || time.Now().After(deadline) {
			return stats
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCommandAndStatusNames(t *testing.T) {
	if got := commandName(wire.CmdQueryDirectory); got != "QUERY_DIRECTORY" {
		t.Errorf("commandName(QUERY_DIRECTORY) = %q", got)
	}
	// An unknown command shares one label, so a client cannot grow the
	// metric's cardinality by sending nonsense.
	if got := commandName(0x00FF); got != "OTHER" {
		t.Errorf("commandName(unknown) = %q, want OTHER", got)
	}
	if got := statusName(wire.StatusSharingViolation); got != "STATUS_SHARING_VIOLATION" {
		t.Errorf("statusName(SHARING_VIOLATION) = %q", got)
	}
	if got := statusName(0xC0000999); got != "0xC0000999" {
		t.Errorf("statusName(unnamed) = %q, want the number", got)
	}
}
