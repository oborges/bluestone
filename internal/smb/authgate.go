package smb

import (
	"net"
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/metrics"
	"go.uber.org/zap"
)

// AuthLimits slow down password guessing. Zero values select the defaults.
type AuthLimits struct {
	// Failures is how many failed attempts from one client start a block.
	Failures int
	// Window is how long failures are remembered: a client that stops
	// failing for this long starts again with a clean record.
	Window time.Duration
	// Block is how long a client is refused once it reaches Failures. Each
	// further failure while blocked doubles the wait, up to MaxBlock.
	Block time.Duration
	// MaxBlock caps that doubling.
	MaxBlock time.Duration
}

// Defaults for AuthLimits: an interactive user who mistypes a password a few
// times is not locked out, while a script trying passwords is slowed to a
// crawl within seconds.
const (
	DefaultAuthFailures = 5
	DefaultAuthWindow   = 5 * time.Minute
	DefaultAuthBlock    = 30 * time.Second
	DefaultAuthMaxBlock = 15 * time.Minute
)

func (l AuthLimits) withDefaults() AuthLimits {
	if l.Failures <= 0 {
		l.Failures = DefaultAuthFailures
	}
	if l.Window <= 0 {
		l.Window = DefaultAuthWindow
	}
	if l.Block <= 0 {
		l.Block = DefaultAuthBlock
	}
	if l.MaxBlock < l.Block {
		l.MaxBlock = max(DefaultAuthMaxBlock, l.Block)
	}
	return l
}

// authGate refuses authentication from a client that keeps failing, so a
// password-guessing client is slowed down without locking out the account
// itself: the block is per client address, and a client that stops failing
// recovers on its own.
//
// Blocking by address is a blunt instrument behind NAT, where many users
// share one address. The window and counts are set so that ordinary retries
// never reach the threshold.
type authGate struct {
	limits AuthLimits
	logger *zap.Logger
	now    func() time.Time

	mu      sync.Mutex
	clients map[string]*authRecord
}

type authRecord struct {
	failures    int
	lastFailure time.Time
	blockedFor  time.Duration
	blockedTill time.Time
}

func newAuthGate(limits AuthLimits, logger *zap.Logger) *authGate {
	return &authGate{
		limits:  limits.withDefaults(),
		logger:  logger,
		now:     time.Now,
		clients: make(map[string]*authRecord),
	}
}

// Allow implements server.AuthGate.
func (g *authGate) Allow(remoteAddr string) bool {
	client := hostOf(remoteAddr)
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()
	record := g.clients[client]
	if record == nil {
		return true
	}
	if now.Before(record.blockedTill) {
		metrics.RecordSMBAuthBlocked()
		return false
	}
	// The record has served its purpose once the block has passed and the
	// failures are older than the window.
	if now.Sub(record.lastFailure) > g.limits.Window {
		delete(g.clients, client)
	}
	return true
}

// Attempted implements server.AuthGate.
func (g *authGate) Attempted(remoteAddr string, ok bool, _ time.Duration) {
	client := hostOf(remoteAddr)
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()
	if ok {
		delete(g.clients, client)
		return
	}

	metrics.RecordSMBAuthFailure()
	record := g.clients[client]
	if record == nil || now.Sub(record.lastFailure) > g.limits.Window {
		record = &authRecord{}
		g.clients[client] = record
	}
	record.failures++
	record.lastFailure = now
	if record.failures < g.limits.Failures {
		return
	}

	// Each block after the first doubles, so a client that keeps trying
	// waits longer and longer.
	if record.blockedFor == 0 {
		record.blockedFor = g.limits.Block
	} else if record.blockedFor < g.limits.MaxBlock {
		record.blockedFor = min(record.blockedFor*2, g.limits.MaxBlock)
	}
	record.blockedTill = now.Add(record.blockedFor)
	record.failures = 0
	g.logger.Warn("Blocking SMB authentication after repeated failures",
		zap.String("client", client),
		zap.Duration("for", record.blockedFor))
}

// blockedClients reports how many clients are currently blocked, and drops
// records that have expired. It is called on a timer so the map does not
// grow with clients that have gone away.
func (g *authGate) blockedClients() int {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()

	blocked := 0
	for client, record := range g.clients {
		switch {
		case now.Before(record.blockedTill):
			blocked++
		case now.Sub(record.lastFailure) > g.limits.Window:
			delete(g.clients, client)
		}
	}
	return blocked
}

// hostOf strips the port from an address, so a client is recognised across
// its connections.
func hostOf(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}
