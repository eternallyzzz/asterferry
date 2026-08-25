package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

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
		Version:    ConfigVersion,
		Role:       RoleGateway,
		Transport:  TransportConfig{ALPN: "af-test-123456"},
		Management: ManagementConfig{AuthTokenFile: "management-token"},
		Obfuscation: ObfuscationConfig{Transport: TransportObfuscationConfig{
			Mode: TransportObfuscationStandard,
		}},
		Gateway: &GatewayConfig{
			Listen: ":4433",
			TLS:    GatewayTLS{CertFile: "cert", KeyFile: "key", ClientCAFile: "client-ca"},
			Agents: []GatewayAgent{{ID: "edge", TokenFile: "token", Reverse: ReverseACL{TCPPorts: []string{"28080"}}}},
		},
	}
}

func TestV3DefaultsAndRoleValidation(t *testing.T) {
	c := gatewayConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Management.Listen != "127.0.0.1:9090" || c.Shutdown.GracePeriodSec != 30 || c.Limits.MaxFrameBytes == 0 || c.Obfuscation.ProxyProfile != ProfileBalanced {
		t.Fatal("defaults were not applied")
	}
	if c.Management.Web.Enabled == nil || !*c.Management.Web.Enabled {
		t.Fatal("management web should be enabled by default")
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

func TestManagementListenSecurityDefaults(t *testing.T) {
	c := gatewayConfig()
	c.Management.Listen = "0.0.0.0:9090"
	if err := c.Validate(); err == nil {
		t.Fatal("non-loopback management listener without TLS should fail")
	}
	c.Management.TLS.CertFile = "management.crt"
	c.Management.TLS.KeyFile = "management.key"
	if err := c.Validate(); err != nil {
		t.Fatalf("non-loopback management listener with TLS rejected: %v", err)
	}
	c.Management.TLS.CertFile = ""
	c.Management.TLS.KeyFile = "management.key"
	if err := c.Validate(); err == nil {
		t.Fatal("management TLS certificate/key mismatch should fail")
	}
	c = gatewayConfig()
	disabled := false
	c.Management.Web.Enabled = &disabled
	if err := c.Validate(); err != nil {
		t.Fatalf("explicit Dashboard disable rejected: %v", err)
	}
	if c.Management.Web.Enabled == nil || *c.Management.Web.Enabled {
		t.Fatal("explicit Dashboard disable was not retained")
	}
}

func TestGatewayEgressDefaultConnectionLimitIsPersisted(t *testing.T) {
	c := gatewayConfig()
	c.Gateway.Agents[0].Reverse = ReverseACL{}
	c.Gateway.Agents[0].Egress = EgressPolicy{Enabled: true, TCPPorts: []string{"443"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, want := c.Gateway.Agents[0].Egress.MaxConnections, c.Limits.MaxConnectionsPerAgent; got != want {
		t.Fatalf("egress max connections = %d, want default %d", got, want)
	}
}

func TestShutdownGracePeriodValidation(t *testing.T) {
	for _, value := range []int64{-1, 3601} {
		c := gatewayConfig()
		c.Shutdown.GracePeriodSec = value
		if err := c.Validate(); err == nil {
			t.Fatalf("grace period %d should be rejected", value)
		}
	}
	c := gatewayConfig()
	c.Shutdown.GracePeriodSec = 45
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Shutdown.GracePeriodSec != 45 {
		t.Fatalf("explicit grace period was changed: %d", c.Shutdown.GracePeriodSec)
	}
}

func TestUsableStreamLimitReservesControlStream(t *testing.T) {
	limits := Limits{MaxStreamsPerAgent: 256}
	if got := UsableStreamLimit(TransportConfig{MaxBidiRemoteStreams: 256}, limits); got != 255 {
		t.Fatalf("usable stream limit = %d, want 255", got)
	}
	if got := UsableStreamLimit(TransportConfig{MaxBidiRemoteStreams: 512}, limits); got != 256 {
		t.Fatalf("usable stream limit = %d, want 256", got)
	}
}

func TestTransportPerformanceLimitsValidate(t *testing.T) {
	c := gatewayConfig()
	c.Transport.InitialStreamReceiveWindow = 1 << 20
	c.Transport.MaxStreamReadBuffer = 8 << 20
	c.Transport.InitialConnectionReceiveWindow = 4 << 20
	c.Transport.MaxConnReadBuffer = 32 << 20
	c.Transport.UDPReadBufferBytes = 8 << 20
	c.Transport.UDPWriteBufferBytes = 8 << 20
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := gatewayConfig()
	bad.Transport.InitialStreamReceiveWindow = 8 << 20
	bad.Transport.MaxStreamReadBuffer = 1 << 20
	if err := bad.Validate(); err == nil {
		t.Fatal("initial stream window exceeding max should fail")
	}
}

func TestRelayBatchLimitValidation(t *testing.T) {
	c := gatewayConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Limits.MaxRecordBytes != 64<<10 || c.Limits.MaxWriteBatchBytes != 256<<10 {
		t.Fatalf("v5 relay defaults = record %d, batch %d", c.Limits.MaxRecordBytes, c.Limits.MaxWriteBatchBytes)
	}
	for _, batch := range []int64{1024, 4<<20 + 1} {
		bad := gatewayConfig()
		bad.Limits.MaxRecordBytes = 64 << 10
		bad.Limits.MaxWriteBatchBytes = batch
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid write batch %d was accepted", batch)
		}
	}
}

func TestClusterNodeIDValidationAndRuntimeResolution(t *testing.T) {
	c := gatewayConfig()
	c.Cluster.NodeID = "gateway-a"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := gatewayConfig()
	bad.Cluster.NodeID = "gateway/a"
	if err := bad.Validate(); err == nil {
		t.Fatal("invalid cluster node id should fail validation")
	}
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "gateway.token")
	if err := os.WriteFile(tokenPath, []byte("01234567890123456789012345678901"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Gateway.Agents[0].TokenFile = tokenPath
	c.Management.AuthTokenFile = tokenPath
	opts, err := c.ResolveGateway()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Cluster.NodeID != "gateway-a" {
		t.Fatalf("resolved node id = %q", opts.Cluster.NodeID)
	}
}

func TestTransportObfuscationDefaultsAndResolution(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "agent.token")
	keyPath := filepath.Join(dir, "obfs.key")
	if err := os.WriteFile(tokenPath, []byte("01234567890123456789012345678901"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Config{
		Version:   ConfigVersion,
		Role:      RoleAgent,
		Transport: TransportConfig{ALPN: "af-test-123456"},
		Obfuscation: ObfuscationConfig{Transport: TransportObfuscationConfig{
			KeyFile: keyPath,
		}},
		Agent: &AgentConfig{
			ID: "edge", Server: "gateway.example.com:4433", TokenFile: tokenPath,
			TLS:   AgentTLS{CAFile: "ca", CertFile: "cert", KeyFile: "key", ServerName: "gateway.example.com"},
			Proxy: ProxyConfig{Inbounds: []Inbound{{Tag: "socks", Protocol: "socks5", Listen: "127.0.0.1:1080"}}},
		},
		Management: ManagementConfig{AuthTokenFile: tokenPath},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Obfuscation.Transport.Mode != TransportObfuscationCamouflage || c.Obfuscation.Transport.HandshakeShaping == nil || !*c.Obfuscation.Transport.HandshakeShaping {
		t.Fatalf("camouflage defaults were not applied: %#v", c.Obfuscation.Transport)
	}
	opts, err := c.ResolveAgent()
	if err != nil {
		t.Fatal(err)
	}
	if string(opts.TransportObfuscation.CurrentKey) != "0123456789abcdef0123456789abcdef" || !opts.TransportObfuscation.HandshakeShaping {
		t.Fatalf("transport obfuscation was not resolved: %#v", opts.TransportObfuscation)
	}
}

func TestTransportObfuscationRejectsInvalidRotation(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "current.key")
	previous := filepath.Join(dir, "previous.key")
	for _, path := range []string{current, previous} {
		if err := os.WriteFile(path, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := gatewayConfig()
	c.Obfuscation.Transport = TransportObfuscationConfig{Mode: TransportObfuscationCamouflage, KeyFile: current, PreviousKeyFile: previous}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "gateway.token")
	if err := os.WriteFile(tokenPath, []byte("01234567890123456789012345678901"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Gateway.Agents[0].TokenFile = tokenPath
	c.Management.AuthTokenFile = tokenPath
	if _, err := c.ResolveGateway(); err == nil {
		t.Fatal("identical current and previous keys should be rejected")
	}
}

func TestAgentProxySecurityValidation(t *testing.T) {
	c := Config{
		Version:   ConfigVersion,
		Role:      RoleAgent,
		Transport: TransportConfig{ALPN: "af-test-123456"},
		Obfuscation: ObfuscationConfig{Transport: TransportObfuscationConfig{
			Mode: TransportObfuscationStandard,
		}},
		Agent: &AgentConfig{
			ID:        "edge",
			Server:    "gateway.example.com:4433",
			TokenFile: "token",
			TLS:       AgentTLS{CAFile: "ca", CertFile: "cert", KeyFile: "key", ServerName: "gateway.example.com"},
			Proxy:     ProxyConfig{Inbounds: []Inbound{{Tag: "socks", Protocol: "socks5", Listen: "127.0.0.1:1080"}}},
		},
		Management: ManagementConfig{AuthTokenFile: "management-token"},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Agent.Proxy.Sniff.Enabled = boolPtr(false)
	if err := c.Validate(); err != nil || c.Agent.Proxy.Sniff.Enabled == nil || *c.Agent.Proxy.Sniff.Enabled {
		t.Fatalf("explicit sniff disable was not retained: %v", err)
	}
	c.Agent.Proxy.Inbounds[0].Listen = "0.0.0.0:1080"
	if err := c.Validate(); err == nil {
		t.Fatal("non-loopback proxy without credentials should be rejected")
	}
}

func TestEnvironmentOverrides(t *testing.T) {
	c := gatewayConfig()
	values := map[string]string{
		"ASTERFERRY_LOG_LEVEL":               "debug",
		"ASTERFERRY_LOG_FORMAT":              "text",
		"ASTERFERRY_LOG_SAMPLE_RATE":         "9",
		"ASTERFERRY_LOG_SAMPLE_BURST":        "30",
		"ASTERFERRY_LOG_SAMPLE_MAX_KEYS":     "128",
		"ASTERFERRY_LOG_SAMPLING_ENABLED":    "false",
		"ASTERFERRY_LOG_EXPOSE_DOMAIN_DEBUG": "true",
		"ASTERFERRY_SHUTDOWN_GRACE_PERIOD":   "45",
	}
	if err := ApplyEnvLookup(&c, func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}); err != nil {
		t.Fatal(err)
	}
	if c.Logging.Level != "debug" || c.Logging.Format != "text" || c.Logging.Sampling.RatePerSecond != 9 || c.Logging.Sampling.Burst != 30 || c.Logging.Sampling.MaxKeys != 128 || c.Logging.Sampling.Enabled == nil || *c.Logging.Sampling.Enabled || !c.Logging.ExposeDomainAtDebug || c.Shutdown.GracePeriodSec != 45 {
		t.Fatalf("environment overrides not applied: %#v", c.Logging)
	}
	values["ASTERFERRY_LOG_SAMPLE_RATE"] = "not-a-number"
	if err := ApplyEnvLookup(&c, func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}); err == nil {
		t.Fatal("invalid environment value should fail")
	}
}

func TestExplicitDisableFlagsSurviveDefaults(t *testing.T) {
	var proxy ProxyConfig
	if err := yaml.Unmarshal([]byte("sniff:\n  enabled: false\n"), &proxy); err != nil {
		t.Fatal(err)
	}
	if proxy.Sniff.Enabled == nil || *proxy.Sniff.Enabled {
		t.Fatal("explicit sniff.enabled=false was not retained")
	}
	var logging LoggingConfig
	if err := yaml.Unmarshal([]byte("sampling:\n  enabled: false\n"), &logging); err != nil {
		t.Fatal(err)
	}
	if logging.Sampling.Enabled == nil || *logging.Sampling.Enabled {
		t.Fatal("explicit logging.sampling.enabled=false was not retained")
	}
}

func TestResolveRuntimeOptions(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "agent.token")
	if err := os.WriteFile(tokenPath, []byte("01234567890123456789012345678901"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Config{
		Version:   ConfigVersion,
		Role:      RoleAgent,
		Transport: TransportConfig{ALPN: "af-test-123456"},
		Obfuscation: ObfuscationConfig{Transport: TransportObfuscationConfig{
			Mode: TransportObfuscationStandard,
		}},
		Agent: &AgentConfig{
			ID:        "edge",
			Server:    "gateway.example.com:4433",
			TokenFile: tokenPath,
			TLS:       AgentTLS{CAFile: "ca", CertFile: "cert", KeyFile: "key", ServerName: "gateway.example.com"},
			Proxy:     ProxyConfig{Sniff: SniffConfig{Enabled: boolPtr(false)}, Inbounds: []Inbound{{Tag: "socks", Protocol: "socks5", Listen: "127.0.0.1:1080"}}},
		},
		Management: ManagementConfig{AuthTokenFile: tokenPath},
	}
	opts, err := c.ResolveAgent()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Agent.Proxy.Sniff.Enabled || opts.Shutdown.GracePeriod != 30*time.Second || string(opts.Token) != "01234567890123456789012345678901" {
		t.Fatalf("runtime options lost resolved values: %#v", opts)
	}
	c.Agent.Proxy.Inbounds[0].Tag = "mutated"
	if opts.Agent.Proxy.Inbounds[0].Tag == "mutated" {
		t.Fatal("runtime options must not alias inbound configuration")
	}
}

func TestResolveGatewayReadsCredentials(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "gateway.token")
	if err := os.WriteFile(tokenPath, []byte("01234567890123456789012345678901"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := gatewayConfig()
	c.Gateway.Agents[0].TokenFile = tokenPath
	c.Management.AuthTokenFile = tokenPath
	opts, err := c.ResolveGateway()
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Agents) != 1 || string(opts.Agents[0].Token) != "01234567890123456789012345678901" {
		t.Fatalf("gateway credentials were not resolved: %#v", opts.Agents)
	}
	if opts.Cluster.NodeID == "" {
		t.Fatal("gateway node identity was not resolved")
	}
}
