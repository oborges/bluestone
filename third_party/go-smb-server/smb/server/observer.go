package server

import "time"

// Observer receives what a server does, so an application can count
// connections, sessions and requests without wrapping the protocol itself.
// Every method may be called from several goroutines at once, and is called
// on the path of the request it describes, so implementations should be
// quick and must not block.
type Observer interface {
	// ConnectionOpened and ConnectionClosed bracket one client connection.
	ConnectionOpened()
	ConnectionClosed()
	// SessionOpened reports an authenticated session; SessionClosed reports
	// it ending, by logoff or because its connection went away.
	SessionOpened()
	SessionClosed()
	// RequestCompleted reports one request: its SMB2 command (the wire.Cmd
	// constants), the status answered, and how long the handler took. A
	// compound request reports each of its commands separately.
	RequestCompleted(command uint16, status uint32, took time.Duration)
}

// LeaseObserver is an Observer that also hears about client caching. kind
// is "lease" or "oplock"; states are lease state bits (read, handle), with
// a level II oplock reported as read.
type LeaseObserver interface {
	// LeaseGranted reports caching granted to an open.
	LeaseGranted(kind string, state uint32)
	// LeaseBroken reports a break sent, from one state to a lower one.
	LeaseBroken(kind string, from, to uint32)
	// LeaseBreakTimedOut reports a break the client never acknowledged.
	LeaseBreakTimedOut()
}

// leaseObs returns the observer's lease reporting, if it has any.
func (s *Server) leaseObs() LeaseObserver {
	if o, ok := s.observer.(LeaseObserver); ok {
		return o
	}
	return nil
}

// WithObserver sends what the server does to o.
func WithObserver(o Observer) Option { return func(s *Server) { s.observer = o } }

// noopObserver is used when the application supplies none, so the server
// never checks for nil on a request path.
type noopObserver struct{}

func (noopObserver) ConnectionOpened()                              {}
func (noopObserver) ConnectionClosed()                              {}
func (noopObserver) SessionOpened()                                 {}
func (noopObserver) SessionClosed()                                 {}
func (noopObserver) RequestCompleted(uint16, uint32, time.Duration) {}
