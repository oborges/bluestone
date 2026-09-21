// Package spnego lets one server offer Kerberos and NTLM side by side, the
// way Windows file servers do (RFC 4178, MS-SPNG). The server advertises
// both in its NEGOTIATE response, and each session setup is handed to
// whichever mechanism the client's first token uses.
package spnego

import (
	"bytes"
	"context"
	"encoding/asn1"
	"errors"

	"github.com/sonroyaalmerol/go-smb-server/smb/auth"
)

// Mechanism OIDs. Windows lists MS-KRB5 first: an OID Microsoft once
// encoded wrongly and has kept (MS-KILE section 3.1.5.13).
var (
	OIDSPNEGO = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 2}
	OIDMSKRB5 = asn1.ObjectIdentifier{1, 2, 840, 48018, 1, 2, 2}
	OIDKRB5   = asn1.ObjectIdentifier{1, 2, 840, 113554, 1, 2, 2}
	OIDNTLM   = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 2, 10}
)

var ntlmSignature = []byte("NTLMSSP\x00")

// NegTokenInit is the SPNEGO token a server puts in its NEGOTIATE response
// to advertise mechs (MS-SPNG section 3.2.5.2, NegTokenInit2 without hints,
// which Windows does not need).
func NegTokenInit(mechs ...asn1.ObjectIdentifier) []byte {
	init, err := asn1.Marshal(struct {
		MechTypes []asn1.ObjectIdentifier `asn1:"explicit,tag:0"`
	}{mechs})
	if err != nil {
		return nil
	}
	oid, err := asn1.Marshal(OIDSPNEGO)
	if err != nil {
		return nil
	}
	return wrap(0x60, append(oid, wrap(0xA0, init)...))
}

// Negotiate returns an authenticator factory for sessions that may use
// either mechanism: each session goes to Kerberos when its first token is
// a Kerberos AP-REQ and to NTLM otherwise. A nil factory disables its
// mechanism.
func Negotiate(kerberos, ntlm auth.Factory) auth.Factory {
	return func() auth.Authenticator {
		return &negotiator{kerberos: kerberos, ntlm: ntlm}
	}
}

type negotiator struct {
	kerberos, ntlm auth.Factory
	chosen         auth.Authenticator
}

// ErrNoMechanism is returned for a first token no configured mechanism can
// take.
var ErrNoMechanism = errors.New("spnego: no mechanism in common")

func (n *negotiator) Accept(ctx context.Context, token []byte) (auth.AcceptResult, error) {
	if n.chosen == nil {
		switch mechanism(token) {
		case mechKerberos:
			if n.kerberos != nil {
				n.chosen = n.kerberos()
			}
		case mechNTLM:
			if n.ntlm != nil {
				n.chosen = n.ntlm()
			}
		}
		if n.chosen == nil {
			return auth.AcceptResult{}, auth.ErrLogonFailed
		}
	}
	return n.chosen.Accept(ctx, token)
}

type mech int

const (
	mechUnknown mech = iota
	mechKerberos
	mechNTLM
)

// mechanism reports which mechanism a session's first token uses: raw
// NTLMSSP or Kerberos, or either inside a SPNEGO NegTokenInit's mechToken.
func mechanism(token []byte) mech {
	if bytes.HasPrefix(token, ntlmSignature) {
		return mechNTLM
	}
	var outer asn1.RawValue
	if _, err := asn1.Unmarshal(token, &outer); err != nil || outer.Class != asn1.ClassApplication || outer.Tag != 0 {
		return mechUnknown
	}
	var oid asn1.ObjectIdentifier
	rest, err := asn1.Unmarshal(outer.Bytes, &oid)
	if err != nil {
		return mechUnknown
	}
	switch {
	case oid.Equal(OIDKRB5):
		return mechKerberos
	case !oid.Equal(OIDSPNEGO):
		return mechUnknown
	}
	var init asn1.RawValue
	if _, err := asn1.Unmarshal(rest, &init); err != nil || init.Tag != 0 {
		return mechUnknown
	}
	var negTokenInit struct {
		MechTypes []asn1.ObjectIdentifier `asn1:"explicit,optional,tag:0"`
		ReqFlags  asn1.BitString          `asn1:"explicit,optional,tag:1"`
		MechToken []byte                  `asn1:"explicit,optional,tag:2"`
	}
	if _, err := asn1.Unmarshal(init.Bytes, &negTokenInit); err != nil {
		return mechUnknown
	}
	if len(negTokenInit.MechToken) == 0 {
		return mechUnknown
	}
	return mechanism(negTokenInit.MechToken)
}

func wrap(tag byte, content []byte) []byte {
	n := len(content)
	var length []byte
	if n < 0x80 {
		length = []byte{byte(n)}
	} else {
		for v := n; v > 0; v >>= 8 {
			length = append([]byte{byte(v)}, length...)
		}
		length = append([]byte{0x80 | byte(len(length))}, length...)
	}
	return append(append([]byte{tag}, length...), content...)
}
