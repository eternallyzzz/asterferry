package dataplane

import (
	"asterferry/internal/domain"
)

func (e *Engine) BeginDrain() { e.draining.Store(true) }

func (e *Engine) EndDrain() {
	if !e.closed.Load() {
		e.draining.Store(false)
	}
}

func (e *Engine) IsDraining() bool { return e.draining.Load() }

func (e *Engine) Close() error { e.closed.Store(true); e.draining.Store(true); return nil }

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
	value.Obfuscation.PreviousKeyCiphertext = append([]byte(nil), value.Obfuscation.PreviousKeyCiphertext...)
	value.Obfuscation.Key = append([]byte(nil), value.Obfuscation.Key...)
	value.Obfuscation.PreviousKey = append([]byte(nil), value.Obfuscation.PreviousKey...)
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
		value.Routes[i].GeoIP = append([]string(nil), value.Routes[i].GeoIP...)
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
