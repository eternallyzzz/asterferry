package observability

import (
	"sync"
	"time"
)

const (
	managementAuthFailureLimit = 8
	managementAuthWindow       = time.Minute
	managementAuthCooldown     = time.Minute
)

// authGuard is intentionally process-local. Management listeners are required
// to bind to loopback, so a per-address table would not distinguish clients
// using an administrative port forward. A small instance-wide window gives a
// bounded brute-force defense without adding configuration or persistence.
type authGuard struct {
	mu           sync.Mutex
	now          func() time.Time
	windowStart  time.Time
	failures     int
	blockedUntil time.Time
}

func newAuthGuard(now func() time.Time) *authGuard {
	if now == nil {
		now = time.Now
	}
	return &authGuard{now: now}
}

func (g *authGuard) blocked() (bool, time.Duration) {
	if g == nil {
		return false, 0
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blockedUntil.IsZero() || !now.Before(g.blockedUntil) {
		return false, 0
	}
	return true, g.blockedUntil.Sub(now)
}

// recordFailure returns true when this failure must be rate limited. Exactly
// managementAuthFailureLimit failures receive normal 401 responses; the next
// failure starts the cooldown and receives 429.
func (g *authGuard) recordFailure() (bool, time.Duration) {
	if g == nil {
		return false, 0
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.blockedUntil.IsZero() && now.Before(g.blockedUntil) {
		return true, g.blockedUntil.Sub(now)
	}
	if g.windowStart.IsZero() || !now.Before(g.windowStart.Add(managementAuthWindow)) {
		g.windowStart = now
		g.failures = 0
		g.blockedUntil = time.Time{}
	}
	if g.failures >= managementAuthFailureLimit {
		g.blockedUntil = now.Add(managementAuthCooldown)
		return true, managementAuthCooldown
	}
	g.failures++
	return false, 0
}

func (g *authGuard) reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.windowStart = time.Time{}
	g.failures = 0
	g.blockedUntil = time.Time{}
	g.mu.Unlock()
}
