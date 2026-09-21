package smb

import (
	"math"
	"testing"

	"github.com/oborges/bluestone/internal/lock"
	"github.com/oborges/bluestone/internal/nfs"
	smbvfs "github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	gonfs "github.com/willscott/go-nfs"
)

func smbOwner(client, id string) smbvfs.LockOwner {
	return smbvfs.LockOwner{Client: client, ID: id}
}

// The point of the shared table: a lock taken over NFS is visible to SMB, and
// the other way round, so two clients on different protocols cannot both hold
// the same bytes.
func TestSMBLocksConflictWithNFSLocks(t *testing.T) {
	locks := lock.NewManager(lock.Options{})
	overSMB := NewLocker(locks)
	overNFS := nfs.NewLocker(locks)

	nfsOwner := gonfs.LockOwner{Client: "nfs4/01", Owner: "owner-a"}
	if conflict, err := overNFS.Lock(nfsOwner, "/shared.db", gonfs.LockRange{Start: 0, End: 100, Exclusive: true}); err != nil || conflict != nil {
		t.Fatalf("NFS Lock() = %v, %v; want it granted", conflict, err)
	}

	conflict, err := overSMB.Lock(smbOwner("smb/7", "handle-1"), "/shared.db",
		smbvfs.LockRange{Start: 50, End: 150, Exclusive: true})
	if err != nil {
		t.Fatalf("SMB Lock() error = %v", err)
	}
	if conflict == nil {
		t.Fatal("SMB Lock() was granted over bytes an NFS client holds")
	}
	if conflict.Owner.Client != "nfs4/01" {
		t.Fatalf("conflict owner = %+v, want the NFS holder", conflict.Owner)
	}

	// A range clear of the NFS lock is granted.
	if conflict, err := overSMB.Lock(smbOwner("smb/7", "handle-1"), "/shared.db",
		smbvfs.LockRange{Start: 100, End: 150, Exclusive: true}); err != nil || conflict != nil {
		t.Fatalf("SMB Lock() clear of the NFS lock = %v, %v; want it granted", conflict, err)
	}

	// Releasing the NFS lock frees the bytes for SMB.
	if err := overNFS.Unlock(nfsOwner, "/shared.db", gonfs.LockRange{Start: 0, End: 100, Exclusive: true}); err != nil {
		t.Fatalf("NFS Unlock() error = %v", err)
	}
	if conflict, err := overSMB.Lock(smbOwner("smb/7", "handle-2"), "/shared.db",
		smbvfs.LockRange{Start: 0, End: 100, Exclusive: true}); err != nil || conflict != nil {
		t.Fatalf("SMB Lock() after the NFS unlock = %v, %v; want it granted", conflict, err)
	}

	// And an SMB lock is equally visible to NFS.
	nfsConflict, err := overNFS.Lock(gonfs.LockOwner{Client: "nfs4/02", Owner: "owner-b"}, "/shared.db",
		gonfs.LockRange{Start: 0, End: 10, Exclusive: true})
	if err != nil {
		t.Fatalf("NFS Lock() error = %v", err)
	}
	if nfsConflict == nil {
		t.Fatal("NFS Lock() was granted over bytes an SMB client holds")
	}
}

func TestSMBLockerSharedAndExclusiveModes(t *testing.T) {
	locks := lock.NewManager(lock.Options{})
	locker := NewLocker(locks)
	const path = "/modes.txt"

	reader := smbOwner("smb/1", "handle-1")
	if conflict, err := locker.Lock(reader, path, smbvfs.LockRange{Start: 0, End: 100}); err != nil || conflict != nil {
		t.Fatalf("shared Lock() = %v, %v; want it granted", conflict, err)
	}
	// Another shared lock on the same bytes is fine.
	if conflict, err := locker.Lock(smbOwner("smb/2", "handle-2"), path, smbvfs.LockRange{Start: 0, End: 100}); err != nil || conflict != nil {
		t.Fatalf("second shared Lock() = %v, %v; want it granted", conflict, err)
	}
	// An exclusive lock over the same bytes is not.
	conflict, err := locker.Lock(smbOwner("smb/3", "handle-3"), path, smbvfs.LockRange{Start: 10, End: 20, Exclusive: true})
	if err != nil {
		t.Fatalf("exclusive Lock() error = %v", err)
	}
	if conflict == nil {
		t.Fatal("an exclusive lock was granted over shared-locked bytes")
	}
}

// Locks belong to the handle that took them: closing one handle releases its
// locks, and losing the session releases every handle's.
func TestSMBLockerReleasesByHandleAndSession(t *testing.T) {
	locks := lock.NewManager(lock.Options{})
	locker := NewLocker(locks)
	const path = "/release.txt"

	first := smbOwner("smb/9", "handle-1")
	second := smbOwner("smb/9", "handle-2")
	if _, err := locker.Lock(first, path, smbvfs.LockRange{Start: 0, End: 10, Exclusive: true}); err != nil {
		t.Fatalf("Lock(first) error = %v", err)
	}
	// A second handle of the same session still conflicts, as on Windows.
	conflict, err := locker.Lock(second, path, smbvfs.LockRange{Start: 0, End: 10, Exclusive: true})
	if err != nil {
		t.Fatalf("Lock(second) error = %v", err)
	}
	if conflict == nil {
		t.Fatal("a second handle in the same session was granted a conflicting lock")
	}

	locker.ReleaseOwner(first)
	if conflict, err := locker.Lock(second, path, smbvfs.LockRange{Start: 0, End: 10, Exclusive: true}); err != nil || conflict != nil {
		t.Fatalf("Lock(second) after releasing the first handle = %v, %v; want it granted", conflict, err)
	}

	locker.ReleaseClient("smb/9")
	if held := locks.Locks(path); len(held) != 0 {
		t.Fatalf("Locks() = %v, want none after the session ended", held)
	}
}

// A lock that runs to the end of the file covers everything after its start.
func TestSMBLockerLocksToEndOfFile(t *testing.T) {
	locks := lock.NewManager(lock.Options{})
	locker := NewLocker(locks)
	const path = "/tail.bin"

	if _, err := locker.Lock(smbOwner("smb/1", "handle-1"), path,
		smbvfs.LockRange{Start: 100, End: math.MaxUint64, Exclusive: true}); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	conflict, err := locker.Lock(smbOwner("smb/2", "handle-2"), path,
		smbvfs.LockRange{Start: 1 << 40, End: 1<<40 + 8, Exclusive: true})
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if conflict == nil {
		t.Fatal("a lock past the end of the locked region was granted")
	}
	if conflict, err := locker.Lock(smbOwner("smb/2", "handle-2"), path,
		smbvfs.LockRange{Start: 0, End: 100, Exclusive: true}); err != nil || conflict != nil {
		t.Fatalf("Lock() before the locked region = %v, %v; want it granted", conflict, err)
	}
}

// Locks meet on the file, however each protocol spells it: NFS names
// "docs/Report.txt" relative to its root, SMB "DOCS\report.TXT" with
// backslashes and in any case. Keyed by the raw names, locks on files in
// subdirectories never conflicted across the protocols.
func TestSMBAndNFSLocksMeetOnTheFile(t *testing.T) {
	g := startGateway(t)
	if err := g.share.MkdirAll(`docs`, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := g.share.WriteFile(`docs\Report.txt`, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	locks := lock.NewManager(lock.Options{})
	overSMB := NewLockerFor(locks, g.filesystem)
	overNFS := nfs.NewLocker(locks)

	nfsOwner := gonfs.LockOwner{Client: "nfs4/01", Owner: "owner-a"}
	if conflict, err := overNFS.Lock(nfsOwner, "docs/Report.txt", gonfs.LockRange{Start: 0, End: 10, Exclusive: true}); err != nil || conflict != nil {
		t.Fatalf("NFS Lock() = %v, %v", conflict, err)
	}
	conflict, err := overSMB.Lock(smbOwner("smb/7", "handle-1"), `DOCS\report.TXT`, smbvfs.LockRange{Start: 0, End: 10, Exclusive: true})
	if err != nil || conflict == nil {
		t.Fatalf("SMB Lock() on the same file, spelled the SMB way = %v, %v; want a conflict with the NFS lock", conflict, err)
	}
}
