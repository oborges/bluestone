package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore is an in-memory lease store; failAll simulates an unreachable
// object store.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	failAll bool
}

func newMemStore() *memStore {
	return &memStore{objects: make(map[string][]byte)}
}

var errStoreDown = errors.New("dial tcp: connection refused")

func (s *memStore) GetObject(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAll {
		return nil, errStoreDown
	}
	data, ok := s.objects[key]
	if !ok {
		// Wrapped like the production COS client so unwrap-blind checks
		// (os.IsNotExist instead of errors.Is) fail tests, not deployments.
		return nil, fmt.Errorf("object %s: %w", key, os.ErrNotExist)
	}
	return append([]byte(nil), data...), nil
}

func (s *memStore) PutObject(_ context.Context, key string, data []byte, _ map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAll {
		return errStoreDown
	}
	s.objects[key] = append([]byte(nil), data...)
	return nil
}

func (s *memStore) DeleteObject(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAll {
		return errStoreDown
	}
	delete(s.objects, key)
	return nil
}

func (s *memStore) currentLease(t *testing.T) *Lease {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[LeaseObjectKey]
	if !ok {
		return nil
	}
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		t.Fatalf("corrupt lease in store: %v", err)
	}
	return &lease
}

func opts(store ObjectStore, dir string) Options {
	return Options{
		Store:             store,
		HeartbeatInterval: 50 * time.Millisecond,
		LeaseTimeout:      300 * time.Millisecond,
		HolderMarkerDir:   dir,
	}
}

func TestAcquireFirstGateway(t *testing.T) {
	store := newMemStore()
	m, err := Acquire(context.Background(), opts(store, t.TempDir()))
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer m.Release()

	lease := store.currentLease(t)
	if lease == nil || lease.HolderID != m.HolderID() {
		t.Fatalf("lease holder = %+v, want %s", lease, m.HolderID())
	}
	if lease.Epoch != 1 {
		t.Fatalf("epoch = %d, want 1", lease.Epoch)
	}
}

func TestSecondGatewayIsFenced(t *testing.T) {
	store := newMemStore()
	m1, err := Acquire(context.Background(), opts(store, t.TempDir()))
	if err != nil {
		t.Fatalf("Acquire(first) error = %v", err)
	}
	defer m1.Release()

	_, err = Acquire(context.Background(), opts(store, t.TempDir()))
	var held *ErrLeaseHeld
	if !errors.As(err, &held) {
		t.Fatalf("Acquire(second) error = %v, want ErrLeaseHeld", err)
	}
	if held.Lease.HolderID != m1.HolderID() {
		t.Fatalf("conflicting holder = %s, want %s", held.Lease.HolderID, m1.HolderID())
	}
}

func TestStaleLeaseIsTakenOver(t *testing.T) {
	store := newMemStore()
	stale := Lease{
		HolderID:   "dead-node-1-abcd",
		Hostname:   "dead-node",
		Epoch:      7,
		AcquiredAt: time.Now().Add(-time.Hour),
		RenewedAt:  time.Now().Add(-time.Hour),
	}
	payload, _ := json.Marshal(stale)
	store.objects[LeaseObjectKey] = payload

	m, err := Acquire(context.Background(), opts(store, t.TempDir()))
	if err != nil {
		t.Fatalf("Acquire(over stale) error = %v", err)
	}
	defer m.Release()

	lease := store.currentLease(t)
	if lease.HolderID != m.HolderID() {
		t.Fatal("stale lease was not taken over")
	}
	if lease.Epoch != 8 {
		t.Fatalf("epoch = %d, want 8 (incremented across takeover)", lease.Epoch)
	}
}

func TestForceTakeoverStealsFreshLease(t *testing.T) {
	store := newMemStore()
	m1, err := Acquire(context.Background(), opts(store, t.TempDir()))
	if err != nil {
		t.Fatalf("Acquire(first) error = %v", err)
	}
	defer m1.Release()

	o := opts(store, t.TempDir())
	o.ForceTakeover = true
	m2, err := Acquire(context.Background(), o)
	if err != nil {
		t.Fatalf("Acquire(force) error = %v", err)
	}
	defer m2.Release()

	if store.currentLease(t).HolderID != m2.HolderID() {
		t.Fatal("force takeover did not replace the lease")
	}
}

func TestHeartbeatRenewsLease(t *testing.T) {
	store := newMemStore()
	m, err := Acquire(context.Background(), opts(store, t.TempDir()))
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer m.Release()

	first := store.currentLease(t).RenewedAt
	time.Sleep(150 * time.Millisecond)
	if !store.currentLease(t).RenewedAt.After(first) {
		t.Fatal("heartbeat did not renew the lease")
	}
}

func TestReleaseDeletesLease(t *testing.T) {
	store := newMemStore()
	m, err := Acquire(context.Background(), opts(store, t.TempDir()))
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	m.Release()
	if store.currentLease(t) != nil {
		t.Fatal("Release() did not delete the lease")
	}
}

func TestDegradedStartRequiresLocalMarker(t *testing.T) {
	downStore := newMemStore()
	downStore.failAll = true

	// A fresh standby with no marker must not start blind.
	if _, err := Acquire(context.Background(), opts(downStore, t.TempDir())); err == nil {
		t.Fatal("Acquire() with store down and no marker should fail")
	}

	// A node that held the lease before (marker present) may recover
	// degraded while the store is down.
	dir := t.TempDir()
	healthy := newMemStore()
	m, err := Acquire(context.Background(), opts(healthy, dir))
	if err != nil {
		t.Fatalf("Acquire(healthy) error = %v", err)
	}
	m.Release()

	if _, err := Acquire(context.Background(), opts(downStore, dir)); err != nil {
		t.Fatalf("Acquire(degraded with marker) error = %v, want success", err)
	}
}

// Made with Bob

// writeForeignLease replaces the lease in the store as though another
// gateway had taken over the bucket.
func writeForeignLease(t *testing.T, store *memStore, holder string) {
	t.Helper()
	payload, err := json.Marshal(Lease{HolderID: holder, Hostname: "other-host", Epoch: 99, AcquiredAt: time.Now(), RenewedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.objects[LeaseObjectKey] = payload
}

// Losing the lease to another gateway is reported, so the gateway can stop
// serving a bucket that is no longer its own.
func TestLeaseTakenIsReported(t *testing.T) {
	store := newMemStore()
	losses := make(chan LeaseLoss, 4)
	options := opts(store, t.TempDir())
	options.OnLeaseLost = func(loss LeaseLoss) { losses <- loss }

	m, err := Acquire(context.Background(), options)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer m.Release()

	writeForeignLease(t, store, "other-host-9999")

	select {
	case loss := <-losses:
		if loss.Reason != "taken" || loss.TakenBy == nil || loss.TakenBy.HolderID != "other-host-9999" {
			t.Fatalf("loss = %+v, want it taken by other-host-9999", loss)
		}
		if desc := loss.Description(); !strings.Contains(desc, "other-host-9999") {
			t.Errorf("Description() = %q, want it to name the holder", desc)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("losing the lease to another gateway was not reported")
	}

	// Reported once, not on every heartbeat after it.
	select {
	case loss := <-losses:
		t.Fatalf("the loss was reported twice: %+v", loss)
	case <-time.After(200 * time.Millisecond):
	}
}

// Heartbeats failing for longer than the lease timeout mean a standby may
// have promoted, so this node can no longer claim the bucket either.
func TestUnrenewableLeaseIsReported(t *testing.T) {
	store := newMemStore()
	losses := make(chan LeaseLoss, 4)
	options := opts(store, t.TempDir())
	options.OnLeaseLost = func(loss LeaseLoss) { losses <- loss }

	m, err := Acquire(context.Background(), options)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer m.Release()

	store.mu.Lock()
	store.failAll = true
	store.mu.Unlock()

	select {
	case loss := <-losses:
		if loss.Reason != "unreachable" || loss.Since < options.LeaseTimeout {
			t.Fatalf("loss = %+v, want it unreachable for at least the lease timeout", loss)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a lease that could not be renewed past its timeout was not reported")
	}
}

// A healthy gateway is never fenced.
func TestHealthyLeaseIsNotReportedLost(t *testing.T) {
	store := newMemStore()
	losses := make(chan LeaseLoss, 4)
	options := opts(store, t.TempDir())
	options.OnLeaseLost = func(loss LeaseLoss) { losses <- loss }

	m, err := Acquire(context.Background(), options)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer m.Release()

	select {
	case loss := <-losses:
		t.Fatalf("a healthy gateway was fenced: %+v", loss)
	case <-time.After(500 * time.Millisecond):
	}
}

// After a takeover the lease belongs to the gateway now serving the bucket:
// releasing must not delete it, or a third gateway could start.
func TestReleaseKeepsAnotherHoldersLease(t *testing.T) {
	store := newMemStore()
	m, err := Acquire(context.Background(), opts(store, t.TempDir()))
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	writeForeignLease(t, store, "other-host-1234")

	m.Release()

	current := store.currentLease(t)
	if current == nil {
		t.Fatal("Release() deleted a lease held by another gateway")
	}
	if current.HolderID != "other-host-1234" {
		t.Fatalf("lease holder = %q, want the other gateway's", current.HolderID)
	}
}
