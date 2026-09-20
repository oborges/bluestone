package server

import "time"

// Limits bound what one client can make a server hold. Zero or negative
// values mean no limit, which is how a server built without them behaves.
type Limits struct {
	// SessionsPerConnection caps authenticated sessions on one connection.
	// Further SESSION_SETUPs are refused with
	// STATUS_INSUFFICIENT_RESOURCES.
	SessionsPerConnection int
	// OpensPerSession caps the files one session holds open across its tree
	// connects. Further CREATEs are refused with
	// STATUS_INSUFFICIENT_RESOURCES.
	OpensPerSession int
	// TreesPerSession caps tree connects on one session.
	TreesPerSession int
}

// WithLimits bounds what one client can make the server hold.
func WithLimits(l Limits) Option { return func(s *Server) { s.limits = l } }

// AuthGate decides whether a client may attempt to authenticate, so an
// application can slow down or block repeated failures. It is consulted for
// every SESSION_SETUP, and told the outcome of each attempt that reached the
// authenticator. Implementations must be safe for concurrent use.
type AuthGate interface {
	// Allow reports whether the client at remoteAddr may attempt to
	// authenticate now. A refused attempt is answered STATUS_LOGON_FAILURE,
	// the same as a wrong password, so a caller cannot tell the difference.
	Allow(remoteAddr string) bool
	// Attempted reports the outcome of an attempt: ok is true once the
	// client is authenticated, false when the credentials were refused.
	// Attempts still in progress (NTLM takes two rounds) are not reported.
	Attempted(remoteAddr string, ok bool, took time.Duration)
}

// WithAuthGate consults g before each authentication attempt.
func WithAuthGate(g AuthGate) Option { return func(s *Server) { s.authGate = g } }

type openAuthGate struct{}

func (openAuthGate) Allow(string) bool                     { return true }
func (openAuthGate) Attempted(string, bool, time.Duration) {}

// gate returns the server's authentication gate, or one that allows every
// attempt for a Server built without New.
func (s *Server) gate() AuthGate {
	if s.authGate == nil {
		return openAuthGate{}
	}
	return s.authGate
}
