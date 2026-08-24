package gateway

import (
	"fmt"
	"sync"
	"sync/atomic"

	"asterferry/internal/transport"
)

// mappingDirectory keeps port ownership and mapping lifecycle behind a local
// interface so a future node-owner router does not have to change Gateway's
// session handling. The current implementation always binds locally.
type mappingDirectory interface {
	Count() int
	Register(*Session, []transport.TunnelRegistration) transport.RegisterResult
	BeginDrain()
	RemoveSession(*Session)
	CloseAll()
}

type mappingManager struct {
	server     *Gateway
	mu         sync.RWMutex
	registerMu sync.Mutex
	items      map[string]*Mapping
	draining   atomic.Bool
}

func newMappingManager(server *Gateway) *mappingManager {
	return &mappingManager{server: server, items: make(map[string]*Mapping)}
}

func (m *mappingManager) Count() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.items)
}

func (m *mappingManager) Register(sess *Session, specs []transport.TunnelRegistration) transport.RegisterResult {
	if m == nil || m.server == nil {
		return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorInternal, "mapping manager unavailable", true)}
	}
	if m.draining.Load() || (m.server.life != nil && !m.server.life.IsRunning()) {
		return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorResourceExhausted, "gateway is draining", true)}
	}
	s := m.server
	m.registerMu.Lock()
	defer m.registerMu.Unlock()
	if m.draining.Load() || (s.life != nil && !s.life.IsRunning()) {
		return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorResourceExhausted, "gateway is draining", true)}
	}
	cred := s.acl[sess.agentID]
	if cred == nil {
		return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorAuthFailed, "agent credentials unavailable", false)}
	}
	if len(specs) > int(s.cfg.StreamLimit) {
		return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorResourceExhausted, "mapping count exceeds agent limit", true)}
	}
	seen := map[string]bool{}
	seenPorts := map[string]bool{}
	for _, spec := range specs {
		if spec.Name == "" || seen[spec.Name] {
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorMappingRejected, "duplicate or empty mapping name", false)}
		}
		seen[spec.Name] = true
		if spec.Protocol != "tcp" && spec.Protocol != "udp" {
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorMappingRejected, "unsupported mapping protocol", false)}
		}
		if (spec.Protocol == "tcp" && !sess.hasCapability(transport.CapabilityReverseTCP)) || (spec.Protocol == "udp" && !sess.hasCapability(transport.CapabilityReverseUDP)) {
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorCapabilityMismatch, "reverse capability was not negotiated", false)}
		}
		if spec.Profile == "balanced" && !sess.hasCapability(transport.CapabilityRelayBalanced) {
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorCapabilityMismatch, "balanced relay capability was not negotiated", false)}
		}
		if _, err := sess.relayProfile(spec.Profile); err != nil {
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorMappingRejected, "unsupported mapping profile", false)}
		}
		if !allowedPort(spec.Protocol, spec.GatewayPort, cred) {
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorPolicyDenied, fmt.Sprintf("port %d is not allowed", spec.GatewayPort), false)}
		}
		key := mappingKey(spec.Protocol, spec.GatewayPort)
		if seenPorts[key] {
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorMappingRejected, fmt.Sprintf("port %d is registered more than once", spec.GatewayPort), false)}
		}
		seenPorts[key] = true
		m.mu.RLock()
		existing := m.items[key]
		m.mu.RUnlock()
		if existing != nil && existing.session != sess {
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorResourceExhausted, fmt.Sprintf("port %d is already in use", spec.GatewayPort), true)}
		}
	}

	m.mu.Lock()
	oldMappings := make([]*Mapping, 0)
	for key, item := range m.items {
		if item.session == sess {
			delete(m.items, key)
			oldMappings = append(oldMappings, item)
		}
	}
	m.mu.Unlock()
	for _, item := range oldMappings {
		_ = item.Close()
	}

	created := make([]*Mapping, 0, len(specs))
	for _, spec := range specs {
		item, err := newMapping(s, sess, spec)
		if err != nil {
			for _, old := range created {
				_ = old.Close()
			}
			s.metrics.MappingFailures.Add(1)
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorMappingRejected, fmt.Sprintf("bind %s/%d failed", spec.Protocol, spec.GatewayPort), true)}
		}
		created = append(created, item)
	}
	m.mu.Lock()
	for _, item := range created {
		m.items[item.key] = item
	}
	m.mu.Unlock()
	for _, item := range created {
		go item.Run()
	}
	return transport.RegisterResult{Mappings: specs}
}

// BeginDrain stops new reverse listener traffic while preserving mappings and
// their already established connections until the role closes or times out.
func (m *mappingManager) BeginDrain() {
	if m == nil {
		return
	}
	m.registerMu.Lock()
	if m.draining.Swap(true) {
		m.registerMu.Unlock()
		return
	}
	m.registerMu.Unlock()
	m.mu.RLock()
	items := make([]*Mapping, 0, len(m.items))
	for _, item := range m.items {
		items = append(items, item)
	}
	m.mu.RUnlock()
	for _, item := range items {
		item.BeginDrain()
	}
}

func (m *mappingManager) RemoveSession(sess *Session) {
	if m == nil || sess == nil {
		return
	}
	m.mu.Lock()
	stale := make([]*Mapping, 0)
	for key, item := range m.items {
		if item.session == sess {
			delete(m.items, key)
			stale = append(stale, item)
		}
	}
	m.mu.Unlock()
	for _, item := range stale {
		go item.Close()
	}
}

func (m *mappingManager) CloseAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	items := make([]*Mapping, 0, len(m.items))
	for key, item := range m.items {
		delete(m.items, key)
		items = append(items, item)
	}
	m.mu.Unlock()
	for _, item := range items {
		_ = item.Close()
	}
}
