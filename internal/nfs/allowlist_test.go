package nfs

import (
	"net"
	"testing"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

func TestClientFilterRules(t *testing.T) {
	filter, err := NewClientFilter([]string{"10.0.1.0/24", "192.168.7.5", "fd00::/8"})
	if err != nil {
		t.Fatalf("NewClientFilter() error = %v", err)
	}

	cases := []struct {
		ip      string
		allowed bool
	}{
		{"10.0.1.55", true},
		{"10.0.2.55", false},
		{"192.168.7.5", true},
		{"192.168.7.6", false},
		{"fd00::1234", true},
		{"fe80::1", false},
	}
	for _, tc := range cases {
		addr := &net.TCPAddr{IP: net.ParseIP(tc.ip), Port: 1000}
		if got := filter.Allowed(addr); got != tc.allowed {
			t.Errorf("Allowed(%s) = %v, want %v", tc.ip, got, tc.allowed)
		}
	}

	// Non-TCP addresses are rejected when a filter is active.
	if filter.Allowed(&net.UnixAddr{Name: "sock"}) {
		t.Error("Allowed(unix addr) = true, want false")
	}
}

func TestClientFilterEmptyAllowsAll(t *testing.T) {
	filter, err := NewClientFilter(nil)
	if err != nil {
		t.Fatalf("NewClientFilter(nil) error = %v", err)
	}
	if filter != nil {
		t.Fatal("empty rules should produce a nil (allow-all) filter")
	}
	if !filter.Allowed(&net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 1000}) {
		t.Error("nil filter must allow all clients")
	}
}

func TestClientFilterInvalidRule(t *testing.T) {
	if _, err := NewClientFilter([]string{"not-an-ip"}); err == nil {
		t.Fatal("NewClientFilter(invalid) should fail")
	}
	if _, err := NewClientFilter([]string{"10.0.0.0/33"}); err == nil {
		t.Fatal("NewClientFilter(bad mask) should fail")
	}
}

func TestFilteredListenerRejectsAndKeepsServing(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer inner.Close()

	// Loopback connections arrive from 127.0.0.1; a filter allowing only a
	// foreign subnet must reject them.
	denyFilter, err := NewClientFilter([]string{"192.0.2.0/24"})
	if err != nil {
		t.Fatalf("NewClientFilter() error = %v", err)
	}
	listener := newFilteredListener(inner, denyFilter, logging.NewKVLogger(zap.NewNop()))

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	// A denied client is closed immediately.
	denied, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer denied.Close()
	_ = denied.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := denied.Read(buf); err == nil {
		t.Fatal("denied connection should be closed by the server")
	}

	select {
	case conn := <-accepted:
		conn.Close()
		t.Fatal("Accept() returned a denied connection")
	case err := <-acceptErr:
		t.Fatalf("Accept() error = %v", err)
	default:
		// Accept is still blocked waiting for an allowed client: correct.
	}
}

func TestFilteredListenerAllowsMatchingClient(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer inner.Close()

	allowFilter, err := NewClientFilter([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("NewClientFilter() error = %v", err)
	}
	listener := newFilteredListener(inner, allowFilter, logging.NewKVLogger(zap.NewNop()))

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	client, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()

	select {
	case conn := <-accepted:
		conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("allowed connection was not accepted")
	}
}

// Made with Bob
