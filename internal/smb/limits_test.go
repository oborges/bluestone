package smb

import (
	"net"
	"testing"
	"time"

	client "github.com/hirochachacha/go-smb2"
	"github.com/sonroyaalmerol/go-smb-server/smb/ntlmssp"
)

// dialShare connects a real SMB client and mounts the share, leaving the
// caller to close the connection.
func dialShare(t *testing.T, g *testGateway, user, password string) (net.Conn, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", g.server.Address(), 3*time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	dialer := &client.Dialer{Initiator: &client.NTLMInitiator{User: user, Password: password, Domain: "BLUESTONE"}}
	session, err := dialer.Dial(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := session.Mount("share"); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	t.Cleanup(func() { conn.Close() })
	return conn, nil
}

// A client cannot open more connections than the limit allows, and closing
// one makes room for another.
func TestConnectionsPerClientLimit(t *testing.T) {
	g := startGateway(t, func(opts *ServerOptions) {
		opts.Limits = Limits{ConnectionsPerClient: 2}
	})

	first, err := dialShare(t, g, "alice", "secret")
	if err != nil {
		t.Fatalf("first connection: %v", err)
	}
	if _, err := dialShare(t, g, "alice", "secret"); err != nil {
		t.Fatalf("second connection: %v", err)
	}
	if _, err := dialShare(t, g, "alice", "secret"); err == nil {
		t.Fatal("third connection was accepted past the limit")
	}

	// Closing one makes room again. The server notices the close on its own
	// schedule, so give it a moment.
	first.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := dialShare(t, g, "alice", "secret")
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no room after closing a connection: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The server as a whole stops accepting past its limit.
func TestTotalConnectionLimit(t *testing.T) {
	g := startGateway(t, func(opts *ServerOptions) {
		opts.Limits = Limits{Connections: 1}
	})

	if _, err := dialShare(t, g, "alice", "secret"); err != nil {
		t.Fatalf("first connection: %v", err)
	}
	if _, err := dialShare(t, g, "alice", "secret"); err == nil {
		t.Fatal("second connection was accepted past the server limit")
	}
}

// A client that keeps guessing passwords is refused even once it guesses
// right, until its block passes.
func TestRepeatedBadPasswordsBlockAClient(t *testing.T) {
	g := startGateway(t, func(opts *ServerOptions) {
		opts.AuthLimits = AuthLimits{Failures: 3, Window: time.Minute, Block: time.Minute, MaxBlock: time.Minute}
	})

	for i := 0; i < 3; i++ {
		if _, err := dialShare(t, g, "alice", "wrong"); err == nil {
			t.Fatalf("attempt %d with a wrong password succeeded", i)
		}
	}
	if _, err := dialShare(t, g, "alice", "secret"); err == nil {
		t.Fatal("the right password was accepted while the client is blocked")
	}

	// The block is what refuses it: with the block behind it, the same
	// credentials work.
	g.server.gate.mu.Lock()
	for _, record := range g.server.gate.clients {
		record.blockedTill = time.Now().Add(-time.Second)
	}
	g.server.gate.mu.Unlock()

	if _, err := dialShare(t, g, "alice", "secret"); err != nil {
		t.Fatalf("after the block passed: %v", err)
	}
}

// Ordinary clients are unaffected: a wrong password now and then never
// reaches the threshold.
func TestOccasionalBadPasswordDoesNotBlock(t *testing.T) {
	g := startGateway(t, func(opts *ServerOptions) {
		opts.AuthLimits = AuthLimits{Failures: 5, Window: time.Minute, Block: time.Minute, MaxBlock: time.Minute}
	})

	for i := 0; i < 3; i++ {
		if _, err := dialShare(t, g, "alice", "wrong"); err == nil {
			t.Fatal("a wrong password was accepted")
		}
		conn, err := dialShare(t, g, "alice", "secret")
		if err != nil {
			t.Fatalf("round %d: the right password was refused: %v", i, err)
		}
		conn.Close()
	}
}

// An account configured with an NT hash authenticates exactly as one
// configured with the password it came from.
func TestNTLMHashAuthenticates(t *testing.T) {
	g := startGateway(t, func(opts *ServerOptions) {
		opts.Users = []User{{Name: "hashed", NTLMHash: ntlmssp.NTHash("secret")}}
	})

	conn, err := dialShare(t, g, "hashed", "secret")
	if err != nil {
		t.Fatalf("login with the hashed account: %v", err)
	}
	conn.Close()

	if _, err := dialShare(t, g, "hashed", "wrong"); err == nil {
		t.Fatal("a wrong password was accepted for the hashed account")
	}
}

// The hash wins when both are configured, so a stale password left in the
// file cannot be used to log in.
func TestNTLMHashWinsOverPassword(t *testing.T) {
	g := startGateway(t, func(opts *ServerOptions) {
		opts.Users = []User{{Name: "hashed", Password: "stale", NTLMHash: ntlmssp.NTHash("current")}}
	})

	conn, err := dialShare(t, g, "hashed", "current")
	if err != nil {
		t.Fatalf("login with the hashed password: %v", err)
	}
	conn.Close()
	if _, err := dialShare(t, g, "hashed", "stale"); err == nil {
		t.Fatal("the stale password was accepted")
	}
}
