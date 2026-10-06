package nfs

import (
	"testing"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// The verifier comes from the path alone: a client paging through a
// directory gets the same one on every call, whatever was listed or
// invalidated in between, with nothing remembered per directory.
func TestStableVerifierDependsOnThePathOnly(t *testing.T) {
	h := NewStableVerifierHandler(nil, logging.NewKVLogger(zap.NewNop())).(*StableVerifierHandler)

	first := h.VerifierFor("/bucket/dir", nil)
	if first == 0 {
		t.Fatal("verifier is 0, which clients read as no verifier")
	}
	if again := h.VerifierFor("/bucket/dir", nil); again != first {
		t.Fatalf("second verifier for the same path = %d, want %d", again, first)
	}
	if other := h.VerifierFor("/bucket/other", nil); other == first {
		t.Fatalf("two paths share the verifier %d", first)
	}

	// A second handler, as after a restart, agrees.
	restarted := NewStableVerifierHandler(nil, logging.NewKVLogger(zap.NewNop())).(*StableVerifierHandler)
	if got := restarted.VerifierFor("/bucket/dir", nil); got != first {
		t.Fatalf("verifier from a new handler = %d, want %d", got, first)
	}
}
