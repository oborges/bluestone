package wire

import (
	"encoding/binary"
	"strconv"
	"strings"
)

// AdditionalInformation bits in a QUERY_INFO or SET_INFO for security
// (MS-SMB2 section 2.2.37), naming which parts of the descriptor the client
// is asking for.
const (
	OwnerSecurityInformation uint32 = 0x00000001
	GroupSecurityInformation uint32 = 0x00000002
	DACLSecurityInformation  uint32 = 0x00000004
	SACLSecurityInformation  uint32 = 0x00000008
	LabelSecurityInformation uint32 = 0x00000010
)

// Security descriptor control bits (MS-DTYP section 2.4.6).
const (
	seDACLPresent  uint16 = 0x0004
	seSACLPresent  uint16 = 0x0010
	seSelfRelative uint16 = 0x8000
)

// Access mask bits used in the descriptor (MS-DTYP section 2.4.3).
const (
	fileAllAccess uint32 = 0x001F01FF
	// fileReadExecute is FILE_GENERIC_READ and FILE_GENERIC_EXECUTE.
	fileReadExecute uint32 = 0x001200A9
)

const (
	accessAllowedACEType uint8 = 0x00
	aceObjectInheritFlag uint8 = 0x01
	aceContainerInherit  uint8 = 0x02
)

// Well-known SIDs used to describe files the gateway does not keep Windows
// ownership for (MS-DTYP section 2.4.2.4).
var (
	// sidEveryone is S-1-1-0.
	sidEveryone = sid{revision: 1, authority: 1, subAuthorities: []uint32{0}}
	// sidAdministrators is S-1-5-32-544, BUILTIN\Administrators.
	sidAdministrators = sid{revision: 1, authority: 5, subAuthorities: []uint32{32, 544}}
	// sidUsers is S-1-5-32-545, BUILTIN\Users.
	sidUsers = sid{revision: 1, authority: 5, subAuthorities: []uint32{32, 545}}
)

type sid struct {
	revision       uint8
	authority      uint64
	subAuthorities []uint32
}

func (s sid) len() int { return 8 + 4*len(s.subAuthorities) }

func (s sid) append(dst []byte) []byte {
	dst = append(dst, s.revision, uint8(len(s.subAuthorities)))
	// IdentifierAuthority is six bytes, big-endian.
	for shift := 40; shift >= 0; shift -= 8 {
		dst = append(dst, uint8(s.authority>>uint(shift)))
	}
	var u32 [4]byte
	for _, sub := range s.subAuthorities {
		binary.LittleEndian.PutUint32(u32[:], sub)
		dst = append(dst, u32[:]...)
	}
	return dst
}

// Descriptor is what a file's security descriptor describes.
type Descriptor struct {
	// IsDir marks a directory, whose ACE is inheritable so Windows shows
	// the permissions its children will have.
	IsDir bool
	// Owner and Group are the file's owner and group as SIDs, such as
	// "S-1-5-21-...-1105". Empty, or not a SID, selects
	// BUILTIN\Administrators and BUILTIN\Users.
	Owner, Group string
	// ReadOnly grants read access only, for a share its user may only read.
	ReadOnly bool
}

// SecurityDescriptor builds the self-relative security descriptor for a file,
// carrying the parts named in additional.
//
// The gateway does not keep Windows ACLs, so the DACL says plainly what is
// enforced: everyone has full access, or read access on a read-only share.
// Clients ask for this on open and to show a file's Security tab; answering
// STATUS_NOT_SUPPORTED instead makes Windows report that it cannot read the
// file's security information.
func SecurityDescriptor(additional uint32, d Descriptor) []byte {
	// A request naming nothing still gets an owner and a DACL: Windows asks
	// with AdditionalInformation zero in some paths and expects a
	// descriptor back.
	if additional&(OwnerSecurityInformation|GroupSecurityInformation|DACLSecurityInformation|SACLSecurityInformation) == 0 {
		additional |= OwnerSecurityInformation | DACLSecurityInformation
	}
	owner, ok := ParseSID(d.Owner)
	if !ok {
		owner = sidAdministrators
	}
	group, ok := ParseSID(d.Group)
	if !ok {
		group = sidUsers
	}
	mask := fileAllAccess
	if d.ReadOnly {
		mask = fileReadExecute
	}

	const headerSize = 20
	var control uint16 = seSelfRelative
	body := make([]byte, 0, 128)

	var ownerOffset, groupOffset, daclOffset uint32
	if additional&OwnerSecurityInformation != 0 {
		ownerOffset = uint32(headerSize + len(body))
		body = owner.append(body)
	}
	if additional&GroupSecurityInformation != 0 {
		groupOffset = uint32(headerSize + len(body))
		body = group.append(body)
	}
	if additional&DACLSecurityInformation != 0 {
		control |= seDACLPresent
		daclOffset = uint32(headerSize + len(body))
		body = appendAllowEveryoneACL(body, d.IsDir, mask)
	}
	// The gateway keeps no audit policy, so a request for the SACL gets a
	// descriptor that says there is none, rather than an error.
	if additional&SACLSecurityInformation != 0 {
		control |= seSACLPresent
	}

	out := make([]byte, headerSize, headerSize+len(body))
	out[0] = 1 // Revision
	out[1] = 0 // Sbz1
	binary.LittleEndian.PutUint16(out[2:4], control)
	binary.LittleEndian.PutUint32(out[4:8], ownerOffset)
	binary.LittleEndian.PutUint32(out[8:12], groupOffset)
	binary.LittleEndian.PutUint32(out[12:16], 0) // OffsetSacl: none
	binary.LittleEndian.PutUint32(out[16:20], daclOffset)
	return append(out, body...)
}

// appendAllowEveryoneACL writes an ACL with one ACE granting everyone mask.
// On a directory the ACE is inheritable, which is how Windows shows that
// new files under it get the same access.
func appendAllowEveryoneACL(dst []byte, isDir bool, mask uint32) []byte {
	aceSize := 8 + sidEveryone.len()
	aclSize := 8 + aceSize

	var flags uint8
	if isDir {
		flags = aceObjectInheritFlag | aceContainerInherit
	}

	start := len(dst)
	dst = append(dst, 2, 0) // AclRevision, Sbz1
	dst = append(dst, 0, 0) // AclSize, filled below
	dst = append(dst, 1, 0) // AceCount
	dst = append(dst, 0, 0) // Sbz2

	dst = append(dst, accessAllowedACEType, flags)
	var u16 [2]byte
	binary.LittleEndian.PutUint16(u16[:], uint16(aceSize))
	dst = append(dst, u16[:]...)
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], mask)
	dst = append(dst, u32[:]...)
	dst = sidEveryone.append(dst)

	binary.LittleEndian.PutUint16(dst[start+2:start+4], uint16(aclSize))
	return dst
}

// ParseSID parses a SID in its string form, S-1-<authority>-<sub>... It
// reports false for anything else.
func ParseSID(s string) (sid, bool) {
	parts := strings.Split(s, "-")
	if len(parts) < 3 || len(parts) > 3+15 || parts[0] != "S" || parts[1] != "1" {
		return sid{}, false
	}
	authority, err := strconv.ParseUint(parts[2], 10, 48)
	if err != nil {
		return sid{}, false
	}
	out := sid{revision: 1, authority: authority}
	for _, p := range parts[3:] {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return sid{}, false
		}
		out.subAuthorities = append(out.subAuthorities, uint32(v))
	}
	return out, true
}
