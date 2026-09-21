package smb

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"testing"

	"github.com/oborges/bluestone/internal/lock"
	"github.com/oborges/bluestone/internal/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/auth"
	"github.com/sonroyaalmerol/go-smb-server/smb/server"
	"go.uber.org/zap"
)

const testDomainSID = "S-1-5-21-3167651404-3865080224-2280184895"

// domainUser is a Kerberos identity of the test domain, as the PAC gives it.
func domainUser(name string, rid int, groups ...string) *auth.Identity {
	return &auth.Identity{
		Username: name, Domain: "CORP", Mechanism: auth.MechanismKerberos,
		SID:          testDomainSID + "-" + strconv.Itoa(rid),
		PrimaryGroup: testDomainSID + "-513",
		Groups:       append([]string{testDomainSID + "-513"}, groups...),
	}
}

func localUser(name, domain string) *auth.Identity {
	return &auth.Identity{Username: name, Domain: domain, Mechanism: auth.MechanismNTLM}
}

func TestAccessRules(t *testing.T) {
	engineering := testDomainSID + "-1108"
	rules := newAccessRules("BLUESTONE",
		[]User{{Name: "carol", Groups: []string{"Auditors"}}},
		[]ShareOptions{
			{Name: "projects", ValidUsers: []string{engineering, `CORP\bob`, "@auditors"}, ReadList: []string{"@auditors", `CORP\bob`}},
			{Name: "public", ReadOnly: true, WriteList: []string{engineering}},
			{Name: "open"},
		})
	alice := domainUser("alice", 1105, engineering)
	bob := domainUser("bob", 1106)
	dave := domainUser("dave", 1107)
	carol := localUser("carol", "WHATEVER")
	// A local account claiming to be in the domain is not bob.
	fakeBob := localUser("bob", "CORP")

	for _, tc := range []struct {
		who   string
		id    *auth.Identity
		share string
		want  server.ShareAccess
	}{
		{"domain group member", alice, "projects", server.ShareReadWrite},
		{"named domain user on the read list", bob, "projects", server.ShareReadOnly},
		{"domain user not listed", dave, "projects", server.ShareDenied},
		{"local account in a listed group", carol, "projects", server.ShareReadOnly},
		{"local account naming the domain", fakeBob, "projects", server.ShareDenied},
		{"read-only share", dave, "public", server.ShareReadOnly},
		{"write list on a read-only share", alice, "public", server.ShareReadWrite},
		{"share without lists", carol, "open", server.ShareReadWrite},
		{"share names ignore case", alice, "PROJECTS", server.ShareReadWrite},
		{"IPC$", dave, "IPC$", server.ShareReadWrite},
		{"no identity", nil, "open", server.ShareDenied},
	} {
		if got := rules.access(tc.id, tc.share); got != tc.want {
			t.Errorf("%s: access to %s = %d, want %d", tc.who, tc.share, got, tc.want)
		}
	}
	// A local account's own domain is the gateway's.
	if !rules.matches(localUser("carol", "anything"), `BLUESTONE\carol`) {
		t.Error(`BLUESTONE\carol does not match the local account carol`)
	}
}

func TestIDMap(t *testing.T) {
	m := &IDMap{DomainSID: testDomainSID, Base: 100000, Local: map[string]LocalIDs{"carol": {UID: 2001, GID: 2002}}}
	if uid, gid, ok := m.owner(domainUser("alice", 1105)); !ok || uid != 101105 || gid != 100513 {
		t.Errorf("domain user maps to %d:%d (%v), want 101105:100513", uid, gid, ok)
	}
	foreign := domainUser("eve", 1105)
	foreign.SID = "S-1-5-21-1-2-3-1105"
	if _, _, ok := m.owner(foreign); ok {
		t.Error("an account of another domain was mapped")
	}
	if uid, gid, ok := m.owner(localUser("CAROL", "x")); !ok || uid != 2001 || gid != 2002 {
		t.Errorf("local user maps to %d:%d (%v), want 2001:2002", uid, gid, ok)
	}
	if _, _, ok := m.owner(localUser("nobody", "x")); ok {
		t.Error("a local account without ids was mapped")
	}
	if owner, group := m.sids(101105, 100513); owner != testDomainSID+"-1105" || group != testDomainSID+"-513" {
		t.Errorf("domain ids name %s and %s", owner, group)
	}
	if owner, group := m.sids(2001, 2002); owner != "S-1-22-1-2001" || group != "S-1-22-2-2002" {
		t.Errorf("local ids name %s and %s, want Unix SIDs", owner, group)
	}
	var none *IDMap
	if owner, group := none.sids(1000, 1000); owner != "" || group != "" {
		t.Error("no id map should leave the server's default owner")
	}
}

// Shares serve directories of the bucket: each sees only its own, the
// directory is created when missing, and a user sees only the shares the
// rules give them, with the access they give.
func TestSMBSharesAndAccess(t *testing.T) {
	g := startGateway(t, func(o *ServerOptions) {
		o.Users = []User{
			{Name: "alice", Password: "secret", UID: 2001, GID: 2002},
			{Name: "bob", Password: "hunter2", Groups: []string{"readers"}},
		}
		o.Shares = []ShareOptions{
			{Name: "projects", Path: "/projects", ReadList: []string{"@readers"}},
			{Name: "hr", Path: "/hr", ValidUsers: []string{"alice"}},
		}
		o.IDMap = &IDMap{Local: map[string]LocalIDs{"alice": {UID: 2001, GID: 2002}}}
	})
	for _, dir := range []string{"/projects", "/hr"} {
		if info, err := g.filesystem.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("share directory %s was not created: %v", dir, err)
		}
	}

	projects, err := g.mountShare(t, "projects", "alice", "secret", "WORKGROUP")
	if err != nil {
		t.Fatalf("alice mounting projects: %v", err)
	}
	if err := projects.WriteFile("plan.txt", []byte("ship it"), 0o644); err != nil {
		t.Fatalf("alice writing to projects: %v", err)
	}
	info, err := g.filesystem.Stat("/projects/plan.txt")
	if err != nil {
		t.Fatalf("the file is not in the share's directory: %v", err)
	}
	if attrs := vfs.FileAttributes(info); attrs.UID != 2001 || attrs.GID != 2002 {
		t.Errorf("file created by alice is owned by %d:%d, want 2001:2002", attrs.UID, attrs.GID)
	}
	if _, err := g.filesystem.Stat("/plan.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the file landed at the bucket's root too: %v", err)
	}

	hr, err := g.mountShare(t, "hr", "alice", "secret", "WORKGROUP")
	if err != nil {
		t.Fatalf("alice mounting hr: %v", err)
	}
	if _, err := hr.Stat("plan.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("hr sees projects' file: %v", err)
	}
	if _, err := g.mountShare(t, "hr", "bob", "hunter2", "WORKGROUP"); err == nil {
		t.Error("bob mounted hr, which only alice may use")
	}

	readOnly, err := g.mountShare(t, "projects", "bob", "hunter2", "WORKGROUP")
	if err != nil {
		t.Fatalf("bob mounting projects: %v", err)
	}
	if data, err := readOnly.ReadFile("plan.txt"); err != nil || string(data) != "ship it" {
		t.Errorf("bob reading projects: %q, %v", data, err)
	}
	if err := readOnly.WriteFile("bob.txt", []byte("x"), 0o644); err == nil {
		t.Error("bob wrote to projects, where he may only read")
	}
	if err := readOnly.Remove("plan.txt"); err == nil {
		t.Error("bob deleted from projects, where he may only read")
	}
	if _, err := g.filesystem.Stat("/projects/plan.txt"); err != nil {
		t.Errorf("plan.txt is gone after bob's attempts: %v", err)
	}

	// The owner shows as a Unix SID in the file's security descriptor.
	backend := newShareBackend(g.filesystem, "/projects", nil, zap.NewNop())
	backend.ids = &IDMap{Local: map[string]LocalIDs{"alice": {UID: 2001, GID: 2002}}}
	h := backend.newHandle("/plan.txt", false)
	fi, err := h.Stat(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if fi.OwnerSID != "S-1-22-1-2001" || fi.GroupSID != "S-1-22-2-2002" {
		t.Errorf("owner %s group %s, want S-1-22-1-2001 and S-1-22-2-2002", fi.OwnerSID, fi.GroupSID)
	}
}

// A share's byte-range locks are keyed by the file's path in the bucket, so
// they conflict with NFS's and those of other shares.
func TestShareLocksKeyedByBucketPath(t *testing.T) {
	g := startGateway(t, func(o *ServerOptions) {})
	backend := newShareBackend(g.filesystem, "/projects", nil, zap.NewNop())
	backend.locks = lock.NewManager(lock.Options{})
	l, ok := backend.ByteRangeLocker().(locker)
	if !ok {
		t.Fatal("a share with the gateway's lock table has no locker of its own")
	}
	if got, want := l.key(`sub\a.txt`), lock.Key("/projects/sub/a.txt"); got != want {
		t.Errorf("lock key %q, want %q", got, want)
	}
	if (&Backend{}).ByteRangeLocker() != nil {
		t.Error("a share without the gateway's lock table claims a locker")
	}
}
