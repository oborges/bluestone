package kerberos_test

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	goforkasn1 "github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/iana"
	"github.com/jcmturner/gokrb5/v8/iana/adtype"
	"github.com/jcmturner/gokrb5/v8/iana/asnAppTag"
	"github.com/jcmturner/gokrb5/v8/iana/chksumtype"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/flags"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/iana/nametype"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/spnego"
	"github.com/jcmturner/gokrb5/v8/test/testdata"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/sonroyaalmerol/go-smb-server/smb/kerberos"
)

// apReqOptions shape the AP-REQ a test client sends.
type apReqOptions struct {
	sname    types.PrincipalName
	realm    string
	kvno     int
	authData types.AuthorizationData
	mutual   bool
	mechs    []goforkasn1.ObjectIdentifier
}

// buildAPReq builds a SPNEGO token carrying an AP-REQ the way Windows does:
// with a subkey, a GSS-API checksum, and mutual authentication when asked
// for. It returns the token, the ticket's session key and the authenticator.
func buildAPReq(t *testing.T, kt *keytab.Keytab, o apReqOptions) ([]byte, types.EncryptionKey, types.Authenticator) {
	t.Helper()
	cname := types.PrincipalName{NameType: nametype.KRB_NT_PRINCIPAL, NameString: []string{testClient}}
	now := time.Now().UTC()
	etype, err := crypto.GetEtype(etypeID.AES256_CTS_HMAC_SHA1_96)
	if err != nil {
		t.Fatal(err)
	}
	ticketKey, err := types.GenerateEncryptionKey(etype)
	if err != nil {
		t.Fatal(err)
	}
	encTicket := messages.EncTicketPart{
		Flags: types.NewKrbFlags(), Key: ticketKey, CRealm: o.realm, CName: cname,
		AuthTime: now, StartTime: now, EndTime: now.Add(time.Hour), RenewTill: now.Add(24 * time.Hour),
		AuthorizationData: o.authData,
	}
	b, err := goforkasn1.Marshal(encTicket)
	if err != nil {
		t.Fatal(err)
	}
	b = asn1tools.AddASNAppTag(b, asnAppTag.EncTicketPart)
	serviceKey, _, err := kt.GetEncryptionKey(o.sname, o.realm, o.kvno, etypeID.AES256_CTS_HMAC_SHA1_96)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := crypto.GetEncryptedData(b, serviceKey, keyusage.KDC_REP_TICKET, o.kvno)
	if err != nil {
		t.Fatal(err)
	}
	tkt := messages.Ticket{TktVNO: iana.PVNO, Realm: o.realm, SName: o.sname, EncPart: enc}

	authn, err := types.NewAuthenticator(o.realm, cname)
	if err != nil {
		t.Fatal(err)
	}
	gssFlags := uint32(gssapi.ContextFlagInteg | gssapi.ContextFlagConf)
	if o.mutual {
		gssFlags |= uint32(gssapi.ContextFlagMutual)
	}
	chksum := make([]byte, 24)
	binary.LittleEndian.PutUint32(chksum[0:4], 16)
	binary.LittleEndian.PutUint32(chksum[20:24], gssFlags)
	authn.Cksum = types.Checksum{CksumType: chksumtype.GSSAPI, Checksum: chksum}
	if err := authn.GenerateSeqNumberAndSubKey(etypeID.AES256_CTS_HMAC_SHA1_96, 32); err != nil {
		t.Fatal(err)
	}
	apreq, err := messages.NewAPReq(tkt, ticketKey, authn)
	if err != nil {
		t.Fatal(err)
	}
	if o.mutual {
		types.SetFlag(&apreq.APOptions, flags.APOptionMutualRequired)
	}
	oidBytes, _ := goforkasn1.Marshal(gssapi.OIDKRB5.OID())
	mech := append(oidBytes, 0x01, 0x00)
	apreqBytes, err := apreq.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	mech = asn1tools.AddASNAppTag(append(mech, apreqBytes...), 0)

	mechs := o.mechs
	if mechs == nil {
		mechs = []goforkasn1.ObjectIdentifier{gssapi.OIDKRB5.OID()}
	}
	st := spnego.SPNEGOToken{Init: true, NegTokenInit: spnego.NegTokenInit{MechTypes: mechs, MechTokenBytes: mech}}
	token, err := st.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return token, ticketKey, authn
}

// A client asking for mutual authentication, as Windows does, gets an
// AP-REP it can decrypt with the ticket's session key, carrying back the
// time from its own authenticator; the reply names the mechanism the client
// offered first.
func TestAuthenticator_MutualAuthentication(t *testing.T) {
	kt := newTestKeytab(t)
	_, sname := testPrincipals()
	mechs := []goforkasn1.ObjectIdentifier{gssapi.OIDMSLegacyKRB5.OID(), gssapi.OIDKRB5.OID()}
	token, ticketKey, authn := buildAPReq(t, kt, apReqOptions{sname: sname, realm: testRealm, kvno: 1, mutual: true, mechs: mechs})

	res, err := kerberos.NewServer(kt)().Accept(context.Background(), token)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if want := authn.SubKey.KeyValue; string(res.SessionKey) != string(want) {
		t.Error("session key is not the client's subkey")
	}
	var resp spnego.SPNEGOToken
	if err := resp.Unmarshal(res.OutputToken); err != nil || !resp.Resp {
		t.Fatalf("response is not a NegTokenResp: %v", err)
	}
	if !resp.NegTokenResp.SupportedMech.Equal(gssapi.OIDMSLegacyKRB5.OID()) {
		t.Errorf("supportedMech %v, want the MS-KRB5 OID the client offered first", resp.NegTokenResp.SupportedMech)
	}
	var mt spnego.KRB5Token
	if err := mt.Unmarshal(resp.NegTokenResp.ResponseToken); err != nil {
		t.Fatalf("response token: %v", err)
	}
	if !mt.IsAPRep() {
		t.Fatal("response token is not an AP-REP")
	}
	plain, err := crypto.DecryptEncPart(mt.APRep.EncPart, ticketKey, keyusage.AP_REP_ENCPART)
	if err != nil {
		t.Fatalf("AP-REP does not decrypt with the ticket's session key: %v", err)
	}
	var part messages.EncAPRepPart
	if err := part.Unmarshal(plain); err != nil {
		t.Fatalf("EncAPRepPart: %v", err)
	}
	if !part.CTime.Equal(authn.CTime.Truncate(time.Second)) || part.Cusec != authn.Cusec {
		t.Errorf("AP-REP time %v/%d, want the authenticator's %v/%d", part.CTime, part.Cusec, authn.CTime, authn.Cusec)
	}
}

// Without mutual authentication the reply carries no AP-REP.
func TestAuthenticator_NoAPRepUnlessAsked(t *testing.T) {
	kt := newTestKeytab(t)
	_, sname := testPrincipals()
	token, _, _ := buildAPReq(t, kt, apReqOptions{sname: sname, realm: testRealm, kvno: 1})
	res, err := kerberos.NewServer(kt)().Accept(context.Background(), token)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	var resp spnego.SPNEGOToken
	if err := resp.Unmarshal(res.OutputToken); err != nil {
		t.Fatal(err)
	}
	if len(resp.NegTokenResp.ResponseToken) != 0 {
		t.Error("AP-REP sent to a client that did not ask for mutual authentication")
	}
}

// The identity comes from the ticket's PAC: the account's SID, primary
// group, groups and NetBIOS domain. The PAC is a real one from Active
// Directory, signed with the key in gokrb5's test keytab.
func TestAuthenticator_IdentityFromPAC(t *testing.T) {
	pacBytes, err := hex.DecodeString(testdata.MarshaledPAC_AD_WIN2K_PAC)
	if err != nil {
		t.Fatal(err)
	}
	ktBytes, err := hex.DecodeString(testdata.KEYTAB_SYSHTTP_TEST_GOKRB5)
	if err != nil {
		t.Fatal(err)
	}
	kt := keytab.New()
	if err := kt.Unmarshal(ktBytes); err != nil {
		t.Fatal(err)
	}
	inner, err := goforkasn1.Marshal(types.AuthorizationData{{ADType: adtype.ADWin2KPAC, ADData: pacBytes}})
	if err != nil {
		t.Fatal(err)
	}
	authData := types.AuthorizationData{{ADType: adtype.ADIfRelevant, ADData: inner}}
	sname, _ := types.ParseSPNString("sysHTTP")
	token, _, _ := buildAPReq(t, kt, apReqOptions{sname: sname, realm: "TEST.GOKRB5", kvno: 2, authData: authData, mutual: true})

	res, err := kerberos.NewServer(kt)().Accept(context.Background(), token)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	id := res.Identity
	if id.Username != "testuser1" {
		t.Errorf("username %q, want testuser1", id.Username)
	}
	if id.Domain == "" || id.Domain == "TEST.GOKRB5" {
		t.Errorf("domain %q, want the NetBIOS name from the PAC", id.Domain)
	}
	if !strings.HasPrefix(id.SID, "S-1-5-21-") || !strings.HasSuffix(id.SID, "-1105") {
		t.Errorf("SID %q, want the domain's SID with RID 1105", id.SID)
	}
	domainSID := strings.TrimSuffix(id.SID, "-1105")
	if id.PrimaryGroup != domainSID+"-513" {
		t.Errorf("primary group %q, want %s-513 (Domain Users)", id.PrimaryGroup, domainSID)
	}
	if len(id.Groups) == 0 {
		t.Error("no groups from the PAC")
	}
	t.Logf("identity %s\\%s %s groups %v", id.Domain, id.Username, id.SID, id.Groups)
}
