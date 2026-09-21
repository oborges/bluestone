package kerberos

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"time"

	goforkasn1 "github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/iana"
	"github.com/jcmturner/gokrb5/v8/iana/asnAppTag"
	"github.com/jcmturner/gokrb5/v8/iana/chksumtype"
	"github.com/jcmturner/gokrb5/v8/iana/flags"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/iana/msgtype"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/jcmturner/gokrb5/v8/spnego"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/sonroyaalmerol/go-smb-server/smb/auth"
)

type Option func(*config)

type config struct {
	settings []func(*service.Settings)
	noPAC    bool
}

func WithMaxClockSkew(d time.Duration) Option {
	return func(c *config) { c.settings = append(c.settings, service.MaxClockSkew(d)) }
}

func WithKeytabPrincipal(spn string) Option {
	return func(c *config) { c.settings = append(c.settings, service.KeytabPrincipal(spn)) }
}

func WithLogger(l *log.Logger) Option {
	return func(c *config) { c.settings = append(c.settings, service.Logger(l)) }
}

func WithoutPAC() Option {
	return func(c *config) { c.noPAC = true }
}

// LoadKeytab reads a keytab file, as ktpass or "net ads keytab" writes it.
func LoadKeytab(path string) (*keytab.Keytab, error) {
	kt, err := keytab.Load(path)
	if err != nil {
		return nil, fmt.Errorf("kerberos: load keytab %s: %w", path, err)
	}
	if len(kt.Entries) == 0 {
		return nil, fmt.Errorf("kerberos: keytab %s has no keys", path)
	}
	return kt, nil
}

// Principals lists the service principals a keytab holds keys for, as
// "cifs/host.example.com@EXAMPLE.COM", for logging what the server answers
// to.
func Principals(kt *keytab.Keytab) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range kt.Entries {
		name := e.Principal.String()
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func NewServer(kt *keytab.Keytab, opts ...Option) auth.Factory {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}
	settingsOpts := make([]func(*service.Settings), 0, len(cfg.settings)+1)
	settingsOpts = append(settingsOpts, cfg.settings...)
	settingsOpts = append(settingsOpts, service.DecodePAC(false))
	settings := service.NewSettings(kt, settingsOpts...)
	wantPAC := !cfg.noPAC
	return func() auth.Authenticator {
		return &Authenticator{settings: settings, wantPAC: wantPAC}
	}
}

type Authenticator struct {
	settings *service.Settings
	wantPAC  bool
	done     bool
}

func (a *Authenticator) Accept(_ context.Context, token []byte) (auth.AcceptResult, error) {
	if a.done {
		return auth.AcceptResult{}, errors.New("kerberos: security context already established")
	}
	if a.settings == nil || a.settings.Keytab == nil {
		return auth.AcceptResult{}, fmt.Errorf("kerberos: no service keytab configured")
	}

	mech, mechOID, err := extractMechToken(token)
	if err != nil {
		return auth.AcceptResult{}, auth.ErrLogonFailed
	}

	var mt spnego.KRB5Token
	if err := mt.Unmarshal(mech); err != nil {
		return auth.AcceptResult{}, auth.ErrLogonFailed
	}
	if !mt.IsAPReq() {
		return auth.AcceptResult{}, auth.ErrLogonFailed
	}

	ok, creds, err := service.VerifyAPREQ(&mt.APReq, a.settings)
	if err != nil || !ok {
		return auth.AcceptResult{}, auth.ErrLogonFailed
	}

	ticketKey := mt.APReq.Ticket.DecryptedEncPart.Key
	key := ticketKey
	if sub := mt.APReq.Authenticator.SubKey; len(sub.KeyValue) > 0 {
		key = sub
	}

	ident := &auth.Identity{
		Username:  creds.UserName(),
		Domain:    creds.Domain(),
		Mechanism: auth.MechanismKerberos,
	}
	if a.wantPAC {
		addPACIdentity(ident, &mt.APReq, a.settings)
	}

	// A client asking for mutual authentication, as Windows always does,
	// gets an AP-REP proving the server could read its ticket; without one
	// it fails the session setup.
	var apRep []byte
	if mutualRequested(&mt.APReq) {
		apRep, err = buildAPRep(&mt.APReq, ticketKey)
		if err != nil {
			return auth.AcceptResult{}, fmt.Errorf("kerberos: build AP-REP: %w", err)
		}
	}
	out, err := acceptCompletedToken(mechOID, apRep)
	if err != nil {
		return auth.AcceptResult{}, fmt.Errorf("kerberos: marshal response token: %w", err)
	}

	a.done = true
	return auth.AcceptResult{
		OutputToken: out,
		Identity:    ident,
		SessionKey:  append([]byte(nil), key.KeyValue...),
	}, nil
}

// Kerberos mechanism OIDs: the standard one, and the one Windows lists
// first, which Microsoft once encoded wrongly and has kept (MS-KILE).
var (
	oidKRB5       = gssapi.OIDKRB5.OID()
	oidMSKRB5     = gssapi.OIDMSLegacyKRB5.OID()
	oidKRB5Token  = goforkasn1.ObjectIdentifier{1, 2, 840, 113554, 1, 2, 2}
	tokIDKRBAPRep = []byte{0x02, 0x00}
)

// extractMechToken returns the Kerberos token inside a session setup's
// security buffer, and the mechanism the client offered it as, which the
// reply names back.
func extractMechToken(token []byte) ([]byte, goforkasn1.ObjectIdentifier, error) {
	if len(token) == 0 {
		return nil, nil, errors.New("kerberos: empty security buffer")
	}
	var st spnego.SPNEGOToken
	if err := st.Unmarshal(token); err == nil {
		if st.Init && len(st.NegTokenInit.MechTokenBytes) > 0 {
			oid := oidKRB5
			if types := st.NegTokenInit.MechTypes; len(types) > 0 && types[0].Equal(oidMSKRB5) {
				oid = oidMSKRB5
			}
			return st.NegTokenInit.MechTokenBytes, oid, nil
		}
		if st.Resp && len(st.NegTokenResp.ResponseToken) > 0 {
			return st.NegTokenResp.ResponseToken, oidKRB5, nil
		}
		return nil, nil, errors.New("kerberos: SPNEGO token carries no mechanism token")
	}
	var mt spnego.KRB5Token
	if err := mt.Unmarshal(token); err != nil {
		return nil, nil, errors.New("kerberos: token is neither SPNEGO nor KRB5")
	}
	return token, nil, nil
}

// mutualRequested reports whether the client asked for mutual
// authentication, in the AP-REQ's options or its GSS-API checksum flags.
func mutualRequested(apreq *messages.APReq) bool {
	if types.IsFlagSet(&apreq.APOptions, flags.APOptionMutualRequired) {
		return true
	}
	cksum := apreq.Authenticator.Cksum
	if cksum.CksumType == chksumtype.GSSAPI && len(cksum.Checksum) >= 24 {
		return binary.LittleEndian.Uint32(cksum.Checksum[20:24])&uint32(gssapi.ContextFlagMutual) != 0
	}
	return false
}

// buildAPRep builds the GSS-API KRB5 token carrying an AP-REP for apreq
// (RFC 4120 section 5.5.2, RFC 4121 section 4.1): the client's own time,
// encrypted in the ticket's session key. No acceptor subkey is sent, so the
// session key stays the client's subkey, or the ticket's.
func buildAPRep(apreq *messages.APReq, ticketKey types.EncryptionKey) ([]byte, error) {
	var seq [4]byte
	if _, err := rand.Read(seq[:]); err != nil {
		return nil, err
	}
	encPart := messages.EncAPRepPart{
		CTime:          apreq.Authenticator.CTime,
		Cusec:          apreq.Authenticator.Cusec,
		SequenceNumber: int64(binary.BigEndian.Uint32(seq[:])&0x3fffffff) + 1,
	}
	plain, err := goforkasn1.Marshal(encPart)
	if err != nil {
		return nil, err
	}
	plain = asn1tools.AddASNAppTag(plain, asnAppTag.EncAPRepPart)
	enc, err := crypto.GetEncryptedData(plain, ticketKey, keyusage.AP_REP_ENCPART, 0)
	if err != nil {
		return nil, err
	}
	rep, err := goforkasn1.Marshal(messages.APRep{PVNO: iana.PVNO, MsgType: msgtype.KRB_AP_REP, EncPart: enc})
	if err != nil {
		return nil, err
	}
	rep = asn1tools.AddASNAppTag(rep, asnAppTag.APREP)
	b, err := goforkasn1.Marshal(oidKRB5Token)
	if err != nil {
		return nil, err
	}
	b = append(b, tokIDKRBAPRep...)
	b = append(b, rep...)
	return asn1tools.AddASNAppTag(b, 0), nil
}

func acceptCompletedToken(mech goforkasn1.ObjectIdentifier, responseToken []byte) ([]byte, error) {
	if mech == nil {
		mech = oidKRB5
	}
	resp := spnego.NegTokenResp{
		NegState:      goforkasn1.Enumerated(spnego.NegStateAcceptCompleted),
		SupportedMech: mech,
		ResponseToken: responseToken,
	}
	return resp.Marshal()
}

// addPACIdentity fills in what the ticket's PAC says about the client: its
// SID, its groups, and its domain's NetBIOS name. A ticket without a PAC,
// or with one whose server signature does not verify, adds nothing.
func addPACIdentity(ident *auth.Identity, apreq *messages.APReq, settings *service.Settings) {
	isPAC, pac, err := apreq.Ticket.GetPACType(settings.Keytab, settings.KeytabPrincipal(), settings.Logger())
	if !isPAC || err != nil || pac.KerbValidationInfo == nil {
		return
	}
	info := pac.KerbValidationInfo
	domainSID := info.LogonDomainID.String()
	if info.UserID != 0 {
		ident.SID = fmt.Sprintf("%s-%d", domainSID, info.UserID)
	}
	if info.PrimaryGroupID != 0 {
		ident.PrimaryGroup = fmt.Sprintf("%s-%d", domainSID, info.PrimaryGroupID)
	}
	if name := info.LogonDomainName.String(); name != "" {
		ident.Domain = name
	}
	if name := info.EffectiveName.String(); name != "" {
		ident.Username = name
	}
	if sids := info.GetGroupMembershipSIDs(); len(sids) > 0 {
		ident.Groups = append([]string(nil), sids...)
	}
}
