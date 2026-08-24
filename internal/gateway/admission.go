package gateway

import (
	"net"
	"sort"
	"sync"
	"time"

	"asterferry/internal/transport"
)

const (
	authFailureWindow = 10 * time.Second
	maxAuthBuckets    = 4096
)

type authBucket struct {
	windowStart time.Time
	failures    int
	blockedTill time.Time
	lastSeen    time.Time
}

// handshakeAdmission keeps unauthenticated transport work bounded before it
// reaches the lifecycle/session registries. The semaphore is global, while
// the bounded failure table prevents one source from repeatedly consuming it.
type handshakeAdmission struct {
	pending chan struct{}
	mu      sync.Mutex
	buckets map[string]authBucket
	now     func() time.Time
}

func newHandshakeAdmission(limit int64) *handshakeAdmission {
	if limit < 1 {
		limit = 32
	}
	return &handshakeAdmission{pending: make(chan struct{}, limit), buckets: make(map[string]authBucket), now: time.Now}
}

func (a *handshakeAdmission) begin(conn transport.Session) (release func(), key string, ok bool) {
	if a == nil {
		return func() {}, remoteKey(conn), true
	}
	key = remoteKey(conn)
	if !a.allow(key) {
		return func() {}, key, false
	}
	select {
	case a.pending <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-a.pending }) }, key, true
	default:
		return func() {}, key, false
	}
}

func (a *handshakeAdmission) failure(key string) {
	if a == nil {
		return
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket, ok := a.buckets[key]
	if !ok || now.Sub(bucket.windowStart) >= authFailureWindow {
		bucket = authBucket{windowStart: now}
	}
	bucket.failures++
	bucket.lastSeen = now
	if bucket.failures >= 5 {
		shift := bucket.failures - 5
		if shift > 5 {
			shift = 5
		}
		delay := time.Duration(1<<shift) * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		bucket.blockedTill = now.Add(delay)
	}
	a.buckets[key] = bucket
	a.trimLocked(now)
}

func (a *handshakeAdmission) success(key string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.buckets, key)
	a.mu.Unlock()
}

func (a *handshakeAdmission) allow(key string) bool {
	if a == nil {
		return true
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket, ok := a.buckets[key]
	if !ok {
		a.trimLocked(now)
		return true
	}
	if now.Before(bucket.blockedTill) {
		return false
	}
	if now.Sub(bucket.windowStart) >= authFailureWindow {
		delete(a.buckets, key)
		return true
	}
	bucket.lastSeen = now
	a.buckets[key] = bucket
	return true
}

func (a *handshakeAdmission) trimLocked(now time.Time) {
	for key, bucket := range a.buckets {
		if now.Sub(bucket.lastSeen) >= authFailureWindow && !now.Before(bucket.blockedTill) {
			delete(a.buckets, key)
		}
	}
	if len(a.buckets) < maxAuthBuckets {
		return
	}
	keys := make([]string, 0, len(a.buckets))
	for key := range a.buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return a.buckets[keys[i]].lastSeen.Before(a.buckets[keys[j]].lastSeen) })
	for len(a.buckets) >= maxAuthBuckets && len(keys) > 0 {
		delete(a.buckets, keys[0])
		keys = keys[1:]
	}
}

func remoteKey(conn transport.Session) string {
	if conn == nil {
		return "unknown"
	}
	address := conn.RemoteAddr()
	if address == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(address.String())
	if err == nil && host != "" {
		return host
	}
	return address.String()
}
