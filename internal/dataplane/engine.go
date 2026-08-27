// Package dataplane is the Controller-independent data-plane engine. It
// accepts already validated snapshots and exposes only session/open admission
// to the transport adapters. There is deliberately no HTTP, YAML, Dashboard,
// or controller import in this package.
package dataplane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"asterferry/internal/afdp"
	"asterferry/internal/domain"
)

type Options struct {
	Role        string
	NodeID      string
	MaxStreams  int
	MaxSessions int
}

type Engine struct {
	role        string
	nodeID      string
	maxStreams  int
	maxSessions int
	draining    atomic.Bool
	closed      atomic.Bool
	mu          sync.RWMutex
	generation  uint64
	// epoch changes on every snapshot activation, including a same-generation
	// resync.  Runtime leases capture it so late cleanup from an old listener
	// set cannot decrement the active counter of a replacement set.
	epoch          uint64
	gatewaySpec    *domain.GatewaySpec
	agentSpec      *domain.AgentSpec
	assignments    map[string]afdp.AssignmentView
	services       map[string]domain.Service
	active         map[string]int
	baseMaxStreams int
	// connectionLimit is the node-scoped Agent limit (or Gateway capacity
	// limit when the snapshot is gateway-scoped). Every AFDP Open represents
	// one live TCP connection or UDP flow, so the existing stream reservation
	// is also the bounded connection admission counter.
	connectionLimit int
	bufferLimit     int
	streams         atomic.Int64
	sessions        atomic.Int64
	egress          domain.EgressPolicy
	egressOpen      atomic.Int64
}

// OpenLease is a generation-scoped admission reservation.  Data-plane
// goroutines should prefer this over the legacy AuthorizeOpen/ReleaseOpen
// pair when their cleanup can outlive a snapshot swap.
type OpenLease struct {
	engine       *Engine
	assignmentID string
	epoch        uint64
	once         atomic.Bool
}

// Release gives the reservation back exactly once.  Releasing after a newer
// snapshot is active still decrements the aggregate stream count, but cannot
// touch the newer generation's per-assignment count.
func (l *OpenLease) Release() {
	if l == nil || l.engine == nil || l.once.Swap(true) {
		return
	}
	l.engine.releaseLease(l.assignmentID, l.epoch)
}

func New(options Options) (*Engine, error) {
	if options.Role != domain.RoleGateway && options.Role != domain.RoleAgent {
		return nil, errors.New("data-plane role must be gateway or agent")
	}
	if err := domain.ValidateID(options.NodeID, "node_id"); err != nil {
		return nil, err
	}
	if options.MaxStreams <= 0 {
		options.MaxStreams = 256
	}
	if options.MaxStreams > 1<<20 {
		return nil, errors.New("data-plane stream limit exceeds the supported maximum")
	}
	if options.MaxSessions <= 0 {
		options.MaxSessions = 256
	}
	if options.MaxSessions > 1<<20 {
		return nil, errors.New("data-plane session limit exceeds the supported maximum")
	}
	return &Engine{role: options.Role, nodeID: options.NodeID, maxStreams: options.MaxStreams, baseMaxStreams: options.MaxStreams, maxSessions: options.MaxSessions, assignments: map[string]afdp.AssignmentView{}, services: map[string]domain.Service{}, active: map[string]int{}}, nil
}

func (e *Engine) Role() string          { return e.role }
func (e *Engine) NodeID() string        { return e.nodeID }
func (e *Engine) Generation() uint64    { e.mu.RLock(); defer e.mu.RUnlock(); return e.generation }
func (e *Engine) ActiveStreams() int64  { return e.streams.Load() }
func (e *Engine) ActiveSessions() int64 { return e.sessions.Load() }

// MaxBufferBytes is the node-scoped copy-buffer budget from the applied
// snapshot. A zero value means the transport default. Consumers use this
// accessor when creating directional copy buffers so a connection cannot
// allocate more than the declared budget merely by entering the data path.
func (e *Engine) MaxBufferBytes() int {
	if e == nil {
		return 0
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.bufferLimit
}

func (e *Engine) ActiveStreamsForAssignment(assignmentID string) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.active[assignmentID]
}

func (e *Engine) GatewaySpec() (domain.GatewaySpec, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.gatewaySpec == nil {
		return domain.GatewaySpec{}, false
	}
	return cloneGatewaySpec(*e.gatewaySpec), true
}

func (e *Engine) AgentSpec() (domain.AgentSpec, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.agentSpec == nil {
		return domain.AgentSpec{}, false
	}
	return cloneAgentSpec(*e.agentSpec), true
}

// Proxy returns a copy of one locally applied proxy entrance. Keeping this
// lookup on the engine prevents a listener manager from retaining a mutable
// pointer into the snapshot index while a newer generation is installed.
func (e *Engine) Proxy(id string) (domain.ProxySpec, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.agentSpec == nil {
		return domain.ProxySpec{}, false
	}
	for _, proxy := range e.agentSpec.Proxies {
		if proxy.ID == id {
			return proxy, true
		}
	}
	return domain.ProxySpec{}, false
}

func (e *Engine) Service(id string) (domain.Service, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	service, ok := e.services[id]
	service.GatewaySelector.MatchLabels = cloneStringMap(service.GatewaySelector.MatchLabels)
	return service, ok
}

func (e *Engine) Assignment(id string) (afdp.AssignmentView, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	assignment, ok := e.assignments[id]
	assignment = assignment.Clone()
	return assignment, ok
}

// AssignmentForSession returns the assignment selected by the peer's
// SessionHello. Gateways use this lookup before accepting an AFDP session;
// the returned view is a deep copy so a transport cannot mutate the active
// authorization index.
func (e *Engine) AssignmentForSession(hello afdp.SessionHello) (afdp.AssignmentView, bool) {
	e.mu.RLock()
	assignment, ok := e.assignments[hello.AssignmentID]
	e.mu.RUnlock()
	if !ok || assignment.AgentID != hello.AgentID || assignment.Generation != hello.Generation {
		return afdp.AssignmentView{}, false
	}
	return assignment.Clone(), true
}

// ApplySnapshot validates and builds all new indexes before taking the lock;
// a malformed or partially failing snapshot therefore cannot replace the
// last-applied generation.
func (e *Engine) ApplySnapshot(ctx context.Context, snapshot domain.DesiredSnapshot, previous *domain.DesiredSnapshot) error {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if e.closed.Load() {
		return errors.New("data-plane engine is closed")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if snapshot.Checksum == "" {
		return &domain.ApplyError{Code: "missing_checksum", Path: "checksum", Message: "data-plane snapshots must include a checksum"}
	}
	if snapshot.NodeID != e.nodeID {
		return errors.New("snapshot node id does not match engine")
	}
	if e.role == domain.RoleGateway && snapshot.Gateway == nil {
		return errors.New("gateway snapshot is required")
	}
	if e.role == domain.RoleAgent && snapshot.Agent == nil {
		return errors.New("agent snapshot is required")
	}
	currentGeneration := e.Generation()
	rollback := previous != nil && snapshot.Generation < previous.Generation && currentGeneration == previous.Generation
	// A same-generation snapshot is normally an exact replay, but the
	// Controller also uses it to repair a node whose cache has diverged.  The
	// caller must provide the previous snapshot so a random direct call cannot
	// bypass the monotonic-generation gate; checksum equality is deliberately
	// not required here because the new snapshot is authoritative.
	sameGeneration := previous != nil && snapshot.Generation == currentGeneration && previous.Generation == snapshot.Generation
	if !rollback && !sameGeneration && snapshot.Generation <= currentGeneration {
		return errors.New("snapshot generation is stale")
	}
	assignments := make(map[string]afdp.AssignmentView)
	services := make(map[string]domain.Service, len(snapshot.Services))
	var gatewaySpec *domain.GatewaySpec
	var agentSpec *domain.AgentSpec
	if snapshot.Gateway != nil {
		value := cloneGatewaySpec(*snapshot.Gateway)
		gatewaySpec = &value
	}
	if snapshot.Agent != nil {
		value := cloneAgentSpec(*snapshot.Agent)
		agentSpec = &value
	}
	for _, service := range snapshot.Services {
		if err := service.Validate(); err != nil {
			return err
		}
		if service.AgentID != e.nodeID && e.role == domain.RoleAgent {
			continue
		}
		service.GatewaySelector.MatchLabels = cloneStringMap(service.GatewaySelector.MatchLabels)
		services[service.ID] = service
	}
	for _, assignment := range snapshot.Assignments {
		if assignment.Generation > snapshot.Generation {
			return fmt.Errorf("assignment %s generation is newer than snapshot", assignment.ID)
		}
		if e.role == domain.RoleGateway && assignment.GatewayID != e.nodeID {
			continue
		}
		if e.role == domain.RoleAgent && assignment.AgentID != e.nodeID {
			continue
		}
		for _, serviceID := range assignment.ServiceIDs {
			service, ok := services[serviceID]
			if !ok {
				return fmt.Errorf("assignment %s references service %s not present in snapshot", assignment.ID, serviceID)
			}
			if service.AgentID != assignment.AgentID {
				return fmt.Errorf("assignment %s references service %s owned by another agent", assignment.ID, serviceID)
			}
		}
		// Derive the assignment ceiling from the immutable node default, not
		// the currently active generation. The latter may be lower because a
		// previous snapshot supplied a tighter Agent/Gateway limit; carrying it
		// into a replacement generation would make limits sticky after they are
		// relaxed by the Controller.
		assignmentLimit := e.baseMaxStreams
		if snapshot.Agent != nil && snapshot.Agent.Limits.MaxStreams > 0 && snapshot.Agent.Limits.MaxStreams < assignmentLimit {
			assignmentLimit = snapshot.Agent.Limits.MaxStreams
		}
		if snapshot.Gateway != nil && snapshot.Gateway.Transport.MaxStreams > 0 && snapshot.Gateway.Transport.MaxStreams < assignmentLimit {
			assignmentLimit = snapshot.Gateway.Transport.MaxStreams
		}
		assignments[assignment.ID] = afdp.AssignmentFromDomain(assignment, assignmentLimit)
	}
	e.mu.Lock()
	if e.closed.Load() {
		e.mu.Unlock()
		return errors.New("data-plane engine is closed")
	}
	// The initial generation check above only covers the time spent building
	// indexes. A direct caller may race another ApplySnapshot during that
	// work; rollback and same-generation repairs are valid only while the
	// expected previous generation is still active.
	if rollback && (previous == nil || e.generation != previous.Generation) {
		e.mu.Unlock()
		return errors.New("snapshot generation changed during rollback")
	}
	if sameGeneration && e.generation != snapshot.Generation {
		e.mu.Unlock()
		return errors.New("snapshot generation changed during resync")
	}
	if !rollback && !sameGeneration && snapshot.Generation <= e.generation {
		e.mu.Unlock()
		return errors.New("snapshot generation is stale")
	}
	e.assignments, e.services, e.generation = assignments, services, snapshot.Generation
	e.epoch++
	// Active counts are generation-scoped. Existing streams retain the
	// aggregate reservation until their lease closes, but a replacement
	// assignment must not inherit the old generation's per-assignment count;
	// stale leases are intentionally prevented from decrementing this map.
	e.active = make(map[string]int, len(assignments))
	e.gatewaySpec, e.agentSpec = gatewaySpec, agentSpec
	e.maxStreams = e.baseMaxStreams
	if agentSpec != nil {
		e.egress = cloneEgressPolicy(agentSpec.Egress)
		e.connectionLimit = agentSpec.Limits.MaxConnections
		e.bufferLimit = agentSpec.Limits.MaxBufferBytes
		if agentSpec.Limits.MaxStreams > 0 && agentSpec.Limits.MaxStreams < e.maxStreams {
			e.maxStreams = agentSpec.Limits.MaxStreams
		}
	} else if gatewaySpec != nil {
		e.egress = cloneEgressPolicy(gatewaySpec.Egress)
		e.connectionLimit = gatewaySpec.Capacity.MaxConnections
		e.bufferLimit = 0
		if gatewaySpec.Transport.MaxStreams > 0 && gatewaySpec.Transport.MaxStreams < e.maxStreams {
			e.maxStreams = gatewaySpec.Transport.MaxStreams
		}
	} else {
		e.egress = domain.EgressPolicy{}
		e.connectionLimit = 0
		e.bufferLimit = 0
	}
	e.mu.Unlock()
	return nil
}

// ResetSnapshot removes a generation that was installed speculatively but
// could not be activated by a sibling data-plane component.  The node
// reconciler uses this only when applying the very first snapshot: there is
// no previous desired document to pass through ApplySnapshot's normal
// rollback path, so leaving the engine index installed would allow a failed
// listener build to admit streams for a generation that is not actually
// serving traffic.
//
// expectedGeneration makes the operation compare-and-swap-like.  A caller
// racing a later successful apply cannot accidentally clear that newer
// generation.  Existing counters are intentionally retained; any in-flight
// reservation belongs to the failed generation and will release through its
// normal close path, while the empty index rejects new admissions.
func (e *Engine) ResetSnapshot(expectedGeneration uint64) error {
	if e == nil {
		return errors.New("data-plane engine is nil")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if expectedGeneration != 0 && e.generation != expectedGeneration {
		return errors.New("data-plane snapshot generation changed during reset")
	}
	e.generation = 0
	e.gatewaySpec = nil
	e.agentSpec = nil
	e.assignments = make(map[string]afdp.AssignmentView)
	e.services = make(map[string]domain.Service)
	e.active = make(map[string]int)
	e.connectionLimit = 0
	e.bufferLimit = 0
	e.maxStreams = e.baseMaxStreams
	e.egress = domain.EgressPolicy{}
	e.epoch++
	// No valid snapshot is active after a failed first apply. Keep admission
	// drained until the next authenticated control stream/snapshot explicitly
	// reopens it.
	e.draining.Store(true)
	return nil
}

func (e *Engine) AuthorizeSession(hello afdp.SessionHello) error {
	if e.closed.Load() || e.draining.Load() {
		return errors.New("data-plane engine is not accepting sessions")
	}
	e.mu.RLock()
	if e.closed.Load() || e.draining.Load() {
		e.mu.RUnlock()
		return errors.New("data-plane engine is not accepting sessions")
	}
	assignment, ok := e.assignments[hello.AssignmentID]
	e.mu.RUnlock()
	if !ok {
		return afdp.ErrUnauthorizedAgent
	}
	if err := afdp.AuthorizeSession(hello, assignment); err != nil {
		return err
	}
	for {
		current := e.sessions.Load()
		if current >= int64(e.maxSessions) {
			return errors.New("session limit reached")
		}
		if e.sessions.CompareAndSwap(current, current+1) {
			return nil
		}
	}
}

// ReleaseSession must be called when the authenticated QUIC session closes.
// Calling it more than once is harmless and never drives the count negative.
func (e *Engine) ReleaseSession() {
	for {
		current := e.sessions.Load()
		if current <= 0 || e.sessions.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (e *Engine) AuthorizeOpen(assignmentID string, open afdp.OpenMetadata) error {
	_, err := e.ReserveOpen(assignmentID, open)
	return err
}

// ReserveOpen authorizes and reserves one reverse/proxy stream.  The lease
// must be released when the stream or UDP flow terminates.
func (e *Engine) ReserveOpen(assignmentID string, open afdp.OpenMetadata) (*OpenLease, error) {
	if e.closed.Load() || e.draining.Load() {
		return nil, errors.New("data-plane engine is not accepting opens")
	}
	// Keep the per-assignment reservation and the global stream reservation in
	// one short critical section. Checking the assignment count under a read
	// lock and incrementing it later would let concurrent opens oversubscribe a
	// small assignment even though each individual check passed.
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() || e.draining.Load() {
		return nil, errors.New("data-plane engine is not accepting opens")
	}
	assignment, assignmentOK := e.assignments[assignmentID]
	if !assignmentOK {
		return nil, afdp.ErrUnauthorizedAgent
	}
	if err := afdp.AuthorizeOpen(open, assignment); err != nil {
		return nil, err
	}
	if !open.Egress {
		service, serviceOK := e.services[open.ServiceID]
		if !serviceOK || !service.Enabled {
			return nil, afdp.ErrUnauthorizedAgent
		}
		if service.Protocol != open.Protocol || service.LocalTarget != open.Target {
			return nil, afdp.ErrUnauthorizedAgent
		}
	}
	maxStreams := e.maxStreams
	if assignment.MaxStreams > 0 && assignment.MaxStreams < maxStreams {
		maxStreams = assignment.MaxStreams
	}
	if e.active[assignmentID] >= maxStreams {
		return nil, errors.New("assignment stream limit reached")
	}
	for {
		current := e.streams.Load()
		if current >= int64(e.maxStreams) {
			return nil, errors.New("stream limit reached")
		}
		// The connection limit is a second global ceiling and must be checked
		// inside the same CAS loop as the stream limit. Checking it only before
		// the loop lets concurrent opens race past a one-connection policy when
		// the first goroutine wins the CAS and the second retries.
		if e.connectionLimit > 0 && current >= int64(e.connectionLimit) {
			return nil, errors.New("connection limit reached")
		}
		if e.streams.CompareAndSwap(current, current+1) {
			break
		}
	}
	e.active[assignmentID]++
	return &OpenLease{engine: e, assignmentID: assignmentID, epoch: e.epoch}, nil
}

// ReserveLocalOpen reserves a node-scoped stream for a local proxy.  It is
// the lease form of AuthorizeLocalOpen and has the same generation-safe
// cleanup semantics.
func (e *Engine) ReserveLocalOpen() (*OpenLease, error) {
	if e == nil || e.closed.Load() || e.draining.Load() {
		return nil, errors.New("data-plane engine is not accepting local opens")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() || e.draining.Load() {
		return nil, errors.New("data-plane engine is not accepting local opens")
	}
	for {
		current := e.streams.Load()
		if current >= int64(e.maxStreams) {
			return nil, errors.New("stream limit reached")
		}
		if e.connectionLimit > 0 && current >= int64(e.connectionLimit) {
			return nil, errors.New("connection limit reached")
		}
		if e.streams.CompareAndSwap(current, current+1) {
			return &OpenLease{engine: e, epoch: e.epoch}, nil
		}
	}
}

// AuthorizeLocalOpen reserves one node-scoped stream for a local Agent proxy
// connection.  Local proxy sockets do not belong to a reverse assignment, but
// they still consume the same bounded connection/stream budget advertised by
// the Agent snapshot.  The returned reservation must be released exactly once
// when the client connection finishes.
func (e *Engine) AuthorizeLocalOpen() error {
	_, err := e.ReserveLocalOpen()
	return err
}

// ReleaseLocalOpen releases a reservation returned by AuthorizeLocalOpen.
// It is saturating so a double cleanup cannot make the global counter
// negative.
func (e *Engine) ReleaseLocalOpen() { e.releaseStreamReservation() }

func (e *Engine) ReleaseOpen(assignmentID string) {
	e.releaseStreamReservation()
	e.mu.Lock()
	if count := e.active[assignmentID]; count <= 1 {
		delete(e.active, assignmentID)
	} else {
		e.active[assignmentID] = count - 1
	}
	e.mu.Unlock()
}

func (e *Engine) releaseLease(assignmentID string, epoch uint64) {
	e.releaseStreamReservation()
	if assignmentID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if epoch != e.epoch {
		return
	}
	if count := e.active[assignmentID]; count <= 1 {
		delete(e.active, assignmentID)
	} else {
		e.active[assignmentID] = count - 1
	}
}

func (e *Engine) releaseStreamReservation() {
	for {
		current := e.streams.Load()
		if current <= 0 || e.streams.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (e *Engine) BeginDrain() { e.draining.Store(true) }
func (e *Engine) EndDrain() {
	if !e.closed.Load() {
		e.draining.Store(false)
	}
}
func (e *Engine) IsDraining() bool { return e.draining.Load() }
func (e *Engine) Close() error     { e.closed.Store(true); e.draining.Store(true); return nil }

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneGatewaySpec(value domain.GatewaySpec) domain.GatewaySpec {
	value.PublicEndpoints = append([]string(nil), value.PublicEndpoints...)
	value.Listeners = append([]domain.Listener(nil), value.Listeners...)
	value.Labels = cloneStringMap(value.Labels)
	value.PortPool.TCP = append([]domain.PortRange(nil), value.PortPool.TCP...)
	value.PortPool.UDP = append([]domain.PortRange(nil), value.PortPool.UDP...)
	value.Obfuscation.KeyCiphertext = append([]byte(nil), value.Obfuscation.KeyCiphertext...)
	value.Egress = cloneEgressPolicy(value.Egress)
	return value
}

func cloneAgentSpec(value domain.AgentSpec) domain.AgentSpec {
	value.GatewaySelector.MatchLabels = cloneStringMap(value.GatewaySelector.MatchLabels)
	value.Proxies = append([]domain.ProxySpec(nil), value.Proxies...)
	value.Routes = append([]domain.RouteRule(nil), value.Routes...)
	value.Egress.TCPPorts = append([]string(nil), value.Egress.TCPPorts...)
	value.Egress.UDPPorts = append([]string(nil), value.Egress.UDPPorts...)
	value.Egress.AllowCIDRs = append([]string(nil), value.Egress.AllowCIDRs...)
	value.Egress.AllowSpecialCIDRs = append([]string(nil), value.Egress.AllowSpecialCIDRs...)
	for i := range value.Routes {
		value.Routes[i].CIDRs = append([]string(nil), value.Routes[i].CIDRs...)
		value.Routes[i].Domains = append([]string(nil), value.Routes[i].Domains...)
	}
	return value
}

func cloneEgressPolicy(value domain.EgressPolicy) domain.EgressPolicy {
	value.TCPPorts = append([]string(nil), value.TCPPorts...)
	value.UDPPorts = append([]string(nil), value.UDPPorts...)
	value.AllowCIDRs = append([]string(nil), value.AllowCIDRs...)
	value.AllowSpecialCIDRs = append([]string(nil), value.AllowSpecialCIDRs...)
	return value
}

func (e *Engine) activeSessionCount() int {
	return int(e.sessions.Load())
}
