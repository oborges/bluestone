package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// slowBackend holds a read until it is released, so a test can drain a
// server with a request genuinely in flight.
type slowBackend struct {
	*memBackend
	release chan struct{}
	started chan struct{}
}

func (b *slowBackend) Open(ctx context.Context, opts vfs.OpenOptions) (vfs.Handle, error) {
	h, err := b.memBackend.Open(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &slowHandle{Handle: h, backend: b}, nil
}

type slowHandle struct {
	vfs.Handle
	backend *slowBackend
}

func (h *slowHandle) Read(ctx context.Context, offset int64, p []byte) (int, error) {
	select {
	case h.backend.started <- struct{}{}:
	default:
	}
	<-h.backend.release
	return h.Handle.Read(ctx, offset, p)
}

// newSlowBackend returns a backend holding one readable file, whose reads
// block until the backend is released.
func newSlowBackend(t *testing.T) *slowBackend {
	t.Helper()
	backend := &slowBackend{memBackend: newMemBackend(), release: make(chan struct{}), started: make(chan struct{}, 1)}
	h, err := backend.memBackend.Open(context.Background(), vfs.OpenOptions{Path: "slow.txt", Disposition: vfs.DispositionCreate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Write(context.Background(), 0, []byte("contents to read")); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return backend
}

// A connection in the middle of a request is not cut off: the request
// finishes, its reply reaches the client, and only then is the connection
// closed.
func TestDrainWaitsForRequestsInFlight(t *testing.T) {
	backend := newSlowBackend(t)
	srv := newTestServer(backend)
	srv.maxConcurrent = 4

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx, listener) }()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)
	mustWrite(t, fc, buildCreate(sessID, treeID, "slow.txt", wire.FileOpen))
	rh, resp := readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("create: %#x", rh.Status)
	}
	var fid [16]byte
	copy(fid[:], resp[64+64:64+80])

	mustWrite(t, fc, buildRead(sessID, treeID, fid, 0, 16))
	<-backend.started

	drained := make(chan error, 1)
	go func() {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer drainCancel()
		drained <- srv.Drain(drainCtx, 0)
	}()

	// The read is still running, so the drain waits rather than closing.
	select {
	case err := <-drained:
		t.Fatalf("Drain() returned %v while a request was in flight", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(backend.release)
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
		t.Fatalf("read that was in flight during the drain: %#x", rh.Status)
	}
	if err := <-drained; err != nil {
		t.Fatalf("Drain() = %v, want nil once the request finished", err)
	}
}

// An idle connection goes as soon as the drain starts.
func TestDrainClosesIdleConnections(t *testing.T) {
	srv := newTestServer(newMemBackend())

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, listener) }()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessionSetup(t, fc)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if err := srv.Drain(drainCtx, 0); err != nil {
		t.Fatalf("Drain() = %v, want nil for an idle connection", err)
	}

	// The connection is gone: the next read fails rather than hanging.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := fc.ReadMessage(); err == nil {
		t.Fatal("the connection was still open after the drain")
	}
}

// A request that outlasts the deadline does not hold the server open for
// ever: the connection is closed and the deadline is reported.
func TestDrainDeadlineClosesBusyConnections(t *testing.T) {
	backend := newSlowBackend(t)
	defer close(backend.release)
	srv := newTestServer(backend)
	srv.maxConcurrent = 4

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, listener) }()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)
	mustWrite(t, fc, buildCreate(sessID, treeID, "slow.txt", wire.FileOpen))
	rh, resp := readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("create: %#x", rh.Status)
	}
	var fid [16]byte
	copy(fid[:], resp[64+64:64+80])
	mustWrite(t, fc, buildRead(sessID, treeID, fid, 0, 16))
	<-backend.started

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer drainCancel()
	if err := srv.Drain(drainCtx, 0); err == nil {
		t.Fatal("Drain() = nil, want the deadline reported for a connection that stayed busy")
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := fc.ReadMessage(); err == nil {
		t.Fatal("the busy connection was left open past the deadline")
	}
}

// A client copying a file sends requests back to back with small gaps
// between them. Draining must not close the connection in one of those gaps,
// which would fail the copy; it waits until the client has been quiet.
func TestDrainWaitsForBusyConnectionToGoQuiet(t *testing.T) {
	srv := newTestServer(newMemBackend())

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, listener) }()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)

	drained := make(chan error, 1)
	go func() {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer drainCancel()
		drained <- srv.Drain(drainCtx, 300*time.Millisecond)
	}()

	// Keep working through gaps shorter than the quiet period: every
	// request must be answered.
	for i := 0; i < 6; i++ {
		time.Sleep(50 * time.Millisecond)
		mustWrite(t, fc, buildCreate(sessID, treeID, "copy.txt", wire.FileOpenIf))
		rh, resp := readReply(t, fc)
		if rh.Status != wire.StatusSuccess {
			t.Fatalf("request %d during the drain: status %#x", i, rh.Status)
		}
		var fid [16]byte
		copy(fid[:], resp[64+64:64+80])
		mustWrite(t, fc, buildClose(sessID, treeID, fid))
		if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
			t.Fatalf("close %d during the drain: status %#x", i, rh.Status)
		}
	}

	// Once the client stops, the connection goes.
	if err := <-drained; err != nil {
		t.Fatalf("Drain() = %v, want nil once the client went quiet", err)
	}
}
