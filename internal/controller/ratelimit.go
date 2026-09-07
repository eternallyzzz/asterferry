package controller

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	loginFailureLimit = 5
	loginWindow       = time.Minute
	loginBlock        = time.Minute
	loginMaxEntries   = 10_000
	loginIdleExpiry   = 10 * time.Minute
	loginPurgeBatch   = 64
)

type loginBucket struct {
	failures    int
	windowStart time.Time
	blockedTill time.Time
	lastSeen    time.Time
}

type loginLimiter struct {
	mu         sync.Mutex
	entries    map[string]loginBucket
	now        func() time.Time
	maxEntries int
	failureCap int
	window     time.Duration
	block      time.Duration
	idleExpiry time.Duration
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: make(map[string]loginBucket), now: time.Now, maxEntries: loginMaxEntries, failureCap: loginFailureLimit, window: loginWindow, block: loginBlock, idleExpiry: loginIdleExpiry}
}

func (l *loginLimiter) allow(keys ...string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.purgeSomeLocked(now)
	var retry time.Duration
	for _, key := range uniqueKeys(keys) {
		bucket, ok := l.entries[key]
		if !ok {
			continue
		}
		if !bucket.blockedTill.After(now) {
			if !bucket.windowStart.IsZero() && now.Sub(bucket.windowStart) >= l.window {
				delete(l.entries, key)
			}
			continue
		}
		if wait := bucket.blockedTill.Sub(now); wait > retry {
			retry = wait
		}
	}
	return retry == 0, retry
}

func (l *loginLimiter) failure(keys ...string) {
	if l == nil {
		return
	}
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.purgeSomeLocked(now)
	for _, key := range uniqueKeys(keys) {
		bucket, exists := l.entries[key]
		if !exists {
			l.evictForInsertLocked()
		}
		if bucket.windowStart.IsZero() || now.Sub(bucket.windowStart) >= l.window {
			bucket = loginBucket{windowStart: now}
		}
		bucket.failures++
		bucket.lastSeen = now
		if bucket.failures >= l.failureCap {
			bucket.blockedTill = now.Add(l.block)
		}
		l.entries[key] = bucket
	}
}

func (l *loginLimiter) success(keys ...string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range uniqueKeys(keys) {
		delete(l.entries, key)
	}
}

func (l *loginLimiter) clock() time.Time {
	if l.now == nil {
		return time.Now().UTC()
	}
	return l.now().UTC()
}

// purgeSomeLocked deliberately bounds work performed while holding the
// limiter mutex. Both the IP and username dimensions are request-derived, so
// an attacker can otherwise force every login attempt to scan all 10,000
// buckets before the actual rate-limit decision.
func (l *loginLimiter) purgeSomeLocked(now time.Time) {
	checked := 0
	for key, bucket := range l.entries {
		if checked >= loginPurgeBatch {
			break
		}
		checked++
		if !bucket.lastSeen.IsZero() && now.Sub(bucket.lastSeen) >= l.idleExpiry {
			delete(l.entries, key)
			continue
		}
		if !bucket.blockedTill.After(now) && !bucket.windowStart.IsZero() && now.Sub(bucket.windowStart) >= l.window {
			delete(l.entries, key)
		}
	}
}

func (l *loginLimiter) evictForInsertLocked() {
	if l.maxEntries <= 0 || len(l.entries) < l.maxEntries {
		return
	}
	// A bounded limiter does not need strict LRU ordering. Evicting the first
	// map entry keeps the critical section O(1) even when the cap is full.
	for key := range l.entries {
		delete(l.entries, key)
		return
	}
}

func uniqueKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

func loginKeys(r *http.Request, username string) []string {
	keys := []string{}
	if r != nil {
		if address := directPeerAddress(r.RemoteAddr); address != "" {
			keys = append(keys, "ip:"+address)
		}
	}
	if normalized := strings.ToLower(strings.TrimSpace(username)); normalized != "" {
		keys = append(keys, "user:"+normalized)
	}
	return keys
}

func directPeerAddress(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	if parsed := net.ParseIP(remote); parsed != nil {
		return parsed.String()
	}
	if len(remote) > 256 || strings.ContainsAny(remote, "\r\n") {
		return ""
	}
	return remote
}
