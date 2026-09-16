package smb

import (
	"github.com/oborges/bluestone/internal/lock"
	smbvfs "github.com/sonroyaalmerol/go-smb-server/smb/vfs"
)

// locker adapts the shared lock table to the SMB server, so a byte-range lock
// taken over SMB conflicts with one taken over NFS on the same bytes.
type locker struct {
	locks *lock.Manager
}

// NewLocker returns an SMB byte-range locker backed by the shared table.
func NewLocker(locks *lock.Manager) smbvfs.ByteRangeLocker {
	return locker{locks: locks}
}

func (l locker) Lock(owner smbvfs.LockOwner, path string, r smbvfs.LockRange) (*smbvfs.LockConflict, error) {
	// A reached cap or an invalid range returns an error, which the server
	// answers with STATUS_LOCK_NOT_GRANTED, the same as a conflict.
	conflict, err := l.locks.Lock(lockOwner(owner), path, lockRange(r), lockMode(r))
	if err != nil {
		return nil, err
	}
	return smbConflict(conflict), nil
}

func (l locker) Unlock(owner smbvfs.LockOwner, path string, r smbvfs.LockRange) error {
	return l.locks.Unlock(lockOwner(owner), path, lockRange(r))
}

func (l locker) ReleaseOwner(owner smbvfs.LockOwner) {
	l.locks.ReleaseOwner(lockOwner(owner))
}

func (l locker) ReleaseClient(client string) {
	l.locks.ReleaseClient(client)
}

func lockOwner(o smbvfs.LockOwner) lock.Owner {
	return lock.Owner{Client: o.Client, ID: o.ID}
}

func lockRange(r smbvfs.LockRange) lock.Range {
	return lock.Range{Start: r.Start, End: r.End}
}

func lockMode(r smbvfs.LockRange) lock.Mode {
	if r.Exclusive {
		return lock.Exclusive
	}
	return lock.Shared
}

func smbConflict(c *lock.Lock) *smbvfs.LockConflict {
	if c == nil {
		return nil
	}
	return &smbvfs.LockConflict{
		LockRange: smbvfs.LockRange{Start: c.Start, End: c.End, Exclusive: c.Mode == lock.Exclusive},
		Owner:     smbvfs.LockOwner{Client: c.Owner.Client, ID: c.Owner.ID},
	}
}
