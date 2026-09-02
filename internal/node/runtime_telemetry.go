package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"asterferry/internal/domain"
)

const (
	runtimeEventQueueLimit  = 4096
	runtimeConnectionLimit  = 4096
	runtimeActionMaxMatches = 4096
	defaultRuntimeLimitTTL  = time.Hour
)

// runtimeTelemetry is deliberately local to one node process.  It is a
// bounded, payload-free registry; the Controller owns durable history and
// queries.  Keeping the active registry here makes disconnect/limit commands
// precise even while a snapshot is being rebuilt.
type runtimeTelemetry struct {
	mu          sync.Mutex
	connections map[string]*runtimeConnection
	events      []domain.RuntimeEvent
	opened      uint64
	closed      uint64
	rejected    uint64
	rateLimited uint64
	dropped     uint64
}

type runtimeConnection struct {
	registry *runtimeTelemetry
	owner    *dataGeneration
	ctx      context.Context
	cancel   context.CancelFunc
	closer   func()

	mu       sync.Mutex
	meta     domain.RuntimeConnection
	closed   bool
	limitIn  *runtimeLimiter
	limitOut *runtimeLimiter
}

func (e *runtimeConnection) id() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.meta.ID
}

func newRuntimeTelemetry() *runtimeTelemetry {
	return &runtimeTelemetry{connections: make(map[string]*runtimeConnection)}
}

func runtimeConnectionID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failures are exceptionally unusual.  The timestamp keeps
		// the fallback unique enough for the in-process registry while retaining
		// the normal ID character set.
		return "rt-" + hex.EncodeToString([]byte(time.Now().UTC().Format("20060102150405.000000000")))
	}
	return "rt-" + hex.EncodeToString(raw[:])
}

func (t *runtimeTelemetry) open(ctx context.Context, owner *dataGeneration, meta domain.RuntimeConnection, closer func()) *runtimeConnection {
	if t == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	connectionCtx, cancel := context.WithCancel(ctx)
	now := time.Now().UTC()
	if meta.ID == "" {
		meta.ID = runtimeConnectionID()
	}
	meta.State = domain.RuntimeStateActive
	if meta.StartedAt.IsZero() {
		meta.StartedAt = now
	}
	if meta.LastActivityAt.IsZero() {
		meta.LastActivityAt = now
	}
	entry := &runtimeConnection{registry: t, owner: owner, ctx: connectionCtx, cancel: cancel, closer: closer, meta: meta}
	t.mu.Lock()
	if len(t.connections) >= runtimeConnectionLimit {
		t.dropped++
		t.mu.Unlock()
		cancel()
		return nil
	}
	t.connections[meta.ID] = entry
	t.opened++
	connectionCopy := cloneRuntimeConnection(meta)
	t.appendEventLocked(domain.RuntimeEvent{ID: runtimeConnectionID(), Type: domain.RuntimeEventOpened, NodeID: meta.NodeID, ConnectionID: meta.ID, Connection: &connectionCopy, CreatedAt: now})
	t.mu.Unlock()
	return entry
}

func (t *runtimeTelemetry) recordRejected(nodeID, connectionID, message string) {
	if t == nil || nodeID == "" {
		return
	}
	t.mu.Lock()
	t.rejected++
	t.appendEventLocked(domain.RuntimeEvent{ID: runtimeConnectionID(), Type: domain.RuntimeEventRejected, NodeID: nodeID, ConnectionID: connectionID, Message: message, CreatedAt: time.Now().UTC()})
	t.mu.Unlock()
}

func (t *runtimeTelemetry) appendEventLocked(event domain.RuntimeEvent) {
	if len(t.events) >= runtimeEventQueueLimit {
		copy(t.events, t.events[1:])
		t.events = t.events[:runtimeEventQueueLimit-1]
		t.dropped++
	}
	t.events = append(t.events, event)
}

func (t *runtimeTelemetry) drainEvents(max int) []domain.RuntimeEvent {
	events := t.peekEvents(max)
	if len(events) == 0 {
		return nil
	}
	t.ackEvents(events)
	return events
}

func (t *runtimeTelemetry) peekEvents(max int) []domain.RuntimeEvent {
	if t == nil || max <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.events) == 0 {
		return nil
	}
	if max > len(t.events) {
		max = len(t.events)
	}
	result := make([]domain.RuntimeEvent, max)
	for i := range result {
		result[i] = cloneRuntimeEvent(t.events[i])
	}
	return result
}

// ackEvents removes only the exact prefix returned by peekEvents. Events are
// acknowledged after stream.Send succeeds; a failed control stream therefore
// leaves them available for the next authenticated connection.
func (t *runtimeTelemetry) ackEvents(events []domain.RuntimeEvent) {
	if t == nil || len(events) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(events) > len(t.events) {
		return
	}
	for i, event := range events {
		if t.events[i].ID != event.ID {
			return
		}
	}
	copy(t.events, t.events[len(events):])
	t.events = t.events[:len(t.events)-len(events)]
}

func cloneRuntimeEvent(value domain.RuntimeEvent) domain.RuntimeEvent {
	clone := value
	if value.Connection != nil {
		connection := cloneRuntimeConnection(*value.Connection)
		clone.Connection = &connection
	}
	return clone
}

func (t *runtimeTelemetry) snapshot(nodeID string) domain.RuntimeSnapshot {
	now := time.Now().UTC()
	result := domain.RuntimeSnapshot{NodeID: nodeID, ObservedAt: now, Metrics: make(map[string]float64)}
	t.mu.Lock()
	entries := make([]*runtimeConnection, 0, len(t.connections))
	for _, entry := range t.connections {
		entries = append(entries, entry)
	}
	result.Metrics["runtime_opened_total"] = float64(t.opened)
	result.Metrics["runtime_closed_total"] = float64(t.closed)
	result.Metrics["runtime_rejected_total"] = float64(t.rejected)
	result.Metrics["runtime_rate_limited_total"] = float64(t.rateLimited)
	result.Metrics["runtime_telemetry_dropped_total"] = float64(t.dropped)
	t.mu.Unlock()
	result.Connections = make([]domain.RuntimeConnection, 0, len(entries))
	for _, entry := range entries {
		if value, ok := entry.snapshot(); ok {
			result.Connections = append(result.Connections, value)
		}
	}
	return result
}

func (e *runtimeConnection) snapshot() (domain.RuntimeConnection, bool) {
	if e == nil {
		return domain.RuntimeConnection{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return domain.RuntimeConnection{}, false
	}
	value := cloneRuntimeConnection(e.meta)
	value.BytesIn = e.meta.BytesIn
	value.BytesOut = e.meta.BytesOut
	return value, true
}

func cloneRuntimeConnection(value domain.RuntimeConnection) domain.RuntimeConnection {
	clone := value
	if value.EndedAt != nil {
		ended := *value.EndedAt
		clone.EndedAt = &ended
	}
	if value.Limit != nil {
		limit := *value.Limit
		clone.Limit = &limit
	}
	return clone
}

func (e *runtimeConnection) touch(in, out uint64) {
	if e == nil {
		return
	}
	now := time.Now().UTC()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.meta.BytesIn += in
	e.meta.BytesOut += out
	e.meta.LastActivityAt = now
	elapsed := now.Sub(e.meta.StartedAt).Seconds()
	if elapsed < 0.001 {
		elapsed = 0.001
	}
	e.meta.RateIn = float64(e.meta.BytesIn) / elapsed
	e.meta.RateOut = float64(e.meta.BytesOut) / elapsed
	e.mu.Unlock()
}

func (e *runtimeConnection) limiter(direction string) *runtimeLimiter {
	if e == nil {
		return nil
	}
	now := time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.meta.Limit == nil || (!e.meta.Limit.ExpiresAt.IsZero() && !now.Before(e.meta.Limit.ExpiresAt)) {
		if e.meta.Limit != nil && !now.Before(e.meta.Limit.ExpiresAt) {
			e.limitIn, e.limitOut, e.meta.Limit = nil, nil, nil
		}
		return nil
	}
	if direction == "in" {
		return e.limitIn
	}
	if direction == "out" {
		return e.limitOut
	}
	return nil
}

func (e *runtimeConnection) setLimit(limit domain.RuntimeRateLimit) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.meta.Limit = &limit
	e.limitIn, e.limitOut = nil, nil
	if limit.Direction == "in" || limit.Direction == "both" {
		e.limitIn = newRuntimeLimiter(limit.BytesPerSecond, limit.BurstBytes, e.ctx)
	}
	if limit.Direction == "out" || limit.Direction == "both" {
		e.limitOut = newRuntimeLimiter(limit.BytesPerSecond, limit.BurstBytes, e.ctx)
	}
}

func (e *runtimeConnection) clearLimit() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.limitIn, e.limitOut, e.meta.Limit = nil, nil, nil
	e.mu.Unlock()
}

func (e *runtimeConnection) close(reason string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	now := time.Now().UTC()
	e.meta.State = domain.RuntimeStateClosed
	e.meta.CloseReason = strings.TrimSpace(reason)
	if e.meta.CloseReason == "" {
		e.meta.CloseReason = domain.RuntimeCloseUnknown
	}
	e.meta.EndedAt = &now
	e.meta.LastActivityAt = now
	value := cloneRuntimeConnection(e.meta)
	closer, cancel := e.closer, e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	t := e.registry
	if t != nil {
		t.mu.Lock()
		if t.connections[e.meta.ID] == e {
			delete(t.connections, e.meta.ID)
		}
		t.closed++
		t.appendEventLocked(domain.RuntimeEvent{ID: runtimeConnectionID(), Type: domain.RuntimeEventClosed, NodeID: value.NodeID, ConnectionID: value.ID, Connection: &value, CreatedAt: now})
		t.mu.Unlock()
	}
	if closer != nil {
		closer()
	}
}

func (t *runtimeTelemetry) closeOwner(owner *dataGeneration, reason string) {
	if t == nil || owner == nil {
		return
	}
	t.mu.Lock()
	entries := make([]*runtimeConnection, 0)
	for _, entry := range t.connections {
		if entry != nil && entry.owner == owner {
			entries = append(entries, entry)
		}
	}
	t.mu.Unlock()
	for _, entry := range entries {
		entry.close(reason)
	}
}

type runtimeSelector struct {
	ConnectionID string `json:"connection_id,omitempty"`
	SourceIP     string `json:"source_ip,omitempty"`
	PeerNodeID   string `json:"peer_node_id,omitempty"`
	AssignmentID string `json:"assignment_id,omitempty"`
	ServiceID    string `json:"service_id,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
}

type runtimeActionRequest struct {
	Action         string          `json:"action"`
	Selector       runtimeSelector `json:"selector"`
	ConnectionID   string          `json:"connection_id,omitempty"`
	Direction      string          `json:"direction,omitempty"`
	BytesPerSecond uint64          `json:"bytes_per_second,omitempty"`
	BurstBytes     uint64          `json:"burst_bytes,omitempty"`
	TTLSeconds     int             `json:"ttl_seconds,omitempty"`
}

func (t *runtimeTelemetry) applyAction(ctx context.Context, name string, payload []byte) (int, error) {
	if t == nil {
		return 0, errors.New("runtime telemetry is unavailable")
	}
	var request runtimeActionRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &request); err != nil {
			return 0, errors.New("runtime action payload is invalid")
		}
	}
	if request.Action == "" {
		request.Action = name
	}
	if request.Action == "clear_runtime_controls" {
		t.mu.Lock()
		entries := make([]*runtimeConnection, 0, len(t.connections))
		for _, entry := range t.connections {
			entries = append(entries, entry)
		}
		t.mu.Unlock()
		for _, entry := range entries {
			entry.clearLimit()
		}
		return len(entries), nil
	}
	if request.ConnectionID != "" && request.Selector.ConnectionID == "" {
		request.Selector.ConnectionID = request.ConnectionID
	}
	if request.Action != "disconnect" && request.Action != "rate_limit" && request.Action != "clear_limit" {
		return 0, errors.New("runtime connection action is unsupported")
	}
	if request.Action == "rate_limit" {
		if request.Direction == "" {
			request.Direction = "both"
		}
		if request.BytesPerSecond == 0 || request.BurstBytes == 0 {
			return 0, errors.New("runtime rate limit requires bytes_per_second and burst_bytes")
		}
		if request.TTLSeconds <= 0 {
			request.TTLSeconds = int(defaultRuntimeLimitTTL / time.Second)
		}
		if request.TTLSeconds > 24*60*60 {
			return 0, errors.New("runtime rate limit ttl is too long")
		}
	}
	if request.Selector.SourceIP != "" {
		request.Selector.SourceIP = normalizedRuntimeIP(request.Selector.SourceIP)
	}
	entries := t.match(request.Selector)
	if len(entries) > runtimeActionMaxMatches {
		return 0, errors.New("runtime action matches too many connections")
	}
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		switch request.Action {
		case "disconnect":
			entry.close(domain.RuntimeCloseOperator)
		case "clear_limit":
			entry.clearLimit()
		case "rate_limit":
			expires := time.Now().UTC().Add(time.Duration(request.TTLSeconds) * time.Second)
			entry.setLimit(domain.RuntimeRateLimit{Direction: request.Direction, BytesPerSecond: request.BytesPerSecond, BurstBytes: request.BurstBytes, ExpiresAt: expires})
			t.recordLimitEvent(entry)
		}
	}
	return len(entries), nil
}

func (t *runtimeTelemetry) match(selector runtimeSelector) []*runtimeConnection {
	t.mu.Lock()
	entries := make([]*runtimeConnection, 0, len(t.connections))
	for _, entry := range t.connections {
		if entry.matches(selector) {
			entries = append(entries, entry)
		}
	}
	t.mu.Unlock()
	return entries
}

func (e *runtimeConnection) matches(selector runtimeSelector) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	value := e.meta
	return (selector.ConnectionID == "" || value.ID == selector.ConnectionID) &&
		(selector.SourceIP == "" || value.SourceIP == selector.SourceIP) &&
		(selector.PeerNodeID == "" || value.PeerNodeID == selector.PeerNodeID) &&
		(selector.AssignmentID == "" || value.AssignmentID == selector.AssignmentID) &&
		(selector.ServiceID == "" || value.ServiceID == selector.ServiceID) &&
		(selector.Protocol == "" || value.Protocol == selector.Protocol)
}

func (t *runtimeTelemetry) recordLimitEvent(entry *runtimeConnection) {
	if t == nil || entry == nil {
		return
	}
	value, ok := entry.snapshot()
	if !ok {
		return
	}
	t.mu.Lock()
	t.rateLimited++
	t.appendEventLocked(domain.RuntimeEvent{ID: runtimeConnectionID(), Type: domain.RuntimeEventRateLimited, NodeID: value.NodeID, ConnectionID: value.ID, Connection: &value, CreatedAt: time.Now().UTC()})
	t.mu.Unlock()
}

func normalizedRuntimeIP(value string) string {
	address, err := netipParse(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return address
}

// netipParse is kept tiny so the selector code does not accept arbitrary
// strings as an IP while avoiding a second copy of domain's internal helper.
func netipParse(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := netip.ParseAddr(value)
	if err != nil {
		return "", err
	}
	return parsed.Unmap().String(), nil
}

func runtimeAddr(addr net.Addr) (string, uint16) {
	if addr == nil {
		return "", 0
	}
	host, portText, err := net.SplitHostPort(addr.String())
	if err != nil {
		return normalizedRuntimeIP(addr.String()), 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return normalizedRuntimeIP(host), 0
	}
	return normalizedRuntimeIP(host), uint16(port)
}

// runtimeLimiter is a small token bucket.  It intentionally uses bytes, not
// packets, and wakes on the owning connection context so an operator
// disconnect cannot leave a writer asleep until its next refill.
type runtimeLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	ctx    context.Context
}

func newRuntimeLimiter(rate, burst uint64, ctx context.Context) *runtimeLimiter {
	if rate == 0 {
		return nil
	}
	if burst == 0 {
		burst = rate
	}
	if burst > math.MaxInt64 {
		burst = math.MaxInt64
	}
	return &runtimeLimiter{rate: float64(rate), burst: float64(burst), tokens: float64(burst), last: time.Now(), ctx: ctx}
}

func (l *runtimeLimiter) wait(bytes int) error {
	if l == nil || bytes <= 0 {
		return nil
	}
	remaining := bytes
	for remaining > 0 {
		chunk := remaining
		if maximum := int(l.burst); maximum > 0 && chunk > maximum {
			chunk = maximum
		}
		for {
			l.mu.Lock()
			now := time.Now()
			elapsed := now.Sub(l.last).Seconds()
			if elapsed > 0 {
				l.tokens += elapsed * l.rate
				if l.tokens > l.burst {
					l.tokens = l.burst
				}
				l.last = now
			}
			if l.tokens >= float64(chunk) {
				l.tokens -= float64(chunk)
				l.mu.Unlock()
				break
			}
			wait := time.Duration((float64(chunk) - l.tokens) / l.rate * float64(time.Second))
			if wait < time.Millisecond {
				wait = time.Millisecond
			}
			l.mu.Unlock()
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-l.ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return l.ctx.Err()
			}
		}
		remaining -= chunk
	}
	return nil
}

type runtimeTrackedRWC struct {
	inner      io.ReadWriteCloser
	onRead     func(int)
	onWrite    func(int)
	writeLimit func() *runtimeLimiter
	closeOnce  sync.Once
}

func newRuntimeTrackedRWC(inner io.ReadWriteCloser, onRead, onWrite func(int), writeLimit func() *runtimeLimiter) *runtimeTrackedRWC {
	return &runtimeTrackedRWC{inner: inner, onRead: onRead, onWrite: onWrite, writeLimit: writeLimit}
}

func (c *runtimeTrackedRWC) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	if n > 0 && c.onRead != nil {
		c.onRead(n)
	}
	return n, err
}

func (c *runtimeTrackedRWC) Write(p []byte) (int, error) {
	if c.writeLimit != nil {
		if limit := c.writeLimit(); limit != nil {
			if err := limit.wait(len(p)); err != nil {
				return 0, err
			}
		}
	}
	n, err := c.inner.Write(p)
	if n > 0 && c.onWrite != nil {
		c.onWrite(n)
	}
	return n, err
}

func (c *runtimeTrackedRWC) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.inner.Close() })
	return err
}

func (c *runtimeTrackedRWC) CloseWrite() error {
	if closer, ok := c.inner.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return errors.New("tracked endpoint does not support write half-close")
}

func (c *runtimeTrackedRWC) Abort() error {
	if aborter, ok := c.inner.(interface{ Abort() error }); ok {
		return aborter.Abort()
	}
	return c.Close()
}
