package ntlmssp

import (
	"bytes"
	"testing"
)

// A stored NT hash derives the same per-login key as the password it came
// from, so a server can keep the hash instead of the password.
func TestNTOWFv2FromHashMatchesPassword(t *testing.T) {
	const password = "Pa$$w0rd!"
	for _, login := range []struct{ user, domain string }{
		{"alice", "BLUESTONE"},
		{"ALICE", "WORKGROUP"},
		{"bob", ""},
	} {
		want := NTOWFv2(password, login.user, login.domain)
		got := NTOWFv2FromHash(NTHash(password), login.user, login.domain)
		if !bytes.Equal(got, want) {
			t.Errorf("NTOWFv2FromHash(%q, %q) = %x, want %x", login.user, login.domain, got, want)
		}
	}
	if n := len(NTHash(password)); n != 16 {
		t.Errorf("NTHash length = %d, want 16", n)
	}
}

// The NT hash is the one Windows and Samba use, so a hash taken from
// elsewhere works here. This is the documented MD4 of the UTF-16LE password.
//
// The constant below is the published NT hash of the word "password", used
// as a test vector in MS-NLMP and by Samba and hashcat. Secret scanners
// recognise it, correctly, as a valid NT hash: it belongs in this test,
// where the point is that the function agrees with everyone else, and
// nowhere else in the tree.
func TestNTHashMatchesKnownValue(t *testing.T) {
	const want = "8846f7eaee8fb117ad06bdd830b7586c"
	if got := NTHash("password"); got == nil || hexOf(got) != want {
		t.Fatalf("NTHash(\"password\") = %x, want %s", got, want)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xF])
	}
	return string(out)
}
