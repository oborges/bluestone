package smb

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeClock lets the tests move time rather than wait for it.
type fakeClock struct{ now time.Time }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestAuthGate(limits AuthLimits) (*authGate, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	gate := newAuthGate(limits, zap.NewNop())
	gate.now = func() time.Time { return clock.now }
	return gate, clock
}

const testClient = "192.0.2.10:4455"

// A client that keeps guessing is blocked, and each block is longer than the
// one before, up to the cap.
func TestAuthGateBlocksAfterRepeatedFailures(t *testing.T) {
	gate, clock := newTestAuthGate(AuthLimits{Failures: 3, Window: time.Minute, Block: 30 * time.Second, MaxBlock: 2 * time.Minute})

	for i := 0; i < 2; i++ {
		if !gate.Allow(testClient) {
			t.Fatalf("attempt %d refused before the limit", i)
		}
		gate.Attempted(testClient, false, 0)
	}
	if !gate.Allow(testClient) {
		t.Fatal("refused before reaching the failure limit")
	}
	gate.Attempted(testClient, false, 0)

	if gate.Allow(testClient) {
		t.Fatal("allowed after reaching the failure limit")
	}
	if gate.blockedClients() != 1 {
		t.Fatalf("blockedClients() = %d, want 1", gate.blockedClients())
	}

	// The block lifts on its own.
	clock.advance(31 * time.Second)
	if !gate.Allow(testClient) {
		t.Fatal("still blocked after the block passed")
	}

	// Failing again blocks for twice as long.
	for i := 0; i < 3; i++ {
		gate.Attempted(testClient, false, 0)
	}
	clock.advance(31 * time.Second)
	if gate.Allow(testClient) {
		t.Fatal("second block did not last longer than the first")
	}
	clock.advance(30 * time.Second)
	if !gate.Allow(testClient) {
		t.Fatal("second block outlasted its doubled length")
	}
}

// Blocks stop doubling at MaxBlock.
func TestAuthGateBlockIsCapped(t *testing.T) {
	gate, clock := newTestAuthGate(AuthLimits{Failures: 1, Window: time.Hour, Block: time.Minute, MaxBlock: 4 * time.Minute})

	for i := 0; i < 6; i++ {
		gate.Attempted(testClient, false, 0)
		clock.advance(5 * time.Minute)
		if !gate.Allow(testClient) {
			t.Fatalf("round %d: still blocked after 5 minutes, longer than the cap", i)
		}
	}
	gate.Attempted(testClient, false, 0)
	clock.advance(4*time.Minute + time.Second)
	if !gate.Allow(testClient) {
		t.Fatal("blocked for longer than MaxBlock")
	}
}

// A successful login clears what came before it, so one mistyped password
// does not count against a user for the rest of the window.
func TestAuthGateSuccessClearsFailures(t *testing.T) {
	gate, _ := newTestAuthGate(AuthLimits{Failures: 3, Window: time.Minute, Block: time.Minute, MaxBlock: time.Minute})

	gate.Attempted(testClient, false, 0)
	gate.Attempted(testClient, false, 0)
	gate.Attempted(testClient, true, 0)
	gate.Attempted(testClient, false, 0)
	gate.Attempted(testClient, false, 0)

	if !gate.Allow(testClient) {
		t.Fatal("blocked although a success cleared the earlier failures")
	}
}

// Failures older than the window are forgotten, so slow retries never build
// up to a block.
func TestAuthGateForgetsOldFailures(t *testing.T) {
	gate, clock := newTestAuthGate(AuthLimits{Failures: 3, Window: time.Minute, Block: time.Minute, MaxBlock: time.Minute})

	for i := 0; i < 5; i++ {
		gate.Attempted(testClient, false, 0)
		clock.advance(2 * time.Minute)
		if !gate.Allow(testClient) {
			t.Fatalf("attempt %d: blocked by failures older than the window", i)
		}
	}
	if blocked := gate.blockedClients(); blocked != 0 {
		t.Fatalf("blockedClients() = %d, want 0", blocked)
	}
	// Sampling drops records for clients that have gone quiet.
	gate.mu.Lock()
	tracked := len(gate.clients)
	gate.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("tracked clients = %d, want the record dropped", tracked)
	}
}

// One client's failures do not block another.
func TestAuthGateIsPerClient(t *testing.T) {
	gate, _ := newTestAuthGate(AuthLimits{Failures: 2, Window: time.Minute, Block: time.Minute, MaxBlock: time.Minute})

	for i := 0; i < 2; i++ {
		gate.Attempted("198.51.100.7:1234", false, 0)
	}
	if gate.Allow("198.51.100.7:1234") {
		t.Fatal("the failing client was not blocked")
	}
	if !gate.Allow(testClient) {
		t.Fatal("a different client was blocked by someone else's failures")
	}
	// The same client from another port is the same client.
	if gate.Allow("198.51.100.7:9999") {
		t.Fatal("the failing client was allowed from a different source port")
	}
}

func TestAuthLimitDefaults(t *testing.T) {
	limits := AuthLimits{}.withDefaults()
	if limits.Failures != DefaultAuthFailures || limits.Window != DefaultAuthWindow ||
		limits.Block != DefaultAuthBlock || limits.MaxBlock != DefaultAuthMaxBlock {
		t.Fatalf("withDefaults() = %+v, want the documented defaults", limits)
	}
	// A block longer than the cap raises the cap rather than blocking for
	// less than asked.
	limits = AuthLimits{Block: time.Hour}.withDefaults()
	if limits.MaxBlock < limits.Block {
		t.Fatalf("MaxBlock %s is shorter than Block %s", limits.MaxBlock, limits.Block)
	}
}
