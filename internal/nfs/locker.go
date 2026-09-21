package nfs

import (
	"errors"

	"github.com/oborges/bluestone/internal/lock"
	nfs "github.com/willscott/go-nfs"
)

// locker adapts the shared lock table to go-nfs, so NFSv4 byte-range locks
// live in the table every protocol server uses.
type locker struct {
	locks *lock.Manager
}

// NewLocker returns a go-nfs byte-range locker backed by the shared table.
func NewLocker(locks *lock.Manager) nfs.ByteRangeLocker {
	return locker{locks: locks}
}

func (l locker) Lock(owner nfs.LockOwner, path string, r nfs.LockRange) (*nfs.LockConflict, error) {
	conflict, err := l.locks.Lock(lockOwner(owner), lock.Key(path), lockRange(r), lockMode(r))
	if errors.Is(err, lock.ErrLimit) {
		return nil, nfs.ErrLockLimit
	}
	if err != nil {
		return nil, err
	}
	return nfsConflict(conflict), nil
}

func (l locker) Unlock(owner nfs.LockOwner, path string, r nfs.LockRange) error {
	return l.locks.Unlock(lockOwner(owner), lock.Key(path), lockRange(r))
}

func (l locker) Test(owner nfs.LockOwner, path string, r nfs.LockRange) (*nfs.LockConflict, error) {
	conflict, err := l.locks.Test(lockOwner(owner), lock.Key(path), lockRange(r), lockMode(r))
	if err != nil {
		return nil, err
	}
	return nfsConflict(conflict), nil
}

func (l locker) ReleaseOwner(owner nfs.LockOwner) {
	l.locks.ReleaseOwner(lockOwner(owner))
}

func (l locker) ReleaseClient(client string) {
	l.locks.ReleaseClient(client)
}

func lockOwner(o nfs.LockOwner) lock.Owner {
	return lock.Owner{Client: o.Client, ID: o.Owner}
}

func lockRange(r nfs.LockRange) lock.Range {
	return lock.Range{Start: r.Start, End: r.End}
}

func lockMode(r nfs.LockRange) lock.Mode {
	if r.Exclusive {
		return lock.Exclusive
	}
	return lock.Shared
}

func nfsConflict(c *lock.Lock) *nfs.LockConflict {
	if c == nil {
		return nil
	}
	return &nfs.LockConflict{
		LockRange: nfs.LockRange{Start: c.Start, End: c.End, Exclusive: c.Mode == lock.Exclusive},
		Owner:     nfs.LockOwner{Client: c.Owner.Client, Owner: c.Owner.ID},
	}
}
