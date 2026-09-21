package smb

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/oborges/bluestone/internal/posix"
	"github.com/sonroyaalmerol/go-smb-server/smb/auth"
	"github.com/sonroyaalmerol/go-smb-server/smb/server"
)

// IDMap maps who a session authenticated as to the uid and gid the gateway
// records for the files it creates, which NFS clients see, and those ids
// back to the SIDs Windows shows as a file's owner.
//
// Domain accounts map by relative ID, as Samba's idmap_rid does: the
// account S-1-5-21-a-b-c-1105 is uid Base+1105, and its primary group gid
// Base+513. Local accounts use the ids they are configured with, and show in
// Windows as Samba's Unix SIDs, S-1-22-1-<uid> and S-1-22-2-<gid>.
type IDMap struct {
	// DomainSID is the Active Directory domain whose accounts are mapped.
	// Empty maps none.
	DomainSID string
	// Base is the uid and gid of relative ID 0.
	Base int
	// Local maps local account names, lower-cased, to their ids.
	Local map[string]LocalIDs
}

// LocalIDs are a local account's uid and gid.
type LocalIDs struct {
	UID, GID int
}

// active reports whether the map maps anything; without it files keep the
// default owner and descriptors say the gateway keeps no Windows owners.
func (m *IDMap) active() bool {
	return m != nil && (m.DomainSID != "" || len(m.Local) > 0)
}

// owner returns the uid and gid to record for files id creates, or false to
// leave the default owner.
func (m *IDMap) owner(id *auth.Identity) (uid, gid int, ok bool) {
	if m == nil || id == nil {
		return 0, 0, false
	}
	if id.Mechanism == auth.MechanismNTLM {
		ids, ok := m.Local[strings.ToLower(id.Username)]
		if !ok || ids.UID == 0 {
			return 0, 0, false
		}
		gid = ids.GID
		if gid == 0 {
			gid = posix.DefaultGID
		}
		return ids.UID, gid, true
	}
	rid, ok := m.rid(id.SID)
	if !ok {
		return 0, 0, false
	}
	gid = posix.DefaultGID
	if g, ok := m.rid(id.PrimaryGroup); ok {
		gid = m.Base + g
	}
	return m.Base + rid, gid, true
}

// rid returns a SID's relative ID if the SID is in the mapped domain.
func (m *IDMap) rid(sid string) (int, bool) {
	if m.DomainSID == "" {
		return 0, false
	}
	rest, ok := strings.CutPrefix(strings.ToUpper(sid), strings.ToUpper(m.DomainSID)+"-")
	if !ok {
		return 0, false
	}
	rid, err := strconv.ParseUint(rest, 10, 32)
	if err != nil || int(rid) > 1<<31-1-m.Base {
		return 0, false
	}
	return int(rid), true
}

// sids returns the SIDs of a file's owner and group, or empty strings when
// the map is not in use, so the server describes the file as it always has.
func (m *IDMap) sids(uid, gid int) (owner, group string) {
	if !m.active() {
		return "", ""
	}
	return m.sid(uid, "1"), m.sid(gid, "2")
}

// sid names one id: a domain account or group when it is in the domain's
// range, a Unix user (kind "1") or group ("2") otherwise.
func (m *IDMap) sid(id int, kind string) string {
	if m.DomainSID != "" && id >= m.Base {
		return fmt.Sprintf("%s-%d", m.DomainSID, id-m.Base)
	}
	return fmt.Sprintf("S-1-22-%s-%d", kind, id)
}

// ShareOptions are one share: a directory of the bucket, and who may use it.
type ShareOptions struct {
	// Name is the share's name.
	Name string
	// Path is the directory of the bucket it serves; "" or "/" serves the
	// whole bucket.
	Path string
	// ReadOnly, ValidUsers, ReadList and WriteList are as in
	// config.SMBShare.
	ReadOnly   bool
	ValidUsers []string
	ReadList   []string
	WriteList  []string
}

// accessRules decide what a user may do on each share.
type accessRules struct {
	// domain is the name the gateway gives its local accounts' domain.
	domain string
	// groups maps local accounts, lower-cased, to their groups,
	// lower-cased.
	groups map[string][]string
	shares map[string]ShareOptions
}

func newAccessRules(domain string, users []User, shares []ShareOptions) *accessRules {
	r := &accessRules{domain: domain, groups: map[string][]string{}, shares: map[string]ShareOptions{}}
	for _, u := range users {
		for _, g := range u.Groups {
			r.groups[strings.ToLower(u.Name)] = append(r.groups[strings.ToLower(u.Name)], strings.ToLower(g))
		}
	}
	for _, s := range shares {
		r.shares[strings.ToLower(s.Name)] = s
	}
	return r
}

// access implements the server's share access hook.
func (r *accessRules) access(id *auth.Identity, share string) server.ShareAccess {
	s, ok := r.shares[strings.ToLower(share)]
	if !ok {
		// IPC$, which every signed-in user needs to list the shares.
		return server.ShareReadWrite
	}
	if id == nil || (len(s.ValidUsers) > 0 && !r.matchesAny(id, s.ValidUsers)) {
		return server.ShareDenied
	}
	readOnly := s.ReadOnly || r.matchesAny(id, s.ReadList)
	if r.matchesAny(id, s.WriteList) {
		readOnly = false
	}
	if readOnly {
		return server.ShareReadOnly
	}
	return server.ShareReadWrite
}

func (r *accessRules) matchesAny(id *auth.Identity, entries []string) bool {
	for _, e := range entries {
		if r.matches(id, e) {
			return true
		}
	}
	return false
}

// matches reports whether an access list entry names id: a SID of the
// account or one of its groups, "@group" for a local account's group, or
// the account's name, with or without its domain.
//
// An NTLM client names its own domain, which proves nothing, so a local
// account matches "DOMAIN\user" only for the gateway's own domain name. A
// Kerberos account's domain comes from its ticket.
func (r *accessRules) matches(id *auth.Identity, entry string) bool {
	e := strings.TrimSpace(entry)
	if strings.HasPrefix(strings.ToUpper(e), "S-1-") {
		for _, g := range append([]string{id.SID, id.PrimaryGroup}, id.Groups...) {
			if strings.EqualFold(e, g) {
				return true
			}
		}
		return false
	}
	if group, ok := strings.CutPrefix(e, "@"); ok {
		if id.Mechanism != auth.MechanismNTLM {
			return false
		}
		for _, g := range r.groups[strings.ToLower(id.Username)] {
			if g == strings.ToLower(group) {
				return true
			}
		}
		return false
	}
	domain, user, qualified := strings.Cut(e, `\`)
	if !qualified {
		return strings.EqualFold(e, id.Username)
	}
	if !strings.EqualFold(user, id.Username) {
		return false
	}
	if id.Mechanism == auth.MechanismNTLM {
		return strings.EqualFold(domain, r.domain)
	}
	return strings.EqualFold(domain, id.Domain)
}
