// Package lock is the protocol-neutral byte-range lock table shared by the
// file protocol servers, so a lock taken over one protocol conflicts with
// locks taken over another.
//
// Locks are advisory and in-memory: this package never enforces them against
// reads or writes, and they do not survive a restart. Protocol layers keep
// their own state around locks (NFSv4 stateids and client leases, SMB
// handles) and map their holders onto Owners.
package lock

import (
	"errors"
	"math"
	pathpkg "path"
	"sort"
	"strings"
	"sync"
)

// EOF as a range end means "to end of file".
const EOF = math.MaxUint64

// Default caps bound lock state per file and per client, so a misbehaving or
// leaky client cannot grow server memory without bound.
const (
	DefaultMaxLocksPerFile   = 512
	DefaultMaxLocksPerClient = 8192
)

var (
	// ErrLimit reports that granting a lock would exceed a per-file or
	// per-client cap. Nothing is granted.
	ErrLimit = errors.New("lock: limit exceeded")
	// ErrInvalid reports an empty or inverted range, or an unknown mode.
	ErrInvalid = errors.New("lock: invalid range or mode")
)

// Mode is a lock's sharing mode.
type Mode uint8

const (
	// Shared locks coexist with other owners' shared locks (POSIX read locks).
	Shared Mode = iota + 1
	// Exclusive locks conflict with every other owner's lock on overlapping
	// bytes (POSIX write locks).
	Exclusive
)

// Owner identifies a lock holder. Requests with equal Owners come from the
// same holder and never conflict with each other.
type Owner struct {
	// Client is the protocol client holding the lock, such as an NFSv4 client
	// ID. Per-client caps and ReleaseClient apply to it.
	Client string
	// ID distinguishes holders within a client, such as an NFSv4 lock_owner.
	ID string
}

// Range is the byte range [Start, End); End == EOF extends to end of file.
type Range struct {
	Start, End uint64
}

func (r Range) overlaps(o Range) bool {
	return r.Start < o.End && o.Start < r.End
}

// Lock is a held range.
type Lock struct {
	Range
	Mode  Mode
	Owner Owner
}

// Options configures a Manager. Zero values select the defaults.
type Options struct {
	MaxLocksPerFile   int
	MaxLocksPerClient int
}

type held struct {
	Range
	mode Mode
}

// Manager is the lock table. It is safe for concurrent use.
type Manager struct {
	mu                sync.Mutex
	maxLocksPerFile   int
	maxLocksPerClient int

	// files maps a path to each owner's held ranges on it, sorted by start.
	files map[string]map[Owner][]held
	// owned indexes the paths each owner holds ranges on.
	owned map[Owner]map[string]struct{}
	// clientLocks counts held ranges per client for the client cap.
	clientLocks map[string]int
}

// NewManager returns an empty lock table.
func NewManager(opts Options) *Manager {
	m := &Manager{
		maxLocksPerFile:   opts.MaxLocksPerFile,
		maxLocksPerClient: opts.MaxLocksPerClient,
		files:             make(map[string]map[Owner][]held),
		owned:             make(map[Owner]map[string]struct{}),
		clientLocks:       make(map[string]int),
	}
	if m.maxLocksPerFile <= 0 {
		m.maxLocksPerFile = DefaultMaxLocksPerFile
	}
	if m.maxLocksPerClient <= 0 {
		m.maxLocksPerClient = DefaultMaxLocksPerClient
	}
	return m
}

// Lock grants mode on r to owner. The owner's own overlapping ranges on path
// are replaced, so a holder can upgrade, downgrade, or split its locks. If
// another owner holds a conflicting range, Lock returns that lock and grants
// nothing. Granting is atomic: on ErrLimit the owner's ranges are unchanged.
// Key is the form paths take in the lock table: the file's key path, from
// the bucket root, without a leading slash. Protocols name files their own
// way (SMB with backslashes and any case, NFS relative to its root), and
// convert to this form so a lock taken over one conflicts with one taken over
// the other on the same file.
func Key(p string) string {
	return strings.TrimPrefix(pathpkg.Clean("/"+p), "/")
}

func (m *Manager) Lock(owner Owner, path string, r Range, mode Mode) (*Lock, error) {
	if err := validate(r, mode); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if conflict := m.conflictLocked(owner, path, r, mode); conflict != nil {
		return conflict, nil
	}
	before := m.files[path][owner]
	after := coalesce(append(subtract(before, r), held{Range: r, mode: mode}))
	delta := len(after) - len(before)
	if delta > 0 && (m.fileLockCountLocked(path)+delta > m.maxLocksPerFile ||
		m.clientLocks[owner.Client]+delta > m.maxLocksPerClient) {
		return nil, ErrLimit
	}
	m.setLocked(owner, path, after, delta)
	return nil, nil
}

// Unlock releases r from owner's ranges on path, splitting ranges that
// straddle it. Releasing bytes the owner does not hold is not an error.
func (m *Manager) Unlock(owner Owner, path string, r Range) error {
	if r.Start >= r.End {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	before := m.files[path][owner]
	after := subtract(before, r)
	m.setLocked(owner, path, after, len(after)-len(before))
	return nil
}

// Test returns the lock another owner holds that would conflict with owner
// locking r in mode, or nil. It grants nothing.
func (m *Manager) Test(owner Owner, path string, r Range, mode Mode) (*Lock, error) {
	if err := validate(r, mode); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conflictLocked(owner, path, r, mode), nil
}

// ReleaseOwner drops every range owner holds, on any path.
func (m *Manager) ReleaseOwner(owner Owner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseOwnerLocked(owner)
}

// ReleaseClient drops every range held by any owner of client, as when a
// client's lease expires or its connection is gone.
func (m *Manager) ReleaseClient(client string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for owner := range m.owned {
		if owner.Client == client {
			m.releaseOwnerLocked(owner)
		}
	}
}

// Locks returns the locks held on path, ordered by start offset.
func (m *Manager) Locks(path string) []Lock {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Lock
	for owner, ranges := range m.files[path] {
		for _, h := range ranges {
			out = append(out, Lock{Range: h.Range, Mode: h.mode, Owner: owner})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return lessOwner(out[i].Owner, out[j].Owner)
	})
	return out
}

func validate(r Range, mode Mode) error {
	if r.Start >= r.End || (mode != Shared && mode != Exclusive) {
		return ErrInvalid
	}
	return nil
}

// conflictLocked returns the conflicting lock with the lowest start held by
// another owner, so denials describe the same lock every time.
func (m *Manager) conflictLocked(owner Owner, path string, r Range, mode Mode) *Lock {
	var found *Lock
	for other, ranges := range m.files[path] {
		if other == owner {
			continue
		}
		for _, h := range ranges {
			if !h.overlaps(r) || (h.mode == Shared && mode == Shared) {
				continue
			}
			if found == nil || h.Start < found.Start ||
				(h.Start == found.Start && lessOwner(other, found.Owner)) {
				found = &Lock{Range: h.Range, Mode: h.mode, Owner: other}
			}
		}
	}
	return found
}

func (m *Manager) fileLockCountLocked(path string) int {
	count := 0
	for _, ranges := range m.files[path] {
		count += len(ranges)
	}
	return count
}

// setLocked stores owner's ranges on path and keeps the indexes and client
// count in step; delta is the change in the owner's range count.
func (m *Manager) setLocked(owner Owner, path string, ranges []held, delta int) {
	owners := m.files[path]
	if len(ranges) == 0 {
		delete(owners, owner)
		if len(owners) == 0 {
			delete(m.files, path)
		}
		if paths := m.owned[owner]; paths != nil {
			delete(paths, path)
			if len(paths) == 0 {
				delete(m.owned, owner)
			}
		}
	} else {
		if owners == nil {
			owners = make(map[Owner][]held)
			m.files[path] = owners
		}
		owners[owner] = ranges
		if m.owned[owner] == nil {
			m.owned[owner] = make(map[string]struct{})
		}
		m.owned[owner][path] = struct{}{}
	}

	if n := m.clientLocks[owner.Client] + delta; n > 0 {
		m.clientLocks[owner.Client] = n
	} else {
		delete(m.clientLocks, owner.Client)
	}
}

func (m *Manager) releaseOwnerLocked(owner Owner) {
	for path := range m.owned[owner] {
		m.setLocked(owner, path, nil, -len(m.files[path][owner]))
	}
}

// subtract returns ranges with sub removed, splitting ranges that straddle
// it. It never modifies its input, which keeps a refused Lock atomic.
func subtract(ranges []held, sub Range) []held {
	out := make([]held, 0, len(ranges)+1)
	for _, h := range ranges {
		if !h.overlaps(sub) {
			out = append(out, h)
			continue
		}
		if h.Start < sub.Start {
			out = append(out, held{Range: Range{Start: h.Start, End: sub.Start}, mode: h.mode})
		}
		if sub.End < h.End {
			out = append(out, held{Range: Range{Start: sub.End, End: h.End}, mode: h.mode})
		}
	}
	return out
}

// coalesce sorts ranges by start and merges adjacent or overlapping ranges of
// the same mode, bounding growth from repeated small locks.
func coalesce(ranges []held) []held {
	if len(ranges) < 2 {
		return ranges
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	out := ranges[:1]
	for _, h := range ranges[1:] {
		last := &out[len(out)-1]
		if h.mode == last.mode && h.Start <= last.End {
			if h.End > last.End {
				last.End = h.End
			}
			continue
		}
		out = append(out, h)
	}
	return out
}

func lessOwner(a, b Owner) bool {
	if a.Client != b.Client {
		return a.Client < b.Client
	}
	return a.ID < b.ID
}
