package gateway

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"asterferry/internal/config"
	"asterferry/internal/observability"
	"asterferry/internal/transport"
)

// mappingDirectory keeps port ownership and mapping lifecycle behind a local
// interface so a future node-owner router does not have to change Gateway's
// session handling. The current implementation binds every mapping locally.
type mappingDirectory interface {
	Count() int
	Snapshot() []observability.GatewayMappingSnapshot
	Register(*Session, []transport.TunnelRegistration) transport.RegisterResult
	BeginDrain()
	RemoveSession(*Session) error
	CloseAll() error
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

func normalizeMappingSpecs(specs []transport.TunnelRegistration) []transport.TunnelRegistration {
	result := append([]transport.TunnelRegistration(nil), specs...)
	for i := range result {
		if result[i].GatewayBind == "" {
			result[i].GatewayBind = config.DefaultReverseGatewayBind
		}
	}
	return result
}

func (m *mappingManager) Count() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.items)
}

func (m *mappingManager) Snapshot() []observability.GatewayMappingSnapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	result := make([]observability.GatewayMappingSnapshot, 0, len(m.items))
	for _, item := range m.items {
		if item == nil || item.session == nil {
			continue
		}
		state := "active"
		if item.draining.Load() {
			state = "draining"
		} else if item.ctx.Err() != nil {
			state = "closed"
		}
		spec := item.specSnapshot()
		result = append(result, observability.GatewayMappingSnapshot{
			Name:        spec.Name,
			AgentID:     item.session.agentID,
			Protocol:    spec.Protocol,
			GatewayPort: spec.GatewayPort,
			GatewayBind: spec.GatewayBind,
			Profile:     spec.Profile,
			State:       state,
		})
	}
	m.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool {
		if result[i].AgentID != result[j].AgentID {
			return result[i].AgentID < result[j].AgentID
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func (m *mappingManager) Register(sess *Session, specs []transport.TunnelRegistration) transport.RegisterResult {
	if m == nil || m.server == nil {
		return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorInternal, "mapping manager unavailable", true)}
	}
	if sess == nil {
		return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorAuthFailed, "session is unavailable", false)}
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
	specs = normalizeMappingSpecs(specs)
	if err := transport.ValidateRegister(transport.Register{Mappings: specs}); err != nil {
		return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorMappingRejected, "invalid mapping registration", false)}
	}
	existing := make(map[string]*Mapping, len(specs))
	m.mu.RLock()
	for _, spec := range specs {
		key := mappingKey(spec.Protocol, spec.GatewayBind, spec.GatewayPort)
		existing[key] = m.items[key]
	}
	m.mu.RUnlock()
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
		key := mappingKey(spec.Protocol, spec.GatewayBind, spec.GatewayPort)
		if seenPorts[key] {
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorMappingRejected, fmt.Sprintf("%s/%d is registered more than once", spec.GatewayBind, spec.GatewayPort), false)}
		}
		seenPorts[key] = true
		item := existing[key]
		if item != nil && item.session != sess {
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorResourceExhausted, fmt.Sprintf("%s/%d is already in use", spec.GatewayBind, spec.GatewayPort), true)}
		}
	}

	desired := make(map[string]transport.TunnelRegistration, len(specs))
	created := make([]*Mapping, 0, len(specs))
	for _, spec := range specs {
		key := mappingKey(spec.Protocol, spec.GatewayBind, spec.GatewayPort)
		desired[key] = spec
		old := existing[key]
		if old != nil && old.ctx.Err() == nil && !old.draining.Load() {
			continue
		}
		if old != nil {
			_ = old.Close()
		}
		item, err := newMapping(s, sess, spec)
		if err != nil {
			for _, staged := range created {
				_ = staged.Close()
			}
			s.metrics.MappingFailures.Add(1)
			return transport.RegisterResult{Error: transport.NewProtocolError(transport.ErrorMappingRejected, fmt.Sprintf("bind %s/%s/%d failed", spec.Protocol, spec.GatewayBind, spec.GatewayPort), true)}
		}
		created = append(created, item)
	}

	createdByKey := make(map[string]*Mapping, len(created))
	for _, item := range created {
		createdByKey[item.key] = item
	}
	obsolete := make([]*Mapping, 0)
	m.mu.Lock()
	for key, item := range m.items {
		if item.session != sess {
			continue
		}
		if _, keep := desired[key]; !keep {
			delete(m.items, key)
			obsolete = append(obsolete, item)
			continue
		}
		if replacement := createdByKey[key]; replacement != nil {
			delete(m.items, key)
			obsolete = append(obsolete, item)
			m.items[key] = replacement
			continue
		}
		item.updateSpec(desired[key])
	}
	for key, item := range createdByKey {
		if m.items[key] == nil {
			m.items[key] = item
		}
	}
	m.mu.Unlock()
	for _, item := range obsolete {
		_ = item.Close()
	}
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

func (m *mappingManager) RemoveSession(sess *Session) error {
	if m == nil || sess == nil {
		return nil
	}
	m.registerMu.Lock()
	defer m.registerMu.Unlock()
	m.mu.Lock()
	stale := make([]*Mapping, 0)
	for key, item := range m.items {
		if item.session == sess {
			delete(m.items, key)
			stale = append(stale, item)
		}
	}
	m.mu.Unlock()
	var errs []error
	for _, item := range stale {
		if err := item.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *mappingManager) CloseAll() error {
	if m == nil {
		return nil
	}
	m.registerMu.Lock()
	defer m.registerMu.Unlock()
	m.mu.Lock()
	items := make([]*Mapping, 0, len(m.items))
	for key, item := range m.items {
		delete(m.items, key)
		items = append(items, item)
	}
	m.mu.Unlock()
	var errs []error
	for _, item := range items {
		if err := item.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
