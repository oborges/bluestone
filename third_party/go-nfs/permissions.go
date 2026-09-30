package nfs

import (
	"bytes"
	"errors"
	"os"

	"github.com/go-git/go-billy/v5"
)

// Permissions makes the server enforce POSIX file permissions, the way a
// kernel NFS server does: each call acts as the user its AUTH_SYS
// credentials name, and may only do what a file's mode, owner and group let
// that user do. Without it (Server.Permissions nil), every caller may do
// anything the filesystem allows.
//
// A few conventions follow Linux's nfsd:
//   - A file's owner may always read and write its data. NFS has no open
//     file handles to carry the permission a process was granted when it
//     opened a file, so a process that created a read-only file, or
//     changed its mode after opening it, can go on writing it.
//   - Reading a file needs read or execute permission, so a client can run
//     a program it may execute but not read.
//   - Only root may give a file away; an owner may change its group to one
//     of their own groups.
//   - Files and directories a caller creates are theirs.
type Permissions struct {
	// RootSquash makes calls from root act as the anonymous user.
	RootSquash bool
	// AnonUID and AnonGID are who calls without AUTH_SYS credentials, and
	// squashed root, act as. Zero selects 65534 (nobody).
	AnonUID, AnonGID uint32
}

// DefaultAnonID is the uid and gid anonymous callers act as by default.
const DefaultAnonID = 65534

// Credentials are who a call acts as.
type Credentials struct {
	UID, GID uint32
	// Groups are the caller's supplementary groups.
	Groups []uint32
}

// Permission bits a caller may need on a file.
const (
	mayRead  uint32 = 4
	mayWrite uint32 = 2
	mayExec  uint32 = 1
)

// authUnix is the RPC flavor AUTH_SYS (RFC 5531 appendix A).
const authUnix = 1

// credentials returns who a call acts as, and whether permissions are
// enforced at all.
func credentials(w *response) (Credentials, bool) {
	if w == nil || w.conn == nil || w.conn.Server == nil || w.conn.Server.Permissions == nil || w.req == nil {
		return Credentials{}, false
	}
	p := w.conn.Server.Permissions
	anon := Credentials{UID: p.AnonUID, GID: p.AnonGID}
	if anon.UID == 0 {
		anon.UID = DefaultAnonID
	}
	if anon.GID == 0 {
		anon.GID = DefaultAnonID
	}
	if w.req.Header.Cred.Flavor != authUnix {
		return anon, true
	}
	c, err := parseAuthUnix(w.req.Header.Cred.Body)
	if err != nil {
		return anon, true
	}
	if p.RootSquash {
		if c.UID == 0 {
			c.UID = anon.UID
		}
		if c.GID == 0 {
			c.GID = anon.GID
		}
		for i, g := range c.Groups {
			if g == 0 {
				c.Groups[i] = anon.GID
			}
		}
	}
	return c, true
}

// authSysParms is an authsys_parms body. The machine name is padded to
// four bytes, which xdr.ReadOpaque would not skip; decoding the struct
// does.
type authSysParms struct {
	Stamp   uint32
	Machine string
	UID     uint32
	GID     uint32
	Groups  []uint32
}

// parseAuthUnix reads an authsys_parms body: stamp, machine name, uid, gid
// and up to 16 supplementary groups.
func parseAuthUnix(body []byte) (Credentials, error) {
	var p authSysParms
	// 255 bounds the machine name; the group list is checked below.
	if err := xdrReadLimited(bytes.NewReader(body), &p, 255); err != nil {
		return Credentials{}, err
	}
	if len(p.Groups) > 16 {
		return Credentials{}, errors.New("nfs: AUTH_SYS lists more than 16 groups")
	}
	return Credentials{UID: p.UID, GID: p.GID, Groups: p.Groups}, nil
}

func (c Credentials) inGroup(gid uint32) bool {
	if c.GID == gid {
		return true
	}
	for _, g := range c.Groups {
		if g == gid {
			return true
		}
	}
	return false
}

// may reports whether c has every permission in want on a file.
func (c Credentials) may(attr *FileAttribute, want uint32) bool {
	perm := uint32(attr.Mode().Perm())
	if c.UID == 0 {
		// Root may do anything, except execute a file nobody may execute.
		return want&mayExec == 0 || attr.Type == FileTypeDirectory || perm&0o111 != 0
	}
	var bits uint32
	switch {
	case c.UID == attr.UID:
		bits = perm >> 6 & 7
	case c.inGroup(attr.GID):
		bits = perm >> 3 & 7
	default:
		bits = perm & 7
	}
	return bits&want == want
}

// errAccess is the error for a permission the caller lacks.
func errAccess() error {
	return &NFSStatusError{NFSStatusAccess, os.ErrPermission}
}

// errPerm is the error for something only the owner or root may do.
func errPerm() error {
	return &NFSStatusError{NFSStatusPerm, os.ErrPermission}
}

// checkAccess returns an error unless the caller of w has want on the file
// at path, or permissions are not enforced. A file that cannot be read is
// left for the operation itself to report.
func checkAccess(w *response, fs billy.Filesystem, path []string, want uint32) error {
	c, enforced := credentials(w)
	if !enforced {
		return nil
	}
	attr := tryStat(fs, path)
	if attr == nil || c.may(attr, want) {
		return nil
	}
	return errAccess()
}

// checkData returns an error unless the caller of w may read (or write)
// the data of the file at path: the permission itself, or being its owner.
// Reading is also allowed with execute permission.
func checkData(w *response, fs billy.Filesystem, path []string, want uint32) error {
	c, enforced := credentials(w)
	if !enforced {
		return nil
	}
	attr := tryStat(fs, path)
	if attr == nil || c.UID == attr.UID || c.may(attr, want) {
		return nil
	}
	if want == mayRead && c.may(attr, mayExec) {
		return nil
	}
	return errAccess()
}

// checkParent returns an error unless the caller of w may add or remove
// entries in the directory holding path: write and execute on it.
func checkParent(w *response, fs billy.Filesystem, path []string) error {
	if len(path) == 0 {
		return nil
	}
	return checkAccess(w, fs, path[:len(path)-1], mayWrite|mayExec)
}

// checkSetAttr returns an error unless the caller of w may make the changes
// s asks for to the file at path.
func checkSetAttr(w *response, fs billy.Filesystem, path []string, s *SetFileAttributes) error {
	c, enforced := credentials(w)
	if !enforced || s == nil {
		return nil
	}
	attr := tryStat(fs, path)
	if attr == nil || c.UID == 0 {
		return nil
	}
	owner := c.UID == attr.UID
	if s.SetMode != nil && !owner && uint32(os.FileMode(*s.SetMode)&os.ModePerm) != uint32(attr.Mode().Perm()) {
		return errPerm()
	}
	if s.SetUID != nil && *s.SetUID != attr.UID {
		return errPerm()
	}
	if s.SetGID != nil && *s.SetGID != attr.GID && (!owner || !c.inGroup(*s.SetGID)) {
		return errPerm()
	}
	if s.SetSize != nil && !owner && !c.may(attr, mayWrite) {
		return errAccess()
	}
	if s.SetAtime != nil || s.SetMtime != nil {
		// Setting times to now needs write permission; setting them to
		// anything else needs ownership.
		if !owner && !(s.TimesToServer && c.may(attr, mayWrite)) {
			if s.TimesToServer {
				return errAccess()
			}
			return errPerm()
		}
	}
	return nil
}

// claimCreated makes the caller of w the owner of a file it just created,
// when permissions are enforced.
func claimCreated(w *response, changer billy.Change, fs billy.Filesystem, fullPath string) error {
	c, enforced := credentials(w)
	if !enforced || changer == nil {
		return nil
	}
	info, err := fs.Lstat(fullPath)
	if err != nil {
		return nil
	}
	attr := ToFileAttribute(info, fullPath)
	if attr.UID == c.UID && attr.GID == c.GID {
		return nil
	}
	return changer.Lchown(fullPath, int(c.UID), int(c.GID))
}

// accessMask returns which of the ACCESS bits asked for the caller of w
// has on the file at path. Bits are those of NFSv3 ACCESS, which NFSv4
// shares: read 0x1, lookup 0x2, modify 0x4, extend 0x8, delete 0x10,
// execute 0x20.
func accessMask(w *response, fs billy.Filesystem, path []string, asked uint32) uint32 {
	c, enforced := credentials(w)
	if !enforced {
		return asked
	}
	attr := tryStat(fs, path)
	if attr == nil {
		return 0
	}
	var granted uint32
	if c.may(attr, mayRead) {
		granted |= 0x1
	}
	if c.may(attr, mayWrite) {
		granted |= 0x4 | 0x8
	}
	if attr.Type == FileTypeDirectory {
		if c.may(attr, mayExec) {
			granted |= 0x2
		}
		if c.may(attr, mayWrite|mayExec) {
			granted |= 0x10
		}
	} else if c.may(attr, mayExec) {
		granted |= 0x20
	}
	return asked & granted
}

// checkOpen returns an error unless the caller of w may open the existing
// file at path for what an NFSv4 OPEN asks: read (or execute, to run a
// program) and write, by the file's mode.
func checkOpen(w *response, fs billy.Filesystem, path []string, read, write bool) error {
	c, enforced := credentials(w)
	if !enforced {
		return nil
	}
	attr := tryStat(fs, path)
	if attr == nil {
		return nil
	}
	if read && !c.may(attr, mayRead) && !c.may(attr, mayExec) {
		return errAccess()
	}
	if write && !c.may(attr, mayWrite) {
		return errAccess()
	}
	return nil
}
