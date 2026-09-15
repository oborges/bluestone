package nfs

import (
	"errors"
	"testing"

	"github.com/oborges/bluestone/internal/lock"
	nfs "github.com/willscott/go-nfs"
)

func TestLockerSharesLocksAcrossProtocols(t *testing.T) {
	locks := lock.NewManager(lock.Options{})
	nfsLocker := NewLocker(locks)

	// Another protocol server holds an exclusive range on the same file.
	smbOwner := lock.Owner{Client: "smb/session-1", ID: "handle-7"}
	if conflict, err := locks.Lock(smbOwner, "/f", lock.Range{Start: 0, End: 100}, lock.Exclusive); err != nil || conflict != nil {
		t.Fatalf("SMB-side Lock() = %+v, %v", conflict, err)
	}

	nfsOwner := nfs.LockOwner{Client: "nfs4/1", Owner: "o"}
	want := nfs.LockConflict{
		LockRange: nfs.LockRange{Start: 0, End: 100, Exclusive: true},
		Owner:     nfs.LockOwner{Client: "smb/session-1", Owner: "handle-7"},
	}
	conflict, err := nfsLocker.Lock(nfsOwner, "/f", nfs.LockRange{Start: 50, End: 60})
	if err != nil || conflict == nil || *conflict != want {
		t.Fatalf("NFS Lock() over SMB range = %+v, %v; want %+v", conflict, err, want)
	}
	if conflict, err := nfsLocker.Test(nfsOwner, "/f", nfs.LockRange{Start: 99, End: 200}); err != nil || conflict == nil || *conflict != want {
		t.Fatalf("NFS Test() = %+v, %v; want %+v", conflict, err, want)
	}

	// Disjoint ranges coexist; the NFS lock is visible to the other side.
	if conflict, err := nfsLocker.Lock(nfsOwner, "/f", nfs.LockRange{Start: 100, End: 200, Exclusive: true}); err != nil || conflict != nil {
		t.Fatalf("NFS disjoint Lock() = %+v, %v", conflict, err)
	}
	if conflict, _ := locks.Test(smbOwner, "/f", lock.Range{Start: 150, End: 151}, lock.Shared); conflict == nil || conflict.Owner != (lock.Owner{Client: "nfs4/1", ID: "o"}) {
		t.Fatalf("SMB-side Test() = %+v, want the NFS lock", conflict)
	}

	// Once the other protocol releases, NFS can take the range.
	locks.ReleaseClient("smb/session-1")
	if conflict, err := nfsLocker.Lock(nfsOwner, "/f", nfs.LockRange{Start: 50, End: 60}); err != nil || conflict != nil {
		t.Fatalf("NFS Lock() after release = %+v, %v", conflict, err)
	}

	if err := nfsLocker.Unlock(nfsOwner, "/f", nfs.LockRange{Start: 0, End: 1000}); err != nil {
		t.Fatalf("NFS Unlock() error = %v", err)
	}
	if got := locks.Locks("/f"); len(got) != 0 {
		t.Fatalf("locks after unlock = %+v, want none", got)
	}
}

func TestLockerMapsLimitAndReleases(t *testing.T) {
	locks := lock.NewManager(lock.Options{MaxLocksPerClient: 1})
	nfsLocker := NewLocker(locks)
	owner := nfs.LockOwner{Client: "nfs4/1", Owner: "o"}

	if _, err := nfsLocker.Lock(owner, "/f", nfs.LockRange{Start: 0, End: 10}); err != nil {
		t.Fatalf("first Lock() error = %v", err)
	}
	if _, err := nfsLocker.Lock(owner, "/g", nfs.LockRange{Start: 0, End: 10}); !errors.Is(err, nfs.ErrLockLimit) {
		t.Fatalf("Lock() over client cap error = %v, want nfs.ErrLockLimit", err)
	}

	nfsLocker.ReleaseOwner(owner)
	if _, err := nfsLocker.Lock(owner, "/g", nfs.LockRange{Start: 0, End: 10}); err != nil {
		t.Fatalf("Lock() after ReleaseOwner error = %v", err)
	}
	nfsLocker.ReleaseClient("nfs4/1")
	if got := locks.Locks("/g"); len(got) != 0 {
		t.Fatalf("locks after ReleaseClient = %+v, want none", got)
	}
}
