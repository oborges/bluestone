package auth

import (
	"context"
	"errors"
)

// Identity is who a session authenticated as.
type Identity struct {
	// Username is the account name, and Domain its domain: the NetBIOS
	// name for a domain account, or whatever an NTLM client sent.
	Username string
	Domain   string
	// SID is the account's security identifier, when authentication
	// carried one (a Kerberos ticket's PAC). PrimaryGroup is the SID of its
	// primary group, and Groups the SIDs of every group it belongs to.
	SID          string
	PrimaryGroup string
	Groups       []string
	// Mechanism is how the session authenticated: MechanismNTLM or
	// MechanismKerberos. An NTLM client names its own domain, which proves
	// nothing; a Kerberos ticket's domain was vouched for by its KDC.
	Mechanism string
}

// Authentication mechanisms an Identity can come from.
const (
	MechanismNTLM     = "ntlm"
	MechanismKerberos = "kerberos"
)

type AcceptResult struct {
	OutputToken []byte
	Identity    *Identity
	SessionKey  []byte
}

type Authenticator interface {
	Accept(ctx context.Context, input []byte) (AcceptResult, error)
}

type Factory func() Authenticator

var ErrLogonFailed = errors.New("auth: logon failed")

func AlwaysAllowFactory() Factory {
	return func() Authenticator { return alwaysAllowAuthenticator{} }
}

type alwaysAllowAuthenticator struct{}

func (alwaysAllowAuthenticator) Accept(_ context.Context, _ []byte) (AcceptResult, error) {
	return AcceptResult{Identity: &Identity{Username: "guest"}}, nil
}
