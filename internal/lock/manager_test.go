package lock

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

var (
	alice = Owner{Client: "nfs4/1", ID: "alice"}
	bob   = Owner{Client: "nfs4/2", ID: "bob"}
)

func mustLock(t *testing.T, m *Manager, owner Owner, path string, start, end uint64, mode Mode) {
	t.Helper()
	conflict, err := m.Lock(owner, path, Range{Start: start, End: end}, mode)
	if err != nil || conflict != nil {
		t.Fatalf("Lock(%v, %s, [%d,%d), mode %d) = %+v, %v; want granted", owner, path, start, end, mode, conflict, err)
	}
}

func assertLocks(t *testing.T, m *Manager, path string, want []Lock) {
	t.Helper()
	if got := m.Locks(path); !reflect.DeepEqual(got, want) {
		t.Fatalf("Locks(%s) = %+v, want %+v", path, got, want)
	}
}

func TestLockConflictsBetweenOwners(t *testing.T) {
	m := NewManager(Options{})
	mustLock(t, m, alice, "/f", 0, 100, Exclusive)

	conflict, err := m.Lock(bob, "/f", Range{Start: 50, End: 150}, Exclusive)
	want := Lock{Range: Range{Start: 0, End: 100}, Mode: Exclusive, Owner: alice}
	if err != nil || conflict == nil || *conflict != want {
		t.Fatalf("overlapping exclusive lock = %+v, %v; want conflict %+v", conflict, err, want)
	}
	if conflict, _ := m.Lock(bob, "/f", Range{Start: 0, End: 10}, Shared); conflict == nil {
		t.Fatal("shared lock over another owner's exclusive lock must conflict")
	}
	mustLock(t, m, bob, "/f", 200, 300, Exclusive)

	mustLock(t, m, alice, "/g", 0, 10, Shared)
	mustLock(t, m, bob, "/g", 0, 10, Shared)

	// Refused requests granted nothing.
	assertLocks(t, m, "/f", []Lock{
		{Range: Range{Start: 0, End: 100}, Mode: Exclusive, Owner: alice},
		{Range: Range{Start: 200, End: 300}, Mode: Exclusive, Owner: bob},
	})
}

func TestConflictReportsLowestHeldRange(t *testing.T) {
	m := NewManager(Options{})
	mustLock(t, m, alice, "/f", 40, 50, Exclusive)
	mustLock(t, m, alice, "/f", 10, 20, Exclusive)

	for i := 0; i < 20; i++ {
		conflict, err := m.Test(bob, "/f", Range{Start: 0, End: EOF}, Shared)
		if err != nil || conflict == nil || conflict.Start != 10 {
			t.Fatalf("Test() = %+v, %v; want the [10,20) lock every time", conflict, err)
		}
	}
}

func TestSameOwnerReplacesAndUnlockSplits(t *testing.T) {
	m := NewManager(Options{})
	mustLock(t, m, alice, "/f", 0, 100, Exclusive)
	// Downgrading the middle replaces the owner's own overlap.
	mustLock(t, m, alice, "/f", 25, 75, Shared)
	assertLocks(t, m, "/f", []Lock{
		{Range: Range{Start: 0, End: 25}, Mode: Exclusive, Owner: alice},
		{Range: Range{Start: 25, End: 75}, Mode: Shared, Owner: alice},
		{Range: Range{Start: 75, End: 100}, Mode: Exclusive, Owner: alice},
	})

	if err := m.Unlock(alice, "/f", Range{Start: 25, End: 75}); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	assertLocks(t, m, "/f", []Lock{
		{Range: Range{Start: 0, End: 25}, Mode: Exclusive, Owner: alice},
		{Range: Range{Start: 75, End: 100}, Mode: Exclusive, Owner: alice},
	})
	mustLock(t, m, bob, "/f", 30, 40, Exclusive)
}

func TestUnlockOfUnheldRangeIsNoop(t *testing.T) {
	m := NewManager(Options{})
	if err := m.Unlock(alice, "/f", Range{Start: 0, End: 10}); err != nil {
		t.Fatalf("Unlock(unheld) error = %v", err)
	}
	mustLock(t, m, bob, "/f", 0, 10, Exclusive)
	if err := m.Unlock(alice, "/f", Range{Start: 0, End: 10}); err != nil {
		t.Fatalf("Unlock(other owner's range) error = %v", err)
	}
	assertLocks(t, m, "/f", []Lock{{Range: Range{Start: 0, End: 10}, Mode: Exclusive, Owner: bob}})
}

func TestTestExcludesRequestingOwner(t *testing.T) {
	m := NewManager(Options{})
	mustLock(t, m, alice, "/f", 0, 100, Exclusive)

	if conflict, err := m.Test(alice, "/f", Range{Start: 0, End: 100}, Exclusive); err != nil || conflict != nil {
		t.Fatalf("self Test() = %+v, %v; want no conflict", conflict, err)
	}
	if conflict, _ := m.Test(bob, "/f", Range{Start: 0, End: 100}, Exclusive); conflict == nil {
		t.Fatal("foreign Test() must report the conflict")
	}
	if got := len(m.Locks("/f")); got != 1 {
		t.Fatalf("Test() must not grant: %d locks", got)
	}
}

func TestReleaseOwnerAndClient(t *testing.T) {
	m := NewManager(Options{})
	aliceOther := Owner{Client: alice.Client, ID: "alice-2"}
	mustLock(t, m, alice, "/f", 0, 10, Exclusive)
	mustLock(t, m, alice, "/g", 0, 10, Exclusive)
	mustLock(t, m, aliceOther, "/h", 0, 10, Exclusive)
	mustLock(t, m, bob, "/f", 20, 30, Exclusive)

	m.ReleaseOwner(alice)
	assertLocks(t, m, "/g", nil)
	assertLocks(t, m, "/f", []Lock{{Range: Range{Start: 20, End: 30}, Mode: Exclusive, Owner: bob}})
	assertLocks(t, m, "/h", []Lock{{Range: Range{Start: 0, End: 10}, Mode: Exclusive, Owner: aliceOther}})

	m.ReleaseClient(alice.Client)
	assertLocks(t, m, "/h", nil)
	assertLocks(t, m, "/f", []Lock{{Range: Range{Start: 20, End: 30}, Mode: Exclusive, Owner: bob}})
}

func TestPerFileLimit(t *testing.T) {
	m := NewManager(Options{})
	// Disjoint one-byte locks with gaps cannot coalesce.
	for i := 0; i < DefaultMaxLocksPerFile; i++ {
		mustLock(t, m, alice, "/f", uint64(i*2), uint64(i*2+1), Exclusive)
	}
	if _, err := m.Lock(bob, "/f", Range{Start: 100000, End: 100001}, Exclusive); !errors.Is(err, ErrLimit) {
		t.Fatalf("lock beyond per-file cap error = %v, want ErrLimit", err)
	}
	mustLock(t, m, bob, "/g", 0, 1, Exclusive)
}

func TestLimitRefusalIsAtomic(t *testing.T) {
	m := NewManager(Options{MaxLocksPerFile: 3})
	mustLock(t, m, alice, "/f", 0, 10, Exclusive)
	mustLock(t, m, alice, "/f", 20, 30, Exclusive)
	mustLock(t, m, alice, "/f", 40, 50, Exclusive)
	before := m.Locks("/f")

	// Splitting [0,10) around a shared byte would need two more ranges.
	if _, err := m.Lock(alice, "/f", Range{Start: 5, End: 6}, Shared); !errors.Is(err, ErrLimit) {
		t.Fatalf("splitting lock error = %v, want ErrLimit", err)
	}
	assertLocks(t, m, "/f", before)

	// A replacement that does not grow the table is always allowed.
	mustLock(t, m, alice, "/f", 0, 30, Exclusive)
	if got := len(m.Locks("/f")); got != 2 {
		t.Fatalf("locks after merge = %d, want 2", got)
	}
}

func TestPerClientLimitFollowsReleases(t *testing.T) {
	m := NewManager(Options{MaxLocksPerClient: 1})
	mustLock(t, m, alice, "/f", 0, 10, Exclusive)
	aliceOther := Owner{Client: alice.Client, ID: "alice-2"}
	if _, err := m.Lock(aliceOther, "/g", Range{Start: 0, End: 10}, Exclusive); !errors.Is(err, ErrLimit) {
		t.Fatalf("second lock for the client error = %v, want ErrLimit", err)
	}
	mustLock(t, m, bob, "/g", 0, 10, Exclusive)

	if err := m.Unlock(alice, "/f", Range{Start: 0, End: 10}); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	mustLock(t, m, aliceOther, "/h", 0, 10, Exclusive)
	m.ReleaseOwner(aliceOther)
	mustLock(t, m, alice, "/f", 0, 10, Exclusive)
}

func TestCoalesceMergesAdjacentSameModeRanges(t *testing.T) {
	m := NewManager(Options{})
	mustLock(t, m, alice, "/f", 0, 10, Exclusive)
	mustLock(t, m, alice, "/f", 10, 20, Exclusive)
	mustLock(t, m, alice, "/f", 20, 30, Exclusive)
	assertLocks(t, m, "/f", []Lock{{Range: Range{Start: 0, End: 30}, Mode: Exclusive, Owner: alice}})
}

func TestInvalidRequests(t *testing.T) {
	m := NewManager(Options{})
	for _, tc := range []struct {
		r    Range
		mode Mode
	}{
		{Range{Start: 5, End: 5}, Exclusive},
		{Range{Start: 10, End: 5}, Shared},
		{Range{Start: 0, End: 10}, 0},
	} {
		if _, err := m.Lock(alice, "/f", tc.r, tc.mode); !errors.Is(err, ErrInvalid) {
			t.Errorf("Lock(%+v, mode %d) error = %v, want ErrInvalid", tc.r, tc.mode, err)
		}
		if _, err := m.Test(alice, "/f", tc.r, tc.mode); !errors.Is(err, ErrInvalid) {
			t.Errorf("Test(%+v, mode %d) error = %v, want ErrInvalid", tc.r, tc.mode, err)
		}
	}
	if err := m.Unlock(alice, "/f", Range{Start: 3, End: 3}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Unlock(empty) error = %v, want ErrInvalid", err)
	}
}

func TestConcurrentOwners(t *testing.T) {
	m := NewManager(Options{})
	const workers, rounds = 16, 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			owner := Owner{Client: fmt.Sprintf("client-%d", w), ID: "o"}
			r := Range{Start: uint64(w * 100), End: uint64(w*100 + 50)}
			for i := 0; i < rounds; i++ {
				if conflict, err := m.Lock(owner, "/shared", r, Exclusive); err != nil || conflict != nil {
					t.Errorf("worker %d Lock() = %+v, %v", w, conflict, err)
					return
				}
				if _, err := m.Test(owner, "/shared", Range{Start: 0, End: EOF}, Shared); err != nil {
					t.Errorf("worker %d Test() error = %v", w, err)
					return
				}
				if err := m.Unlock(owner, "/shared", r); err != nil {
					t.Errorf("worker %d Unlock() error = %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	assertLocks(t, m, "/shared", nil)
}
