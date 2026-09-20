package ha

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// LeaseObjectKey is the bucket object used for active/passive fencing. It is
// hidden from the NFS namespace by the handler's reserved-name filter.
//
// The key keeps its pre-rename name on purpose: every gateway version must
// contend for the same object, or a mixed-version HA pair could both believe
// they hold the bucket.
const LeaseObjectKey = ".nfs-gateway.lease"

// ObjectStore is the minimal object API the lease manager needs.
type ObjectStore interface {
	GetObject(ctx context.Context, key string) ([]byte, error)
	PutObject(ctx context.Context, key string, data []byte, metadata map[string]string) error
	DeleteObject(ctx context.Context, key string) error
}

// Lease is the durable fencing record stored in the bucket.
type Lease struct {
	HolderID   string    `json:"holder_id"`
	Hostname   string    `json:"hostname"`
	Epoch      uint64    `json:"epoch"`
	AcquiredAt time.Time `json:"acquired_at"`
	RenewedAt  time.Time `json:"renewed_at"`
}

// Manager maintains the active gateway's lease and fences out concurrent
// gateways for the same bucket. One gateway owning one export is the
// supported model; the lease makes violations fail loudly instead of
// corrupting write-back state.
type Manager struct {
	store             ObjectStore
	holderID          string
	hostname          string
	heartbeatInterval time.Duration
	leaseTimeout      time.Duration
	holderMarkerPath  string
	onLeaseLost       func(LeaseLoss)

	mu sync.Mutex
	// lastRenewed is when the lease was last written successfully, so the
	// manager knows how long it has been unable to prove it still holds it.
	lastRenewed time.Time
	lease       Lease
	cancel      context.CancelFunc
	done        chan struct{}
	// lost records that the loss was already reported, so a node is fenced
	// once rather than on every heartbeat after it.
	lost atomic.Bool
	// now is time.Now, replaced in tests.
	now func() time.Time
}

// Options configures the lease manager.
type Options struct {
	Store             ObjectStore
	HeartbeatInterval time.Duration
	LeaseTimeout      time.Duration
	// HolderMarkerDir is a local directory (the staging root) where the
	// manager records that this node held the lease, allowing degraded
	// restarts on the same node while the object store is unreachable.
	HolderMarkerDir string
	// ForceTakeover steals a fresh foreign lease. Break-glass only: the
	// operator must know the previous holder is dead.
	ForceTakeover bool
	// OnLeaseLost is called once when this node can no longer prove it holds
	// the lease: another gateway took it, or heartbeats have been failing
	// for longer than the lease timeout, after which a standby is entitled
	// to promote. The gateway uses it to stop serving the bucket. It is
	// called from the heartbeat goroutine; nil keeps the old behaviour of
	// logging and serving on.
	OnLeaseLost func(LeaseLoss)
}

// LeaseLoss describes why a node stopped holding the lease.
type LeaseLoss struct {
	// Reason is "taken" when another holder owns the lease now, or
	// "unreachable" when heartbeats failed for longer than the lease
	// timeout.
	Reason string
	// TakenBy is the holder that owns the lease, when it was taken.
	TakenBy *Lease
	// Since is how long heartbeats have been failing, when unreachable.
	Since time.Duration
	// Err is the last heartbeat error, when unreachable.
	Err error
}

// Description reports the loss in the form an operator reads in a log.
func (l LeaseLoss) Description() string {
	if l.Reason == "taken" && l.TakenBy != nil {
		return fmt.Sprintf("another gateway holds the lease (%s on %s)", l.TakenBy.HolderID, l.TakenBy.Hostname)
	}
	return fmt.Sprintf("the lease could not be renewed for %s: %v", l.Since.Round(time.Second), l.Err)
}

// ErrLeaseHeld is returned when another live gateway holds the lease.
type ErrLeaseHeld struct {
	Lease Lease
}

func (e *ErrLeaseHeld) Error() string {
	return fmt.Sprintf("bucket lease is held by %s (host %s, renewed %s); refusing to start a second active gateway — stop the holder or use force takeover",
		e.Lease.HolderID, e.Lease.Hostname, e.Lease.RenewedAt.Format(time.RFC3339))
}

func newHolderID(hostname string) string {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return fmt.Sprintf("%s-%d-%s", hostname, os.Getpid(), hex.EncodeToString(suffix))
}

// Acquire fences this gateway in: it refuses when a fresh foreign lease
// exists, takes over stale leases, and tolerates an unreachable object store
// only when the local holder marker shows this node was the previous holder
// (crash recovery during an outage).
func Acquire(ctx context.Context, opts Options) (*Manager, error) {
	hostname, _ := os.Hostname()
	m := &Manager{
		store:             opts.Store,
		holderID:          newHolderID(hostname),
		hostname:          hostname,
		heartbeatInterval: opts.HeartbeatInterval,
		leaseTimeout:      opts.LeaseTimeout,
		holderMarkerPath:  filepath.Join(opts.HolderMarkerDir, "ha-holder-marker"),
		onLeaseLost:       opts.OnLeaseLost,
		done:              make(chan struct{}),
		now:               time.Now,
	}
	if m.heartbeatInterval <= 0 {
		m.heartbeatInterval = 15 * time.Second
	}
	if m.leaseTimeout <= 0 {
		m.leaseTimeout = 60 * time.Second
	}

	current, err := m.readLease(ctx)
	switch {
	case err != nil && errors.Is(err, os.ErrNotExist):
		// No lease: first gateway for this bucket.
	case err != nil:
		// Object store unreachable. Allow a degraded start only when this
		// node provably held the lease before (crash recovery during an
		// outage); a fresh standby must not promote blind.
		if !m.localMarkerPresent() && !opts.ForceTakeover {
			return nil, fmt.Errorf("cannot verify bucket lease (object store unreachable) and this node holds no local lease marker: %w", err)
		}
		logging.Error("Starting with unverified lease: object store unreachable and this node held the lease before (or takeover forced)",
			zap.Error(err))
	default:
		fresh := time.Since(current.RenewedAt) < m.leaseTimeout
		if fresh && !opts.ForceTakeover && !m.localMarkerMatches(current.HolderID) {
			return nil, &ErrLeaseHeld{Lease: *current}
		}
		if fresh && opts.ForceTakeover {
			logging.Error("Force takeover of a fresh lease; ensure the previous holder is stopped",
				zap.String("previous_holder", current.HolderID),
				zap.String("previous_host", current.Hostname))
		}
		m.lease.Epoch = current.Epoch
	}

	m.lease = Lease{
		HolderID:   m.holderID,
		Hostname:   m.hostname,
		Epoch:      m.lease.Epoch + 1,
		AcquiredAt: m.now(),
		RenewedAt:  m.now(),
	}
	// The clock for "unable to prove we hold the lease" runs from here, so
	// a node whose first write fails is still fenced once the lease times
	// out rather than serving on indefinitely.
	m.lastRenewed = m.now()
	if err := m.writeLease(ctx); err != nil {
		if !m.localMarkerPresent() && !opts.ForceTakeover {
			return nil, fmt.Errorf("failed to write bucket lease: %w", err)
		}
		logging.Error("Could not persist lease at startup; heartbeat will retry", zap.Error(err))
	}
	m.writeLocalMarker()

	hbCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go m.heartbeatLoop(hbCtx)

	logging.Info("HA lease acquired",
		zap.String("holder_id", m.holderID),
		zap.Uint64("epoch", m.lease.Epoch),
		zap.Duration("heartbeat_interval", m.heartbeatInterval),
		zap.Duration("lease_timeout", m.leaseTimeout))
	return m, nil
}

func (m *Manager) readLease(ctx context.Context) (*Lease, error) {
	data, err := m.store.GetObject(ctx, LeaseObjectKey)
	if err != nil {
		return nil, err
	}
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		// A corrupt lease is treated as absent but logged: better one loud
		// takeover than a permanently fenced bucket.
		logging.Error("Corrupt HA lease object; treating as absent", zap.Error(err))
		return nil, os.ErrNotExist
	}
	return &lease, nil
}

func (m *Manager) writeLease(ctx context.Context) error {
	m.mu.Lock()
	m.lease.RenewedAt = m.now()
	payload, err := json.MarshalIndent(m.lease, "", "  ")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if err := m.store.PutObject(ctx, LeaseObjectKey, payload, map[string]string{}); err != nil {
		return err
	}
	m.mu.Lock()
	m.lastRenewed = m.now()
	m.mu.Unlock()
	return nil
}

func (m *Manager) heartbeatLoop(ctx context.Context) {
	defer close(m.done)
	ticker := time.NewTicker(m.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbCtx, cancel := context.WithTimeout(ctx, m.heartbeatInterval)
			// Theft: another holder owns the lease, so this node must stop
			// serving the bucket. Two gateways writing one bucket is what
			// the lease exists to prevent.
			if current, err := m.readLease(hbCtx); err == nil && current.HolderID != m.holderID {
				logging.Error("HA LEASE LOST: another gateway took over this bucket",
					zap.String("taken_by", current.HolderID),
					zap.String("taken_by_host", current.Hostname))
				taken := *current
				m.reportLoss(LeaseLoss{Reason: "taken", TakenBy: &taken})
				cancel()
				continue
			}
			if err := m.writeLease(hbCtx); err != nil {
				failingFor := m.renewFailingFor()
				logging.Error("HA lease heartbeat failed; standby may take over after the lease timeout",
					zap.Error(err),
					zap.Duration("failing_for", failingFor),
					zap.Duration("lease_timeout", m.leaseTimeout))
				// Past the lease timeout a standby is entitled to promote,
				// so this node can no longer claim the bucket is its own.
				if failingFor > m.leaseTimeout {
					m.reportLoss(LeaseLoss{Reason: "unreachable", Since: failingFor, Err: err})
				}
			}
			cancel()
		}
	}
}

// renewFailingFor reports how long it has been since the lease was last
// written successfully.
func (m *Manager) renewFailingFor() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastRenewed.IsZero() {
		return 0
	}
	return m.now().Sub(m.lastRenewed)
}

// reportLoss tells the application once that this node no longer holds the
// lease. Without a handler the node keeps serving and the error log is the
// only warning, which is what it did before fencing existed.
func (m *Manager) reportLoss(loss LeaseLoss) {
	if !m.lost.CompareAndSwap(false, true) {
		return
	}
	if m.onLeaseLost == nil {
		return
	}
	m.onLeaseLost(loss)
}

func (m *Manager) localMarkerPresent() bool {
	// The marker must name THIS host: staging replication can copy the
	// primary's marker onto a standby, and existence alone would let that
	// standby start blind during an object-store outage.
	data, err := os.ReadFile(m.holderMarkerPath)
	return err == nil && string(data) == m.hostname
}

func (m *Manager) localMarkerMatches(holderID string) bool {
	data, err := os.ReadFile(m.holderMarkerPath)
	if err != nil {
		return false
	}
	// Marker stores the node hostname: any previous holder from this same
	// node allows recovery (the process identity changes across restarts).
	return string(data) == m.hostname && holderID != "" && leaseFromSameHost(holderID, m.hostname)
}

func leaseFromSameHost(holderID, hostname string) bool {
	return len(holderID) > len(hostname) && holderID[:len(hostname)] == hostname
}

func (m *Manager) writeLocalMarker() {
	if err := os.MkdirAll(filepath.Dir(m.holderMarkerPath), 0700); err != nil {
		logging.Error("Failed to create HA marker directory", zap.Error(err))
		return
	}
	if err := os.WriteFile(m.holderMarkerPath, []byte(m.hostname), 0600); err != nil {
		logging.Error("Failed to write HA holder marker", zap.Error(err))
	}
}

// Release stops the heartbeat and deletes the lease so a standby can promote
// immediately on graceful shutdown.
func (m *Manager) Release() {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Never delete a lease this node does not hold: after a takeover the
	// object belongs to the gateway that now serves the bucket, and
	// deleting it would let a third one start.
	if current, err := m.readLease(ctx); err == nil && current.HolderID != m.holderID {
		logging.Warn("Not releasing the HA lease: another gateway holds it now",
			zap.String("held_by", current.HolderID))
		return
	}
	if err := m.store.DeleteObject(ctx, LeaseObjectKey); err != nil {
		logging.Error("Failed to release HA lease; standby promotion will wait for the lease timeout",
			zap.Error(err))
		return
	}
	logging.Info("HA lease released")
}

// HolderID exposes this node's lease identity (for status endpoints).
func (m *Manager) HolderID() string {
	return m.holderID
}

// Made with Bob
