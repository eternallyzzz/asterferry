package node

import (
	"asterferry/internal/domain"
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"
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
	result := domain.RuntimeSnapshot{NodeID: nodeID, ObservedAt: now}
	t.mu.Lock()
	entries := make([]*runtimeConnection, 0, len(t.connections))
	for _, entry := range t.connections {
		entries = append(entries, entry)
	}
	result.Metrics.RuntimeOpenedTotal = t.opened
	result.Metrics.RuntimeClosedTotal = t.closed
	result.Metrics.RuntimeRejectedTotal = t.rejected
	result.Metrics.RuntimeRateLimitedTotal = t.rateLimited
	result.Metrics.RuntimeTelemetryDroppedTotal = t.dropped
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

func cumulativeByteRate(bytes uint64, startedAt, now time.Time) float64 {
	elapsed := now.Sub(startedAt)
	if elapsed < time.Millisecond {
		elapsed = time.Millisecond
	}
	return float64(bytes) / elapsed.Seconds()
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
	e.meta.RateIn = cumulativeByteRate(e.meta.BytesIn, e.meta.StartedAt, now)
	e.meta.RateOut = cumulativeByteRate(e.meta.BytesOut, e.meta.StartedAt, now)
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
