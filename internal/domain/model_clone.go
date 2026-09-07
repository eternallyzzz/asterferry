package domain

import (
	"fmt"
	"sort"
)

func (s DesiredSnapshot) WithChecksum() (DesiredSnapshot, error) {
	s = s.Normalize()
	// WithChecksum is also the explicit repair operation callers use after
	// changing a snapshot in memory. Ignore an old checksum while validating
	// the structure, then compute and replace it below; plain Validate still
	// rejects a non-empty checksum that does not match its content.
	s.Checksum = ""
	if err := s.Validate(); err != nil {
		return DesiredSnapshot{}, err
	}
	checksum, err := s.ComputeChecksum()
	if err != nil {
		return DesiredSnapshot{}, err
	}
	s.Checksum = checksum
	return s, nil
}

func (s DesiredSnapshot) Clone() DesiredSnapshot {
	clone := s
	if s.Gateway != nil {
		value := cloneGatewaySpec(*s.Gateway)
		clone.Gateway = &value
	}
	if s.Agent != nil {
		value := cloneAgentSpec(*s.Agent)
		clone.Agent = &value
	}
	clone.Services = make([]Service, len(s.Services))
	for i, service := range s.Services {
		clone.Services[i] = service
		clone.Services[i].GatewaySelector.MatchLabels = cloneLabels(service.GatewaySelector.MatchLabels)
	}
	clone.Assignments = make([]Assignment, len(s.Assignments))
	for i, assignment := range s.Assignments {
		clone.Assignments[i] = assignment
		clone.Assignments[i].ServiceIDs = append([]string(nil), assignment.ServiceIDs...)
		clone.Assignments[i].Bindings = append([]Binding(nil), assignment.Bindings...)
		clone.Assignments[i].Obfuscation.KeyCiphertext = append([]byte(nil), assignment.Obfuscation.KeyCiphertext...)
		clone.Assignments[i].Obfuscation.PreviousKeyCiphertext = append([]byte(nil), assignment.Obfuscation.PreviousKeyCiphertext...)
		clone.Assignments[i].Obfuscation.Key = append([]byte(nil), assignment.Obfuscation.Key...)
		clone.Assignments[i].Obfuscation.PreviousKey = append([]byte(nil), assignment.Obfuscation.PreviousKey...)
	}
	return clone
}

func cloneGatewaySpec(value GatewaySpec) GatewaySpec {
	value.PublicEndpoints = append([]string(nil), value.PublicEndpoints...)
	value.Listeners = append([]Listener(nil), value.Listeners...)
	value.Labels = cloneLabels(value.Labels)
	value.PortPool.TCP = append([]PortRange(nil), value.PortPool.TCP...)
	value.PortPool.UDP = append([]PortRange(nil), value.PortPool.UDP...)
	value.Obfuscation.KeyCiphertext = append([]byte(nil), value.Obfuscation.KeyCiphertext...)
	value.Obfuscation.PreviousKeyCiphertext = append([]byte(nil), value.Obfuscation.PreviousKeyCiphertext...)
	value.Obfuscation.Key = append([]byte(nil), value.Obfuscation.Key...)
	value.Obfuscation.PreviousKey = append([]byte(nil), value.Obfuscation.PreviousKey...)
	value.Egress = cloneEgress(value.Egress)
	return value
}

func cloneAgentSpec(value AgentSpec) AgentSpec {
	value.GatewaySelector.MatchLabels = cloneLabels(value.GatewaySelector.MatchLabels)
	value.Proxies = append([]ProxySpec(nil), value.Proxies...)
	value.Routes = append([]RouteRule(nil), value.Routes...)
	for i := range value.Routes {
		value.Routes[i].CIDRs = append([]string(nil), value.Routes[i].CIDRs...)
		value.Routes[i].Domains = append([]string(nil), value.Routes[i].Domains...)
		value.Routes[i].GeoIP = append([]string(nil), value.Routes[i].GeoIP...)
	}
	value.Egress = cloneEgress(value.Egress)
	return value
}

func (s DesiredSnapshot) Normalize() DesiredSnapshot {
	clone := s.Clone()
	if clone.Gateway != nil {
		sort.Slice(clone.Gateway.Listeners, func(i, j int) bool {
			left, right := clone.Gateway.Listeners[i], clone.Gateway.Listeners[j]
			return fmt.Sprintf("%s|%s|%d", left.Protocol, left.Bind, left.Port) < fmt.Sprintf("%s|%s|%d", right.Protocol, right.Bind, right.Port)
		})
		sort.Strings(clone.Gateway.Egress.TCPPorts)
		sort.Strings(clone.Gateway.Egress.UDPPorts)
		sort.Strings(clone.Gateway.Egress.AllowCIDRs)
		sort.Strings(clone.Gateway.Egress.AllowSpecialCIDRs)
	}
	sort.Slice(clone.Services, func(i, j int) bool { return clone.Services[i].ID < clone.Services[j].ID })
	if clone.Agent != nil {
		sort.Slice(clone.Agent.Proxies, func(i, j int) bool { return clone.Agent.Proxies[i].ID < clone.Agent.Proxies[j].ID })
		sort.Strings(clone.Agent.Egress.TCPPorts)
		sort.Strings(clone.Agent.Egress.UDPPorts)
		sort.Strings(clone.Agent.Egress.AllowCIDRs)
		sort.Strings(clone.Agent.Egress.AllowSpecialCIDRs)
	}
	for i := range clone.Assignments {
		sort.Strings(clone.Assignments[i].ServiceIDs)
		sort.Slice(clone.Assignments[i].Bindings, func(left, right int) bool {
			a, b := clone.Assignments[i].Bindings[left], clone.Assignments[i].Bindings[right]
			return fmt.Sprintf("%s|%s|%s|%d", a.ServiceID, a.Protocol, a.Bind, a.Port) < fmt.Sprintf("%s|%s|%s|%d", b.ServiceID, b.Protocol, b.Bind, b.Port)
		})
	}
	sort.Slice(clone.Assignments, func(i, j int) bool { return clone.Assignments[i].ID < clone.Assignments[j].ID })
	return clone
}
