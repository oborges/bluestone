package lock

import (
	"fmt"
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/pkg/types"
	"go.uber.org/zap"
)

// Manager manages file locks
type Manager struct {
	locks         map[string]*types.Lock
	mu            sync.RWMutex
	defaultTTL    time.Duration
	cleanupTicker *time.Ticker
	stopCleanup   chan struct{}
	closed        bool
	closeMu       sync.Mutex
}

// NewManager creates a new lock manager
func NewManager(defaultTTL time.Duration) *Manager {
	m := &Manager{
		locks:       make(map[string]*types.Lock),
		defaultTTL:  defaultTTL,
		stopCleanup: make(chan struct{}),
	}

	// Start cleanup goroutine
	m.cleanupTicker = time.NewTicker(defaultTTL / 2)
	go m.cleanupExpired()

	logging.Info("Lock manager initialized", zap.Duration("defaultTTL", defaultTTL))
	return m
}

// AcquireLock attempts to acquire a lock on a path
func (m *Manager) AcquireLock(path string, lockType types.LockType, owner string, timeout time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	log := logging.WithOperation("AcquireLock").With(
		zap.String("path", path),
		zap.String("owner", owner),
		zap.Int("lockType", int(lockType)),
	)

	// Check if lock exists
	if existingLock, exists := m.locks[path]; exists {
		// Check if expired
		if time.Now().After(existingLock.ExpiresAt) {
			// Lock expired, remove it
			delete(m.locks, path)
			log.Debug("Expired lock removed")
		} else {
			// Lock still valid
			if existingLock.Owner == owner {
				// Same owner, renew the lock
				existingLock.ExpiresAt = time.Now().Add(timeout)
				log.Debug("Lock renewed")
				return nil
			}

			// Different owner
			if lockType == types.LockTypeExclusive || existingLock.Type == types.LockTypeExclusive {
				log.Debug("Lock conflict",
					zap.String("existingOwner", existingLock.Owner),
					zap.Int("existingType", int(existingLock.Type)),
				)
				return fmt.Errorf("lock held by another owner")
			}

			// Both are shared locks, allow
			log.Debug("Shared lock granted")
		}
	}

	// Create new lock
	lock := &types.Lock{
		Type:      lockType,
		Owner:     owner,
		ExpiresAt: time.Now().Add(timeout),
	}
	m.locks[path] = lock

	log.Info("Lock acquired")
	return nil
}

// ReleaseLock releases a lock
func (m *Manager) ReleaseLock(path string, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	log := logging.WithOperation("ReleaseLock").With(
		zap.String("path", path),
		zap.String("owner", owner),
	)

	lock, exists := m.locks[path]
	if !exists {
		log.Debug("Lock not found")
		return fmt.Errorf("lock not found")
	}

	if lock.Owner != owner {
		log.Warn("Lock owner mismatch", zap.String("lockOwner", lock.Owner))
		return fmt.Errorf("not lock owner")
	}

	delete(m.locks, path)
	log.Info("Lock released")
	return nil
}

// RenewLock renews a lock's expiration time
func (m *Manager) RenewLock(path string, owner string, timeout time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	log := logging.WithOperation("RenewLock").With(
		zap.String("path", path),
		zap.String("owner", owner),
	)

	lock, exists := m.locks[path]
	if !exists {
		log.Debug("Lock not found")
		return fmt.Errorf("lock not found")
	}

	if lock.Owner != owner {
		log.Warn("Lock owner mismatch", zap.String("lockOwner", lock.Owner))
		return fmt.Errorf("not lock owner")
	}

	lock.ExpiresAt = time.Now().Add(timeout)
	log.Debug("Lock renewed", zap.Time("expiresAt", lock.ExpiresAt))
	return nil
}

// CheckLock checks if a path is locked
func (m *Manager) CheckLock(path string) (*types.Lock, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	lock, exists := m.locks[path]
	if !exists {
		return nil, false
	}

	// Check if expired
	if time.Now().After(lock.ExpiresAt) {
		return nil, false
	}

	return lock, true
}

// IsLocked checks if a path is currently locked
func (m *Manager) IsLocked(path string) bool {
	_, locked := m.CheckLock(path)
	return locked
}

// GetLockOwner returns the owner of a lock
func (m *Manager) GetLockOwner(path string) (string, bool) {
	lock, exists := m.CheckLock(path)
	if !exists {
		return "", false
	}
	return lock.Owner, true
}

// ListLocks returns all active locks
func (m *Manager) ListLocks() map[string]*types.Lock {
	m.mu.RLock()
	defer m.mu.RUnlock()

	locks := make(map[string]*types.Lock)
	now := time.Now()

	for path, lock := range m.locks {
		if now.Before(lock.ExpiresAt) {
			locks[path] = &types.Lock{
				Type:      lock.Type,
				Owner:     lock.Owner,
				ExpiresAt: lock.ExpiresAt,
			}
		}
	}

	return locks
}

// Stats returns lock statistics
func (m *Manager) Stats() LockStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := LockStats{
		TotalLocks: len(m.locks),
	}

	now := time.Now()
	for _, lock := range m.locks {
		if now.Before(lock.ExpiresAt) {
			stats.ActiveLocks++
			if lock.Type == types.LockTypeShared {
				stats.SharedLocks++
			} else {
				stats.ExclusiveLocks++
			}
		} else {
			stats.ExpiredLocks++
		}
	}

	return stats
}

// cleanupExpired removes expired locks periodically
func (m *Manager) cleanupExpired() {
	for {
		select {
		case <-m.cleanupTicker.C:
			m.removeExpiredLocks()
		case <-m.stopCleanup:
			m.cleanupTicker.Stop()
			return
		}
	}
}

// removeExpiredLocks removes all expired locks
func (m *Manager) removeExpiredLocks() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	count := 0

	for path, lock := range m.locks {
		if now.After(lock.ExpiresAt) {
			delete(m.locks, path)
			count++
		}
	}

	if count > 0 {
		logging.Debug("Expired locks cleaned", zap.Int("count", count))
	}
}

// Close stops the lock manager
func (m *Manager) Close() error {
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	
	if m.closed {
		return nil // Already closed
	}
	
	m.closed = true
	close(m.stopCleanup)
	
	if m.cleanupTicker != nil {
		m.cleanupTicker.Stop()
	}
	
	logging.Info("Lock manager closed")
	return nil
}

// LockStats represents lock statistics
type LockStats struct {
	TotalLocks     int
	ActiveLocks    int
	ExpiredLocks   int
	SharedLocks    int
	ExclusiveLocks int
}

// Made with Bob
