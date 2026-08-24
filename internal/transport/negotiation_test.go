package transport

import (
	"testing"

	"asterferry/internal/config"
)

func TestNegotiationRejectsMalformedOffers(t *testing.T) {
	baseCaps := []Capability{CapabilityErrorsV1, CapabilityLimitsV1}
	baseLimits := Limits{MaxFrameBytes: 4096, MaxRecordBytes: 2048, MaxUDPBytes: 1024, MaxStreams: 4}
	for _, caps := range [][]Capability{
		{CapabilityUnspecified},
		{CapabilityErrorsV1, CapabilityErrorsV1},
		{CapabilityLimitsV1, CapabilityErrorsV1},
		{CapabilityErrorsV1, CapabilityLimitsV1, Capability(999)},
	} {
		if err := ValidateCapabilities(caps); err == nil {
			t.Fatalf("capabilities %#v should be rejected", caps)
		}
	}
	if _, err := NegotiateCapabilities([]Capability{CapabilityErrorsV1}, baseCaps); err == nil {
		t.Fatal("missing mandatory limits capability should fail")
	}
	if _, err := NegotiateCapabilities(baseCaps, []Capability{CapabilityLimitsV1}); err == nil {
		t.Fatal("unsupported mandatory capability should fail")
	}
	if _, err := NegotiateLimits(Limits{MaxFrameBytes: 512}, baseLimits); err == nil {
		t.Fatal("too-small offered limits should fail")
	}
	if _, err := NegotiateLimits(baseLimits, Limits{MaxStreams: 0}); err == nil {
		t.Fatal("too-small supported limits should fail")
	}
	if err := ValidateNegotiation(baseCaps, []Capability{CapabilityErrorsV1, Capability(10)}, baseLimits, baseLimits); err == nil {
		t.Fatal("unoffered selected capability should fail")
	}
	if err := ValidateNegotiation(baseCaps, []Capability{CapabilityErrorsV1}, baseLimits, baseLimits); err == nil {
		t.Fatal("selected capabilities missing limits should fail")
	}
	if err := ValidateNegotiation(baseCaps, baseCaps, baseLimits, Limits{MaxFrameBytes: 8192, MaxRecordBytes: 2048, MaxUDPBytes: 1024, MaxStreams: 4}); err == nil {
		t.Fatal("selected limits above offer should fail")
	}
}

func TestCapabilitiesFromRuntimeOptions(t *testing.T) {
	agentCaps := AgentCapabilities(&config.AgentOptions{Agent: config.AgentRuntime{Reverse: []config.Tunnel{{Protocol: "tcp"}, {Protocol: "udp"}}}, Obfuscation: config.ObfuscationConfig{ProxyProfile: config.ProfileBalanced}})
	for _, wanted := range []Capability{CapabilityErrorsV1, CapabilityLimitsV1, CapabilityReverseTCP, CapabilityReverseUDP, CapabilityRelayBalanced, CapabilityEgressProxy} {
		if !SupportsCapability(agentCaps, wanted) {
			t.Fatalf("agent capabilities missing %v: %#v", wanted, agentCaps)
		}
	}
	if got := AgentCapabilities(nil); len(got) != 2 || !SupportsCapability(got, CapabilityErrorsV1) {
		t.Fatalf("nil agent capabilities = %#v", got)
	}

	gatewayOptions := &config.GatewayOptions{
		Agents: []config.GatewayAgentOptions{
			{
				Reverse: config.ReverseACL{TCPPorts: []string{"80"}, UDPPorts: []string{"53"}},
				Egress:  config.EgressPolicy{Enabled: true},
			},
		},
	}
	gatewayCaps := GatewayCapabilities(gatewayOptions)
	for _, wanted := range []Capability{CapabilityErrorsV1, CapabilityLimitsV1, CapabilityReverseTCP, CapabilityReverseUDP, CapabilityRelayBalanced, CapabilityEgressProxy} {
		if !SupportsCapability(gatewayCaps, wanted) {
			t.Fatalf("gateway capabilities missing %v: %#v", wanted, gatewayCaps)
		}
	}
	limits := LimitsFromConfig(config.Limits{MaxFrameBytes: 2048, MaxRecordBytes: 1024, MaxUDPBytes: 512, MaxStreamsPerAgent: 8}, 0)
	if limits.MaxStreams != 8 {
		t.Fatalf("fallback stream limit = %d", limits.MaxStreams)
	}
}
