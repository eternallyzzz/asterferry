package gateway

import (
	"fmt"
	"sync"

	"asterferry/internal/relay"
	"asterferry/internal/transport"
)

type mappingManager struct {
	server     *Gateway
	mu         sync.RWMutex
	registerMu sync.Mutex
	items      map[string]*Mapping
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
		return transport.RegisterResult{Error: "mapping manager unavailable"}
	}
	s := m.server
	m.registerMu.Lock()
	defer m.registerMu.Unlock()
	cred := s.acl[sess.agentID]
	if cred == nil {
		return transport.RegisterResult{Error: "agent credentials disappeared"}
	}
	if len(specs) > int(s.cfg.StreamLimit) {
		return transport.RegisterResult{Error: "mapping count exceeds agent limit"}
	}
	seen := map[string]bool{}
	seenPorts := map[string]bool{}
	for _, spec := range specs {
		if spec.Name == "" || seen[spec.Name] {
			return transport.RegisterResult{Error: "duplicate or empty mapping name"}
		}
		seen[spec.Name] = true
		if spec.Protocol != "tcp" && spec.Protocol != "udp" {
			return transport.RegisterResult{Error: "unsupported mapping protocol"}
		}
		if _, err := relay.NewProfile(s.profile(spec.Profile), s.cfg.Limits.MaxRecordBytes, s.cfg.Obfuscation.MaxPaddingBytes); err != nil {
			return transport.RegisterResult{Error: "unsupported mapping profile"}
		}
		if !allowedPort(spec.Protocol, spec.GatewayPort, cred) {
			return transport.RegisterResult{Error: fmt.Sprintf("port %d is not allowed", spec.GatewayPort)}
		}
		key := mappingKey(spec.Protocol, spec.GatewayPort)
		if seenPorts[key] {
			return transport.RegisterResult{Error: fmt.Sprintf("port %d is registered more than once", spec.GatewayPort)}
		}
		seenPorts[key] = true
		m.mu.RLock()
		existing := m.items[key]
		m.mu.RUnlock()
		if existing != nil && existing.session != sess {
			return transport.RegisterResult{Error: fmt.Sprintf("port %d is already in use", spec.GatewayPort)}
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
			return transport.RegisterResult{Error: fmt.Sprintf("bind %s/%d: %v", spec.Protocol, spec.GatewayPort, err)}
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
