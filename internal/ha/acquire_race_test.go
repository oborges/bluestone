package ha

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// hookStore is a memStore that lets a test act between the steps of Acquire.
type hookStore struct {
	*memStore
	// beforeGet and afterPut run on every read and after every successful
	// write of the lease.
	beforeGet func()
	afterPut  func()
	// failGets fails that many reads, counted from the first write on.
	failGets int
	put      bool
	mu       sync.Mutex
}

func (s *hookStore) GetObject(ctx context.Context, key string) ([]byte, error) {
	if s.beforeGet != nil {
		s.beforeGet()
	}
	s.mu.Lock()
	fail := s.put && s.failGets > 0
	if fail {
		s.failGets--
	}
	s.mu.Unlock()
	if fail {
		return nil, errStoreDown
	}
	return s.memStore.GetObject(ctx, key)
}

func (s *hookStore) PutObject(ctx context.Context, key string, data []byte, metadata map[string]string) error {
	if err := s.memStore.PutObject(ctx, key, data, metadata); err != nil {
		return err
	}
	s.mu.Lock()
	s.put = true
	s.mu.Unlock()
	if s.afterPut != nil {
		s.afterPut()
	}
	return nil
}

// barrier holds its first n callers until all of them have arrived, and lets
// every later one straight through.
type barrier struct {
	mu      sync.Mutex
	n       int
	arrived int
	open    chan struct{}
}

func newBarrier(n int) *barrier {
	return &barrier{n: n, open: make(chan struct{})}
}

func (b *barrier) wait() {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.open)
	}
	b.mu.Unlock()
	<-b.open
}

// Another gateway that wrote the lease after this one did owns the bucket:
// this one stands down at once, without waiting for a heartbeat to notice.
func TestAcquireStandsDownWhenAnotherGatewayWroteLast(t *testing.T) {
	mem := newMemStore()
	var once sync.Once
	store := &hookStore{memStore: mem, afterPut: func() {
		once.Do(func() { writeForeignLease(t, mem, "other-host-9-ffff") })
	}}
	dir := t.TempDir()

	m, err := Acquire(context.Background(), opts(store, dir))
	var held *ErrLeaseHeld
	if !errors.As(err, &held) {
		if m != nil {
			m.Release()
		}
		t.Fatalf("Acquire() error = %v, want ErrLeaseHeld", err)
	}
	if held.Lease.HolderID != "other-host-9-ffff" {
		t.Fatalf("lease reported held by %s, want the gateway that wrote last", held.Lease.HolderID)
	}
	if lease := mem.currentLease(t); lease == nil || lease.HolderID != "other-host-9-ffff" {
		t.Fatalf("lease in the bucket = %+v, want the other gateway's left alone", lease)
	}
	// The marker lets a node restart blind during an outage; a node that
	// never held the lease must not have one.
	if _, err := os.Stat(filepath.Join(dir, "ha-holder-marker")); !os.IsNotExist(err) {
		t.Fatalf("holder marker written by a gateway that lost the lease (stat error = %v)", err)
	}
}

// Two gateways that start together both find the lease free and both write
// it. Only the one whose write the bucket kept may serve.
func TestSimultaneousAcquireLeavesOneHolder(t *testing.T) {
	mem := newMemStore()
	reads, writes := newBarrier(2), newBarrier(2)
	store := &hookStore{memStore: mem, beforeGet: reads.wait, afterPut: writes.wait}

	managers := make([]*Manager, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range managers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			managers[i], errs[i] = Acquire(context.Background(), opts(store, t.TempDir()))
		}()
	}
	wg.Wait()

	var winner *Manager
	for i, m := range managers {
		if errs[i] != nil {
			var held *ErrLeaseHeld
			if !errors.As(errs[i], &held) {
				t.Fatalf("Acquire() error = %v, want ErrLeaseHeld", errs[i])
			}
			continue
		}
		defer m.Release()
		if winner != nil {
			t.Fatal("both gateways acquired the lease")
		}
		winner = m
	}
	if winner == nil {
		t.Fatalf("neither gateway acquired the lease: %v, %v", errs[0], errs[1])
	}
	if lease := mem.currentLease(t); lease == nil || lease.HolderID != winner.HolderID() {
		t.Fatalf("lease in the bucket = %+v, want the winner's (%s)", lease, winner.HolderID())
	}
}

// A read-back that fails says nothing about who holds the lease; the gateway
// that just wrote it starts, and its heartbeat does the checking.
func TestAcquireToleratesFailedReadBack(t *testing.T) {
	store := &hookStore{memStore: newMemStore(), failGets: 1}
	m, err := Acquire(context.Background(), opts(store, t.TempDir()))
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer m.Release()
	if lease := store.currentLease(t); lease == nil || lease.HolderID != m.HolderID() {
		t.Fatalf("lease in the bucket = %+v, want this gateway's", lease)
	}
}
