package wire

import (
	"encoding/binary"
	"testing"
)

// parsed is a security descriptor taken apart the way a client does.
type parsed struct {
	revision    uint8
	control     uint16
	owner       string
	group       string
	daclPresent bool
	aceCount    int
	aceFlags    uint8
	aceMask     uint32
	aceSID      string
}

func parseDescriptor(t *testing.T, sd []byte) parsed {
	t.Helper()
	if len(sd) < 20 {
		t.Fatalf("descriptor is %d bytes, too short for a header", len(sd))
	}
	p := parsed{revision: sd[0], control: binary.LittleEndian.Uint16(sd[2:4])}
	ownerOffset := binary.LittleEndian.Uint32(sd[4:8])
	groupOffset := binary.LittleEndian.Uint32(sd[8:12])
	saclOffset := binary.LittleEndian.Uint32(sd[12:16])
	daclOffset := binary.LittleEndian.Uint32(sd[16:20])
	if saclOffset != 0 {
		t.Errorf("SACL offset = %d, want 0: the gateway keeps no audit policy", saclOffset)
	}
	if ownerOffset != 0 {
		p.owner = readSID(t, sd, ownerOffset)
	}
	if groupOffset != 0 {
		p.group = readSID(t, sd, groupOffset)
	}
	if daclOffset != 0 {
		p.daclPresent = true
		acl := sd[daclOffset:]
		if len(acl) < 8 {
			t.Fatal("ACL shorter than its header")
		}
		if size := binary.LittleEndian.Uint16(acl[2:4]); int(size) > len(acl) {
			t.Fatalf("AclSize = %d, past the end of the descriptor (%d bytes left)", size, len(acl))
		}
		p.aceCount = int(binary.LittleEndian.Uint16(acl[4:6]))
		if p.aceCount > 0 {
			ace := acl[8:]
			if len(ace) < 8 {
				t.Fatal("ACE shorter than its header")
			}
			p.aceFlags = ace[1]
			p.aceMask = binary.LittleEndian.Uint32(ace[4:8])
			p.aceSID = readSID(t, ace, 8)
		}
	}
	return p
}

// readSID renders a SID as S-1-<authority>-<sub>… the way Windows writes it.
func readSID(t *testing.T, b []byte, offset uint32) string {
	t.Helper()
	if int(offset)+8 > len(b) {
		t.Fatalf("SID at %d is past the end", offset)
	}
	s := b[offset:]
	subCount := int(s[1])
	var authority uint64
	for _, v := range s[2:8] {
		authority = authority<<8 | uint64(v)
	}
	out := "S-" + itoa(uint64(s[0])) + "-" + itoa(authority)
	for i := 0; i < subCount; i++ {
		start := 8 + 4*i
		if start+4 > len(s) {
			t.Fatalf("SID sub-authority %d is past the end", i)
		}
		out += "-" + itoa(uint64(binary.LittleEndian.Uint32(s[start:start+4])))
	}
	return out
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// Windows reads the descriptor to show a file's Security tab, so it has to
// be a well-formed self-relative descriptor with the parts asked for.
func TestSecurityDescriptorStructure(t *testing.T) {
	sd := SecurityDescriptor(OwnerSecurityInformation|GroupSecurityInformation|DACLSecurityInformation, false)
	got := parseDescriptor(t, sd)

	if got.revision != 1 {
		t.Errorf("revision = %d, want 1", got.revision)
	}
	if got.control&seSelfRelative == 0 {
		t.Errorf("control = %#x, want the self-relative bit", got.control)
	}
	if got.control&seDACLPresent == 0 {
		t.Errorf("control = %#x, want the DACL-present bit", got.control)
	}
	if got.owner != "S-1-5-32-544" {
		t.Errorf("owner = %s, want BUILTIN\\Administrators", got.owner)
	}
	if got.group != "S-1-5-32-545" {
		t.Errorf("group = %s, want BUILTIN\\Users", got.group)
	}
	if got.aceCount != 1 || got.aceSID != "S-1-1-0" || got.aceMask != fileAllAccess {
		t.Errorf("ACE = %d entries, sid %s, mask %#x; want one full-access entry for Everyone",
			got.aceCount, got.aceSID, got.aceMask)
	}
	if got.aceFlags != 0 {
		t.Errorf("ACE flags on a file = %#x, want none: only a directory's ACE is inheritable", got.aceFlags)
	}
}

// A directory's ACE is inheritable, which is how Windows shows that files
// created under it get the same access.
func TestSecurityDescriptorDirectoryInherits(t *testing.T) {
	got := parseDescriptor(t, SecurityDescriptor(DACLSecurityInformation, true))
	if got.aceFlags&(aceObjectInheritFlag|aceContainerInherit) != (aceObjectInheritFlag | aceContainerInherit) {
		t.Errorf("ACE flags on a directory = %#x, want object and container inherit", got.aceFlags)
	}
}

// Only the parts the client asked for come back.
func TestSecurityDescriptorHonoursAdditionalInformation(t *testing.T) {
	owner := parseDescriptor(t, SecurityDescriptor(OwnerSecurityInformation, false))
	if owner.owner == "" {
		t.Error("owner-only request returned no owner")
	}
	if owner.group != "" {
		t.Errorf("owner-only request returned group %s", owner.group)
	}
	if owner.daclPresent {
		t.Error("owner-only request returned a DACL")
	}

	dacl := parseDescriptor(t, SecurityDescriptor(DACLSecurityInformation, false))
	if dacl.owner != "" || dacl.group != "" {
		t.Errorf("DACL-only request returned owner %q and group %q", dacl.owner, dacl.group)
	}
	if !dacl.daclPresent {
		t.Error("DACL-only request returned no DACL")
	}

	// Asking for the SACL says there is none rather than failing.
	sacl := parseDescriptor(t, SecurityDescriptor(SACLSecurityInformation, false))
	if sacl.control&seSACLPresent == 0 {
		t.Errorf("control = %#x, want the SACL-present bit", sacl.control)
	}

	// Some clients ask for nothing in particular and still expect a
	// descriptor.
	empty := parseDescriptor(t, SecurityDescriptor(0, false))
	if empty.owner == "" || !empty.daclPresent {
		t.Errorf("request with no parts named returned owner %q, dacl %v; want both", empty.owner, empty.daclPresent)
	}
}
