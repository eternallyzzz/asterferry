package config

import (
	"bytes"
	"errors"
	"fmt"
)

// AgentOptions is the normalized runtime view of an agent configuration.
// It deliberately contains no role union and no YAML-only defaults. The
// command layer resolves a Config once, then passes this value to the agent
// runtime.
type AgentOptions struct {
	Transport            TransportConfig
	Management           ManagementConfig
	Limits               Limits
	Obfuscation          ObfuscationConfig
	TransportObfuscation TransportObfuscationOptions
	Logging              LoggingOptions
	Agent                AgentRuntime
	Token                []byte
	// StreamLimit is the usable data-stream budget after reserving the
	// control stream from the QUIC incoming-stream limit.
	StreamLimit int64
}

type SamplingOptions struct {
	Enabled            bool
	RatePerSecond      int64
	Burst              int64
	SummaryIntervalSec int64
	MaxKeys            int64
}

type LoggingOptions struct {
	Level               string
	Format              string
	Sampling            SamplingOptions
	ExposeDomainAtDebug bool
}

type SniffOptions struct {
	Enabled       bool
	MaxBytes      int64
	TimeoutMillis int64
}

type ProxyOptions struct {
	Inbounds     []Inbound
	DefaultRoute string
	Rules        []RouteRule
	Sniff        SniffOptions
}

type AgentRuntime struct {
	ID      string
	Server  string
	TLS     AgentTLS
	Proxy   ProxyOptions
	Reverse []Tunnel
}

// GatewayAgentOptions contains the credentials and policy belonging to one
// gateway-side agent. Token files are read during resolution so the runtime
// never needs to understand the configuration file system layout.
type GatewayAgentOptions struct {
	ID      string
	Token   []byte
	Reverse ReverseACL
	Egress  EgressPolicy
}

// GatewayOptions is the normalized runtime view of a gateway configuration.
type GatewayOptions struct {
	Transport            TransportConfig
	Management           ManagementConfig
	Limits               Limits
	Obfuscation          ObfuscationConfig
	TransportObfuscation TransportObfuscationOptions
	Logging              LoggingOptions
	Gateway              GatewayConfig
	Agents               []GatewayAgentOptions
	// StreamLimit is the usable data-stream budget after reserving the
	// control stream from the QUIC incoming-stream limit.
	StreamLimit int64
}

// TransportObfuscationOptions is the resolved, immutable runtime view of the
// outer UDP obfuscation layer. Secrets are loaded once during config
// resolution and are never re-read by the QUIC adapter.
type TransportObfuscationOptions struct {
	Mode               string
	CurrentKey         []byte
	PreviousKey        []byte
	HandshakeShaping   bool
	MinFragmentBytes   int64
	MaxFragmentBytes   int64
	MaxWirePacketBytes int64
}

// ResolveAgent converts a validated YAML document into immutable runtime
// inputs. It also reads the agent token once, keeping file handling out of the
// transport and session layers.
func (c *Config) ResolveAgent() (*AgentOptions, error) {
	if c == nil || c.Role != RoleAgent || c.Agent == nil {
		return nil, errors.New("agent configuration is required")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	token, err := ReadToken(c.Agent.TokenFile)
	if err != nil {
		return nil, err
	}
	transportObfuscation, err := resolveTransportObfuscation(c.Obfuscation.Transport)
	if err != nil {
		return nil, err
	}
	proxy := ProxyOptions{
		Inbounds:     append([]Inbound(nil), c.Agent.Proxy.Inbounds...),
		DefaultRoute: c.Agent.Proxy.DefaultRoute,
		Rules:        cloneRouteRules(c.Agent.Proxy.Rules),
		Sniff: SniffOptions{
			Enabled:       c.Agent.Proxy.Sniff.Enabled != nil && *c.Agent.Proxy.Sniff.Enabled,
			MaxBytes:      c.Agent.Proxy.Sniff.MaxBytes,
			TimeoutMillis: c.Agent.Proxy.Sniff.TimeoutMillis,
		},
	}
	reverse := append([]Tunnel(nil), c.Agent.Reverse...)
	return &AgentOptions{
		Transport:            c.Transport,
		Management:           c.Management,
		Limits:               c.Limits,
		Obfuscation:          c.Obfuscation,
		TransportObfuscation: transportObfuscation,
		Logging:              resolveLogging(c.Logging),
		Agent:                AgentRuntime{ID: c.Agent.ID, Server: c.Agent.Server, TLS: c.Agent.TLS, Proxy: proxy, Reverse: reverse},
		Token:                append([]byte(nil), token...),
		StreamLimit:          UsableStreamLimit(c.Transport, c.Limits),
	}, nil
}

// ResolveGateway converts a validated YAML document into normalized runtime
// inputs and reads all configured agent tokens before the listener starts.
func (c *Config) ResolveGateway() (*GatewayOptions, error) {
	if c == nil || c.Role != RoleGateway || c.Gateway == nil {
		return nil, errors.New("gateway configuration is required")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	transportObfuscation, err := resolveTransportObfuscation(c.Obfuscation.Transport)
	if err != nil {
		return nil, err
	}
	agents := make([]GatewayAgentOptions, 0, len(c.Gateway.Agents))
	for _, raw := range c.Gateway.Agents {
		token, err := ReadToken(raw.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", raw.ID, err)
		}
		agents = append(agents, GatewayAgentOptions{
			ID:      raw.ID,
			Token:   append([]byte(nil), token...),
			Reverse: ReverseACL{TCPPorts: append([]string(nil), raw.Reverse.TCPPorts...), UDPPorts: append([]string(nil), raw.Reverse.UDPPorts...)},
			Egress:  cloneEgressPolicy(raw.Egress),
		})
	}
	return &GatewayOptions{
		Transport:            c.Transport,
		Management:           c.Management,
		Limits:               c.Limits,
		Obfuscation:          c.Obfuscation,
		TransportObfuscation: transportObfuscation,
		Logging:              resolveLogging(c.Logging),
		Gateway: GatewayConfig{
			Listen: c.Gateway.Listen,
			TLS:    c.Gateway.TLS,
		},
		Agents:      agents,
		StreamLimit: UsableStreamLimit(c.Transport, c.Limits),
	}, nil
}

func resolveTransportObfuscation(raw TransportObfuscationConfig) (TransportObfuscationOptions, error) {
	result := TransportObfuscationOptions{
		Mode:               raw.Mode,
		HandshakeShaping:   raw.HandshakeShaping != nil && *raw.HandshakeShaping,
		MinFragmentBytes:   raw.MinFragmentBytes,
		MaxFragmentBytes:   raw.MaxFragmentBytes,
		MaxWirePacketBytes: raw.MaxWirePacketBytes,
	}
	if result.Mode == TransportObfuscationStandard {
		return result, nil
	}
	current, err := ReadSecret(raw.KeyFile)
	if err != nil {
		return TransportObfuscationOptions{}, fmt.Errorf("read transport obfuscation key: %w", err)
	}
	result.CurrentKey = current
	if raw.PreviousKeyFile != "" {
		previous, err := ReadSecret(raw.PreviousKeyFile)
		if err != nil {
			return TransportObfuscationOptions{}, fmt.Errorf("read previous transport obfuscation key: %w", err)
		}
		if bytes.Equal(current, previous) {
			return TransportObfuscationOptions{}, errors.New("transport obfuscation current and previous keys must differ")
		}
		result.PreviousKey = previous
	}
	return result, nil
}

func resolveLogging(raw LoggingConfig) LoggingOptions {
	sampling := SamplingOptions{Enabled: raw.Sampling.Enabled == nil || *raw.Sampling.Enabled, RatePerSecond: raw.Sampling.RatePerSecond, Burst: raw.Sampling.Burst, SummaryIntervalSec: raw.Sampling.SummaryIntervalSec, MaxKeys: raw.Sampling.MaxKeys}
	return LoggingOptions{Level: raw.Level, Format: raw.Format, Sampling: sampling, ExposeDomainAtDebug: raw.ExposeDomainAtDebug}
}

func cloneRouteRules(rules []RouteRule) []RouteRule {
	result := make([]RouteRule, len(rules))
	for i, rule := range rules {
		result[i] = RouteRule{
			Inbound: rule.Inbound,
			CIDRs:   append([]string(nil), rule.CIDRs...),
			GeoIP:   append([]string(nil), rule.GeoIP...),
			Domains: append([]string(nil), rule.Domains...),
			Route:   rule.Route,
		}
	}
	return result
}

func cloneEgressPolicy(raw EgressPolicy) EgressPolicy {
	raw.TCPPorts = append([]string(nil), raw.TCPPorts...)
	raw.UDPPorts = append([]string(nil), raw.UDPPorts...)
	raw.AllowCIDRs = append([]string(nil), raw.AllowCIDRs...)
	return raw
}
