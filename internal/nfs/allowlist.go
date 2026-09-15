package nfs

import (
	"net"

	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/logging"
)

// ClientFilter decides whether a client address may connect to the NFS
// export. It is the gateway-level equivalent of a cloud security group:
// connections from outside the allowlist are dropped at TCP accept, before
// any RPC bytes are parsed.
type ClientFilter struct {
	allowed []*net.IPNet
}

// NewClientFilter builds a filter from allowed_clients rules. An empty rule
// list returns nil, meaning all clients are allowed.
func NewClientFilter(rules []string) (*ClientFilter, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	networks, err := config.ParseClientRules(rules)
	if err != nil {
		return nil, err
	}
	return &ClientFilter{allowed: networks}, nil
}

// Allowed reports whether the remote address is inside the allowlist.
func (f *ClientFilter) Allowed(addr net.Addr) bool {
	if f == nil {
		return true
	}
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok || tcpAddr.IP == nil {
		return false
	}
	for _, network := range f.allowed {
		if network.Contains(tcpAddr.IP) {
			return true
		}
	}
	return false
}

// filteredListener enforces a ClientFilter at accept time. Rejected
// connections are closed immediately and logged for auditability; Accept
// keeps serving allowed clients.
type filteredListener struct {
	net.Listener
	filter *ClientFilter
	logger *logging.KVLogger
}

// newFilteredListener wraps a listener with a client allowlist. A nil filter
// returns the listener unchanged.
func newFilteredListener(inner net.Listener, filter *ClientFilter, logger *logging.KVLogger) net.Listener {
	if filter == nil {
		return inner
	}
	return &filteredListener{Listener: inner, filter: filter, logger: logger}
}

func (l *filteredListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.filter.Allowed(conn.RemoteAddr()) {
			return conn, nil
		}
		l.logger.Error("Rejected NFS connection from address outside allowed_clients",
			"remote_addr", conn.RemoteAddr().String())
		_ = conn.Close()
	}
}

// Made with Bob
