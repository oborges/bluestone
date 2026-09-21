package spnego

import (
	"bytes"
	"context"
	"encoding/asn1"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/auth"
	"github.com/sonroyaalmerol/go-smb-server/smb/ntlmssp"
)

// Advertising NTLM alone gives the token the server has always sent.
func TestNegTokenInitMatchesNTLMOnly(t *testing.T) {
	if got, want := NegTokenInit(OIDNTLM), ntlmssp.NegTokenInitNTLM(); !bytes.Equal(got, want) {
		t.Fatalf("NegTokenInit(NTLM) = %x, want %x", got, want)
	}
}

func spnegoInit(mechs []asn1.ObjectIdentifier, mechToken []byte) []byte {
	body, _ := asn1.Marshal(struct {
		MechTypes []asn1.ObjectIdentifier `asn1:"explicit,tag:0"`
		MechToken []byte                  `asn1:"explicit,tag:2"`
	}{mechs, mechToken})
	oid, _ := asn1.Marshal(OIDSPNEGO)
	return wrap(0x60, append(oid, wrap(0xA0, body)...))
}

func krb5Token() []byte {
	oid, _ := asn1.Marshal(OIDKRB5)
	return wrap(0x60, append(oid, 0x01, 0x00, 0x6e, 0x00))
}

func TestMechanism(t *testing.T) {
	ntlm := append([]byte("NTLMSSP\x00"), 1, 0, 0, 0)
	for _, tc := range []struct {
		name  string
		token []byte
		want  mech
	}{
		{"raw NTLMSSP", ntlm, mechNTLM},
		{"raw Kerberos", krb5Token(), mechKerberos},
		{"SPNEGO with NTLM", spnegoInit([]asn1.ObjectIdentifier{OIDNTLM}, ntlm), mechNTLM},
		{"SPNEGO with Kerberos, as Windows", spnegoInit([]asn1.ObjectIdentifier{OIDMSKRB5, OIDKRB5, OIDNTLM}, krb5Token()), mechKerberos},
		{"SPNEGO without a token", spnegoInit([]asn1.ObjectIdentifier{OIDNTLM}, nil), mechUnknown},
		{"garbage", []byte{1, 2, 3}, mechUnknown},
	} {
		if got := mechanism(tc.token); got != tc.want {
			t.Errorf("%s: mechanism = %d, want %d", tc.name, got, tc.want)
		}
	}
}

type named string

func (n named) Accept(context.Context, []byte) (auth.AcceptResult, error) {
	return auth.AcceptResult{Identity: &auth.Identity{Username: string(n)}}, nil
}

// Each session sticks with the mechanism its first token chose, and a
// mechanism that is not configured fails the logon.
func TestNegotiateRoutesByFirstToken(t *testing.T) {
	krb := func() auth.Authenticator { return named("kerberos") }
	ntlm := func() auth.Authenticator { return named("ntlm") }
	ntlmToken := append([]byte("NTLMSSP\x00"), 1, 0, 0, 0)

	a := Negotiate(krb, ntlm)()
	if res, _ := a.Accept(context.Background(), krb5Token()); res.Identity.Username != "kerberos" {
		t.Errorf("Kerberos token went to %s", res.Identity.Username)
	}
	if res, _ := a.Accept(context.Background(), ntlmToken); res.Identity.Username != "kerberos" {
		t.Error("a session changed mechanism after its first token")
	}
	if res, _ := Negotiate(krb, ntlm)().Accept(context.Background(), ntlmToken); res.Identity.Username != "ntlm" {
		t.Errorf("NTLM token went to %s", res.Identity.Username)
	}
	if _, err := Negotiate(nil, ntlm)().Accept(context.Background(), krb5Token()); err != auth.ErrLogonFailed {
		t.Errorf("Kerberos token without Kerberos configured: %v, want a logon failure", err)
	}
}
