package lock

import (
	"errors"
	"sync"
	"testing"
)

const allShare = AccessRead | AccessWrite | AccessDelete

func TestShareTableConflicts(t *testing.T) {
	tests := []struct {
		name                  string
		heldAccess, heldShare Access
		wantAccess, wantShare Access
		wantViolation         bool
	}{
		{
			name:       "a reader that shares reads admits another reader",
			heldAccess: AccessRead, heldShare: AccessRead,
			wantAccess: AccessRead, wantShare: AccessRead,
		},
		{
			name:       "a reader that shares reads refuses a writer",
			heldAccess: AccessRead, heldShare: AccessRead,
			wantAccess: AccessWrite, wantShare: allShare,
			wantViolation: true,
		},
		{
			name:       "a writer that shares nothing refuses a reader",
			heldAccess: AccessWrite, heldShare: 0,
			wantAccess: AccessRead, wantShare: allShare,
			wantViolation: true,
		},
		{
			name:       "a writer that shares everything admits a reader",
			heldAccess: AccessWrite, heldShare: allShare,
			wantAccess: AccessRead, wantShare: allShare,
		},
		{
			name:       "a new open that shares nothing refuses to join an existing one",
			heldAccess: AccessRead, heldShare: allShare,
			wantAccess: AccessRead, wantShare: 0,
			wantViolation: true,
		},
		{
			name:       "two writers that share writes coexist",
			heldAccess: AccessWrite, heldShare: AccessWrite,
			wantAccess: AccessWrite, wantShare: AccessWrite,
		},
		{
			name:       "deleting needs the other open to share deletes",
			heldAccess: AccessRead, heldShare: AccessRead | AccessWrite,
			wantAccess: AccessDelete, wantShare: allShare,
			wantViolation: true,
		},
		{
			name:       "deleting is allowed when deletes are shared",
			heldAccess: AccessRead, heldShare: allShare,
			wantAccess: AccessDelete, wantShare: allShare,
		},
		{
			name:       "an open needing nothing still has to tolerate the holder",
			heldAccess: AccessWrite, heldShare: allShare,
			wantAccess: 0, wantShare: 0,
			wantViolation: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table := NewShareTable(ShareOptions{})
			held, err := table.Acquire("/f", tt.heldAccess, tt.heldShare)
			if err != nil {
				t.Fatalf("first Acquire() error = %v", err)
			}

			second, err := table.Acquire("/f", tt.wantAccess, tt.wantShare)
			if tt.wantViolation {
				if !errors.Is(err, ErrSharingViolation) {
					t.Fatalf("second Acquire() error = %v, want a sharing violation", err)
				}
			} else if err != nil {
				t.Fatalf("second Acquire() error = %v, want it granted", err)
			}

			// Releasing the first open frees the file either way.
			held.Release()
			second.Release()
			third, err := table.Acquire("/f", tt.wantAccess, tt.wantShare)
			if err != nil {
				t.Fatalf("Acquire() after release error = %v", err)
			}
			third.Release()
			if opens := table.Opens("/f"); len(opens) != 0 {
				t.Fatalf("Opens() = %v, want none left", opens)
			}
		})
	}
}

func TestShareTableIsolatesPathsAndRepeatsRelease(t *testing.T) {
	table := NewShareTable(ShareOptions{})

	exclusive, err := table.Acquire("/one", AccessWrite, 0)
	if err != nil {
		t.Fatalf("Acquire(/one) error = %v", err)
	}
	other, err := table.Acquire("/two", AccessWrite, 0)
	if err != nil {
		t.Fatalf("Acquire(/two) error = %v", err)
	}
	if _, err := table.Acquire("/one", AccessRead, allShare); !errors.Is(err, ErrSharingViolation) {
		t.Fatalf("second Acquire(/one) error = %v, want a sharing violation", err)
	}

	exclusive.Release()
	exclusive.Release() // releasing twice must not disturb the table
	if _, err := table.Acquire("/one", AccessRead, allShare); err != nil {
		t.Fatalf("Acquire(/one) after release error = %v", err)
	}
	if opens := table.Opens("/two"); len(opens) != 1 {
		t.Fatalf("Opens(/two) = %v, want the untouched open", opens)
	}
	other.Release()
}

func TestShareTableRenameCarriesOpens(t *testing.T) {
	table := NewShareTable(ShareOptions{})
	held, err := table.Acquire("/old.txt", AccessWrite, 0)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	table.Rename("/old.txt", "/new.txt")
	if opens := table.Opens("/new.txt"); len(opens) != 1 {
		t.Fatalf("Opens(/new.txt) = %v, want the moved open", opens)
	}
	if opens := table.Opens("/old.txt"); len(opens) != 0 {
		t.Fatalf("Opens(/old.txt) = %v, want none", opens)
	}
	if _, err := table.Acquire("/new.txt", AccessRead, allShare); !errors.Is(err, ErrSharingViolation) {
		t.Fatalf("Acquire(/new.txt) error = %v, want the moved open to still conflict", err)
	}

	held.Release()
	if opens := table.Opens("/new.txt"); len(opens) != 0 {
		t.Fatalf("Opens(/new.txt) after release = %v, want none", opens)
	}
}

func TestShareTableCapsOpensPerFile(t *testing.T) {
	table := NewShareTable(ShareOptions{MaxOpensPerFile: 2})
	for i := range 2 {
		if _, err := table.Acquire("/f", AccessRead, allShare); err != nil {
			t.Fatalf("Acquire() %d error = %v", i, err)
		}
	}
	if _, err := table.Acquire("/f", AccessRead, allShare); !errors.Is(err, ErrLimit) {
		t.Fatalf("Acquire() past the cap error = %v, want ErrLimit", err)
	}
}

func TestShareTableIsSafeForConcurrentUse(t *testing.T) {
	table := NewShareTable(ShareOptions{})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 50 {
				handle, err := table.Acquire("/shared", AccessRead, allShare)
				if err != nil {
					t.Errorf("Acquire() error = %v", err)
					return
				}
				table.Opens("/shared")
				handle.Release()
			}
		}(i)
	}
	wg.Wait()
	if opens := table.Opens("/shared"); len(opens) != 0 {
		t.Fatalf("Opens() = %v, want none left", opens)
	}
}
