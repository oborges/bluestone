package cos

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// circuitOpenError implements the legacy Temporary contract used by the IBM
// SDK retryer. Returning false is important: retry backoff after a fast-fail
// would defeat the breaker.
type circuitOpenError struct{}

func (circuitOpenError) Error() string {
	return "object store circuit open: backend unreachable, failing fast"
}
func (circuitOpenError) Temporary() bool { return false }

// ErrCircuitOpen is returned (wrapped by net/http and the SDK) when the
// breaker rejects a request because the object store is known unreachable.
var ErrCircuitOpen error = circuitOpenError{}

const (
	// breakerFailureThreshold consecutive transport-level failures open the
	// circuit. Conservative enough that isolated timeouts under load do not
	// trip it.
	breakerFailureThreshold = 5
	// breakerProbeInterval is how often a single request is let through an
	// open circuit to test whether the backend recovered.
	breakerProbeInterval = 5 * time.Second
)

// breakerTransport is a three-state circuit breaker at the HTTP transport
// layer. During an object-store outage every uncached operation otherwise
// waits out full dial/retry timeouts before the gateway's staging and
// stale-cache fallbacks engage; the breaker turns those waits into immediate
// failures once the outage is established, and re-closes within one probe
// interval of the backend recovering.
//
// Only transport-level errors (dial, TLS, timeout) count as failures: an
// HTTP response of any status proves the backend is reachable. Requests
// canceled by their own caller are not counted.
type breakerTransport struct {
	inner http.RoundTripper

	mu               sync.Mutex
	consecutiveFails int
	open             bool
	lastProbe        time.Time
}

func newBreakerTransport(inner http.RoundTripper) *breakerTransport {
	return &breakerTransport{inner: inner}
}

// admit decides whether a request may proceed. When the circuit is open, one
// request per probe interval is admitted as the recovery probe.
func (b *breakerTransport) admit() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.open {
		return nil
	}
	if time.Since(b.lastProbe) >= breakerProbeInterval {
		// This request becomes the probe.
		b.lastProbe = time.Now()
		return nil
	}
	return ErrCircuitOpen
}

func (b *breakerTransport) recordSuccess() {
	b.mu.Lock()
	wasOpen := b.open
	b.open = false
	b.consecutiveFails = 0
	b.mu.Unlock()

	if wasOpen {
		logging.Info("Object store circuit closed: backend reachable again")
	}
}

func (b *breakerTransport) recordFailure(err error) {
	b.mu.Lock()
	b.consecutiveFails++
	opened := false
	if !b.open && b.consecutiveFails >= breakerFailureThreshold {
		b.open = true
		b.lastProbe = time.Now()
		opened = true
	}
	failures := b.consecutiveFails
	b.mu.Unlock()

	if opened {
		logging.Error("Object store circuit OPEN: backend unreachable, failing fast until a probe succeeds",
			zap.Int("consecutive_failures", failures),
			zap.Duration("probe_interval", breakerProbeInterval),
			zap.Error(err))
	}
}

func (b *breakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := b.admit(); err != nil {
		return nil, err
	}

	resp, err := b.inner.RoundTrip(req)
	if err != nil {
		// An explicit cancellation says nothing about the backend. Deadline
		// failures do count: dial/TLS/request timeouts are outage signals.
		if ctxErr := req.Context().Err(); errors.Is(ctxErr, context.Canceled) {
			return nil, err
		}
		b.recordFailure(err)
		return nil, err
	}

	b.recordSuccess()
	return resp, nil
}

// CloseIdleConnections preserves the optional transport capability through
// the wrapper.
func (b *breakerTransport) CloseIdleConnections() {
	if closer, ok := b.inner.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// Open reports whether the circuit is currently rejecting requests.
func (b *breakerTransport) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}
