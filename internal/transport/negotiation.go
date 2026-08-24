package transport

import "asterferry/internal/config"

func LimitsFromConfig(limits config.Limits, streamLimit int64) Limits {
	if streamLimit <= 0 {
		streamLimit = limits.MaxStreamsPerAgent
	}
	return Limits{
		MaxFrameBytes:      limits.MaxFrameBytes,
		MaxRecordBytes:     limits.MaxRecordBytes,
		MaxWriteBatchBytes: limits.MaxWriteBatchBytes,
		MaxUDPBytes:        limits.MaxUDPBytes,
		MaxStreams:         streamLimit,
	}
}

func AgentCapabilities(cfg *config.AgentOptions) []Capability {
	result := []Capability{CapabilityErrorsV1, CapabilityLimitsV1}
	if cfg == nil {
		return result
	}
	for _, tunnel := range cfg.Agent.Reverse {
		if tunnel.Protocol == "tcp" && !SupportsCapability(result, CapabilityReverseTCP) {
			result = append(result, CapabilityReverseTCP)
		}
		if tunnel.Protocol == "udp" && !SupportsCapability(result, CapabilityReverseUDP) {
			result = append(result, CapabilityReverseUDP)
		}
	}
	if cfg.Obfuscation.ProxyProfile == config.ProfileBalanced || cfg.Obfuscation.ReverseProfile == config.ProfileBalanced {
		result = append(result, CapabilityRelayBalanced)
	}
	// The Agent may route a proxy connection through the Gateway even when its
	// current rules happen to select direct routing.
	result = append(result, CapabilityEgressProxy)
	return normalizedCapabilities(result)
}

func GatewayCapabilities(cfg *config.GatewayOptions) []Capability {
	result := []Capability{CapabilityErrorsV1, CapabilityLimitsV1, CapabilityRelayBalanced}
	if cfg == nil {
		return result
	}
	for _, agent := range cfg.Agents {
		if len(agent.Reverse.TCPPorts) > 0 && !SupportsCapability(result, CapabilityReverseTCP) {
			result = append(result, CapabilityReverseTCP)
		}
		if len(agent.Reverse.UDPPorts) > 0 && !SupportsCapability(result, CapabilityReverseUDP) {
			result = append(result, CapabilityReverseUDP)
		}
		if agent.Egress.Enabled && !SupportsCapability(result, CapabilityEgressProxy) {
			result = append(result, CapabilityEgressProxy)
		}
	}
	return normalizedCapabilities(result)
}
