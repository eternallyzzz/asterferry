package controller

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/peer"
)

type admissionBucket struct {
	windowStart time.Time
	count       int
	lastSeen    time.Time
}

type admissionLimiter struct {
	mu         sync.Mutex
	entries    map[string]admissionBucket
	limit      int
	window     time.Duration
	maxEntries int
}

func newAdmissionLimiter(limit int, window time.Duration, maxEntries int) *admissionLimiter {
	return &admissionLimiter{entries: make(map[string]admissionBucket), limit: limit, window: window, maxEntries: maxEntries}
}

func (l *admissionLimiter) allow(key string) (bool, time.Duration) {
	if l == nil || l.limit <= 0 || l.window <= 0 {
		return true, 0
	}
	if strings.TrimSpace(key) == "" {
		key = "unknown"
	}
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) >= l.maxEntries {
		for oldKey, bucket := range l.entries {
			if now.Sub(bucket.lastSeen) >= l.window {
				delete(l.entries, oldKey)
			}
		}
		if len(l.entries) >= l.maxEntries {
			for oldKey := range l.entries {
				delete(l.entries, oldKey)
				break
			}
		}
	}
	bucket := l.entries[key]
	if bucket.windowStart.IsZero() || now.Sub(bucket.windowStart) >= l.window {
		bucket = admissionBucket{windowStart: now}
	}
	bucket.lastSeen = now
	if bucket.count >= l.limit {
		retry := l.window - now.Sub(bucket.windowStart)
		if retry < time.Second {
			retry = time.Second
		}
		l.entries[key] = bucket
		return false, retry
	}
	bucket.count++
	l.entries[key] = bucket
	return true, 0
}

func peerAddressKey(ctx context.Context) string {
	if ctx == nil {
		return "unknown"
	}
	info, ok := peer.FromContext(ctx)
	if !ok || info == nil || info.Addr == nil {
		return "unknown"
	}
	address := info.Addr.String()
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}
