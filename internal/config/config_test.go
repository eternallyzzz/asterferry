package config

import "testing"

func TestParsePortRanges(t *testing.T) {
	ranges, err := ParsePortRanges([]string{"2000-2002", "2100"})
	if err != nil || len(ranges) != 2 {
		t.Fatalf("parse ranges: %v %#v", err, ranges)
	}
	if !ranges[0].Contains(2001) || ranges[0].Contains(2003) || !ranges[1].Contains(2100) {
		t.Fatal("range contains behavior is wrong")
	}
	if _, err := ParsePortRanges([]string{"0", "3000-2000"}); err == nil {
		t.Fatal("expected invalid range")
	}
}

func gatewayConfig() Config {
	return Config{
		Version:   ConfigVersion,
		Role:      RoleGateway,
		Transport: TransportConfig{ALPN: "af-test-123456"},
		Gateway: &GatewayConfig{
			Listen: ":4433",
			TLS:    GatewayTLS{CertFile: "cert", KeyFile: "key", ClientCAFile: "client-ca"},
			Agents: []GatewayAgent{{ID: "edge", TokenFile: "token", Reverse: ReverseACL{TCPPorts: []string{"28080"}}}},
		},
	}
}

func TestV2DefaultsAndRoleValidation(t *testing.T) {
	c := gatewayConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Management.Listen != "127.0.0.1:9090" || c.Limits.MaxFrameBytes == 0 || c.Obfuscation.ProxyProfile != ProfileBalanced {
		t.Fatal("defaults were not applied")
	}
	bad := gatewayConfig()
	bad.Gateway.Agents[0].Reverse = ReverseACL{}
	if err := bad.Validate(); err == nil {
		t.Fatal("empty ACL should be rejected by a gateway")
	}
	bad = gatewayConfig()
	bad.Agent = &AgentConfig{}
	if err := bad.Validate(); err == nil {
		t.Fatal("mixed role sections should be rejected")
	}
}

func TestAgentProxySecurityValidation(t *testing.T) {
	c := Config{
		Version:   ConfigVersion,
		Role:      RoleAgent,
		Transport: TransportConfig{ALPN: "af-test-123456"},
		Agent: &AgentConfig{
			ID:        "edge",
			Server:    "gateway.example.com:4433",
			TokenFile: "token",
			TLS:       AgentTLS{CAFile: "ca", CertFile: "cert", KeyFile: "key", ServerName: "gateway.example.com"},
			Proxy:     ProxyConfig{Inbounds: []Inbound{{Tag: "socks", Protocol: "socks5", Listen: "127.0.0.1:1080"}}},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Agent.Proxy.Inbounds[0].Listen = "0.0.0.0:1080"
	if err := c.Validate(); err == nil {
		t.Fatal("non-loopback proxy without credentials should be rejected")
	}
}
