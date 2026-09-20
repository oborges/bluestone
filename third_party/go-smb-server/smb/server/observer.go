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
