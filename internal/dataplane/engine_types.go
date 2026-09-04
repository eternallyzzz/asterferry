package dataplane

import (
	"asterferry/internal/afdp"
	"asterferry/internal/domain"
	"errors"
	"sync"
	"sync/atomic"
)

type Options struct {
	Kind        domain.NodeSpecKind
	NodeID      string
	MaxStreams  int
	MaxSessions int
}

type Engine struct {
	kind        domain.NodeSpecKind
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
// goroutines should prefer this over an unscoped authorize/release pair when
// their cleanup can outlive a snapshot swap.
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
	if options.Kind != domain.NodeSpecGateway && options.Kind != domain.NodeSpecAgent {
		return nil, errors.New("data-plane kind must be gateway or agent")
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
	return &Engine{kind: options.Kind, nodeID: options.NodeID, maxStreams: options.MaxStreams, baseMaxStreams: options.MaxStreams, maxSessions: options.MaxSessions, assignments: map[string]afdp.AssignmentView{}, services: map[string]domain.Service{}, active: map[string]int{}}, nil
}

func (e *Engine) Kind() domain.NodeSpecKind { return e.kind }

func (e *Engine) NodeID() string { return e.nodeID }

func (e *Engine) Generation() uint64 { e.mu.RLock(); defer e.mu.RUnlock(); return e.generation }

func (e *Engine) ActiveStreams() int64 { return e.streams.Load() }

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
