package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ConfigVersion = 2

	RoleGateway = "gateway"
	RoleAgent   = "agent"

	RouteDirect  = "direct"
	RouteGateway = "gateway"

	ProfileStandard = "standard"
	ProfileBalanced = "balanced"

	relayRecordHeaderBytes = 8
)

// Config is a strict role-specific document. The selected role must be the
// only role section present; this keeps accepted configuration and runtime
// behavior aligned.
type Config struct {
	Version     int               `yaml:"version"`
	Role        string            `yaml:"role"`
	Transport   TransportConfig   `yaml:"transport"`
	Management  ManagementConfig  `yaml:"management"`
	Limits      Limits            `yaml:"limits"`
	Obfuscation ObfuscationConfig `yaml:"obfuscation"`
	Gateway     *GatewayConfig    `yaml:"gateway"`
	Agent       *AgentConfig      `yaml:"agent"`
}

type TransportConfig struct {
	ALPN                 string `yaml:"alpn"`
	MaxBidiRemoteStreams int64  `yaml:"max_bidi_remote_streams"`
	MaxStreamReadBuffer  int64  `yaml:"max_stream_read_buffer"`
	MaxStreamWriteBuffer int64  `yaml:"max_stream_write_buffer"`
	MaxConnReadBuffer    int64  `yaml:"max_conn_read_buffer"`
	HandshakeTimeoutSec  int64  `yaml:"handshake_timeout_seconds"`
	IdleTimeoutSec       int64  `yaml:"idle_timeout_seconds"`
	KeepAliveSec         int64  `yaml:"keep_alive_seconds"`
}

type ManagementConfig struct {
	Listen string `yaml:"listen"`
}

type Limits struct {
	MaxAgents              int64 `yaml:"max_agents"`
	MaxConnectionsPerAgent int64 `yaml:"max_connections_per_agent"`
	MaxStreamsPerAgent     int64 `yaml:"max_streams_per_agent"`
	MaxFrameBytes          int64 `yaml:"max_frame_bytes"`
	MaxRecordBytes         int64 `yaml:"max_record_bytes"`
	MaxUDPBytes            int64 `yaml:"max_udp_bytes"`
	DialTimeoutSec         int64 `yaml:"dial_timeout_seconds"`
	UDPIdleTimeoutSec      int64 `yaml:"udp_idle_timeout_seconds"`
}

type ObfuscationConfig struct {
	ProxyProfile    string `yaml:"proxy_profile"`
	ReverseProfile  string `yaml:"reverse_profile"`
	MaxPaddingBytes int64  `yaml:"max_padding_bytes"`
}

type GatewayConfig struct {
	Listen string         `yaml:"listen"`
	TLS    GatewayTLS     `yaml:"tls"`
	Agents []GatewayAgent `yaml:"agents"`
}

type GatewayTLS struct {
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

type GatewayAgent struct {
	ID        string       `yaml:"id"`
	TokenFile string       `yaml:"token_file"`
	Reverse   ReverseACL   `yaml:"reverse"`
	Egress    EgressPolicy `yaml:"egress"`
}

type ReverseACL struct {
	TCPPorts []string `yaml:"tcp_ports"`
	UDPPorts []string `yaml:"udp_ports"`
}

type EgressPolicy struct {
	Enabled             bool     `yaml:"enabled"`
	TCPPorts            []string `yaml:"tcp_ports"`
	UDPPorts            []string `yaml:"udp_ports"`
	AllowCIDRs          []string `yaml:"allow_cidrs"`
	DenyPrivateNetworks bool     `yaml:"deny_private_networks"`
	MaxConnections      int64    `yaml:"max_connections"`
}

type AgentConfig struct {
	ID        string      `yaml:"id"`
	Server    string      `yaml:"server"`
	TLS       AgentTLS    `yaml:"tls"`
	TokenFile string      `yaml:"token_file"`
	Proxy     ProxyConfig `yaml:"proxy"`
	Reverse   []Tunnel    `yaml:"reverse"`
}

type AgentTLS struct {
	CAFile     string `yaml:"ca_file"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	ServerName string `yaml:"server_name"`
}

type ProxyConfig struct {
	Inbounds     []Inbound   `yaml:"inbounds"`
	DefaultRoute string      `yaml:"default_route"`
	Rules        []RouteRule `yaml:"rules"`
}

type Inbound struct {
	Tag      string `yaml:"tag"`
	Protocol string `yaml:"protocol"`
	Listen   string `yaml:"listen"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

type RouteRule struct {
	Inbound string   `yaml:"inbound"`
	CIDRs   []string `yaml:"cidrs"`
	GeoIP   []string `yaml:"geoip"`
	Domains []string `yaml:"domains"`
	Route   string   `yaml:"route"`
}

type Tunnel struct {
	Name        string `yaml:"name"`
	Protocol    string `yaml:"protocol"`
	Local       string `yaml:"local"`
	GatewayPort uint16 `yaml:"gateway_port"`
}

type PortRange struct{ Min, Max uint16 }

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) Validate() error {
	if c.Version != ConfigVersion {
		return fmt.Errorf("version must be %d", ConfigVersion)
	}
	if c.Role != RoleGateway && c.Role != RoleAgent {
		return errors.New("role must be gateway or agent")
	}
	if c.Gateway != nil && c.Agent != nil {
		return errors.New("gateway and agent sections are mutually exclusive")
	}
	if c.Role == RoleGateway && c.Gateway == nil {
		return errors.New("gateway section is required for gateway role")
	}
	if c.Role == RoleAgent && c.Agent == nil {
		return errors.New("agent section is required for agent role")
	}
	if c.Role == RoleGateway && c.Agent != nil {
		return errors.New("agent section is not valid for gateway role")
	}
	if c.Role == RoleAgent && c.Gateway != nil {
		return errors.New("gateway section is not valid for agent role")
	}
	if c.Transport.ALPN == "" || len(c.Transport.ALPN) < 8 || len(c.Transport.ALPN) > 64 {
		return errors.New("transport.alpn must be between 8 and 64 characters")
	}
	for _, r := range c.Transport.ALPN {
		if r < 0x21 || r > 0x7e {
			return errors.New("transport.alpn must contain printable ASCII only")
		}
	}
	if c.Management.Listen == "" {
		if c.Role == RoleGateway {
			c.Management.Listen = "127.0.0.1:9090"
		} else {
			c.Management.Listen = "127.0.0.1:9091"
		}
	}
	if !isLoopbackListen(c.Management.Listen) {
		return errors.New("management.listen must bind to loopback")
	}
	if c.Transport.MaxBidiRemoteStreams == 0 {
		c.Transport.MaxBidiRemoteStreams = 256
	}
	if c.Transport.MaxBidiRemoteStreams < 1 || c.Transport.MaxBidiRemoteStreams > 65536 {
		return errors.New("transport.max_bidi_remote_streams is out of range")
	}
	if c.Transport.HandshakeTimeoutSec == 0 {
		c.Transport.HandshakeTimeoutSec = 10
	}
	if c.Transport.IdleTimeoutSec == 0 {
		c.Transport.IdleTimeoutSec = 1800
	}
	if c.Transport.KeepAliveSec == 0 {
		c.Transport.KeepAliveSec = 20
	}
	if c.Transport.HandshakeTimeoutSec < 1 || c.Transport.IdleTimeoutSec < 1 || c.Transport.KeepAliveSec < 1 {
		return errors.New("transport timeouts must be positive")
	}
	if c.Transport.MaxStreamReadBuffer < 0 || c.Transport.MaxStreamWriteBuffer < 0 || c.Transport.MaxConnReadBuffer < 0 {
		return errors.New("transport buffer limits cannot be negative")
	}
	if c.Limits.MaxAgents == 0 {
		c.Limits.MaxAgents = 256
	}
	if c.Limits.MaxConnectionsPerAgent == 0 {
		c.Limits.MaxConnectionsPerAgent = 64
	}
	if c.Limits.MaxStreamsPerAgent == 0 {
		c.Limits.MaxStreamsPerAgent = 256
	}
	if c.Limits.MaxFrameBytes == 0 {
		c.Limits.MaxFrameBytes = 16 << 20
	}
	if c.Limits.MaxRecordBytes == 0 {
		c.Limits.MaxRecordBytes = 16 << 10
	}
	if c.Limits.MaxUDPBytes == 0 {
		c.Limits.MaxUDPBytes = 64 << 10
	}
	if c.Limits.DialTimeoutSec == 0 {
		c.Limits.DialTimeoutSec = 10
	}
	if c.Limits.UDPIdleTimeoutSec == 0 {
		c.Limits.UDPIdleTimeoutSec = 30
	}
	if c.Limits.MaxAgents < 1 || c.Limits.MaxAgents > 100000 || c.Limits.MaxConnectionsPerAgent < 1 || c.Limits.MaxConnectionsPerAgent > 65536 || c.Limits.MaxStreamsPerAgent < 1 || c.Limits.MaxStreamsPerAgent > 65536 {
		return errors.New("limits must be positive")
	}
	if c.Limits.MaxFrameBytes < 1024 || c.Limits.MaxFrameBytes > 64<<20 {
		return errors.New("limits.max_frame_bytes must be between 1KiB and 64MiB")
	}
	if c.Limits.MaxRecordBytes < 1024 || c.Limits.MaxRecordBytes > c.Limits.MaxFrameBytes {
		return errors.New("limits.max_record_bytes is out of range")
	}
	if c.Limits.MaxUDPBytes < 512 || c.Limits.MaxUDPBytes > 64<<10 {
		return errors.New("limits.max_udp_bytes must be between 512B and 64KiB")
	}
	if c.Limits.DialTimeoutSec < 1 || c.Limits.DialTimeoutSec > 300 {
		return errors.New("limits.dial_timeout_seconds must be between 1 and 300")
	}
	if c.Limits.UDPIdleTimeoutSec < 1 {
		return errors.New("limits.udp_idle_timeout_seconds must be positive")
	}
	if c.Obfuscation.ProxyProfile == "" {
		c.Obfuscation.ProxyProfile = ProfileBalanced
	}
	if c.Obfuscation.ReverseProfile == "" {
		c.Obfuscation.ReverseProfile = ProfileStandard
	}
	if !validProfile(c.Obfuscation.ProxyProfile) || !validProfile(c.Obfuscation.ReverseProfile) {
		return errors.New("obfuscation profiles must be standard or balanced")
	}
	if c.Obfuscation.MaxPaddingBytes == 0 {
		c.Obfuscation.MaxPaddingBytes = 2048
	}
	if c.Obfuscation.MaxPaddingBytes < 0 || c.Obfuscation.MaxPaddingBytes > 64<<10 {
		return errors.New("obfuscation.max_padding_bytes is out of range")
	}
	if c.Obfuscation.MaxPaddingBytes > c.Limits.MaxRecordBytes-relayRecordHeaderBytes {
		return errors.New("obfuscation.max_padding_bytes exceeds max record size")
	}
	if c.Role == RoleGateway {
		return c.validateGateway()
	}
	return c.validateAgent()
}

func (c *Config) validateGateway() error {
	g := c.Gateway
	if g.Listen == "" {
		return errors.New("gateway.listen is required")
	}
	if err := validateEndpoint(g.Listen, "gateway.listen"); err != nil {
		return err
	}
	if g.TLS.CertFile == "" || g.TLS.KeyFile == "" || g.TLS.ClientCAFile == "" {
		return errors.New("gateway.tls.cert_file, key_file and client_ca_file are required")
	}
	if len(g.Agents) == 0 {
		return errors.New("gateway.agents must contain at least one agent")
	}
	seen := map[string]bool{}
	for _, agent := range g.Agents {
		if agent.ID == "" || seen[agent.ID] {
			return errors.New("gateway agent IDs must be non-empty and unique")
		}
		seen[agent.ID] = true
		if agent.TokenFile == "" {
			return fmt.Errorf("agent %q has no token_file", agent.ID)
		}
		if err := validateACL(agent.Reverse.TCPPorts, agent.Reverse.UDPPorts, "reverse", agent.ID); err != nil {
			return err
		}
		if agent.Egress.Enabled {
			// Private, loopback, link-local and metadata destinations are denied
			// by default; an explicit future allow-private policy must be added
			// as a separate, auditable option rather than relying on bool zero.
			agent.Egress.DenyPrivateNetworks = true
			if agent.Egress.MaxConnections == 0 {
				agent.Egress.MaxConnections = c.Limits.MaxConnectionsPerAgent
			}
		}
		if err := validateEgress(agent.Egress, agent.ID); err != nil {
			return err
		}
		if len(agent.Reverse.TCPPorts) == 0 && len(agent.Reverse.UDPPorts) == 0 && !agent.Egress.Enabled {
			return fmt.Errorf("agent %q must define reverse ports or enable egress", agent.ID)
		}
	}
	return nil
}

func (c *Config) validateAgent() error {
	a := c.Agent
	if a.ID == "" || a.Server == "" || a.TokenFile == "" {
		return errors.New("agent.id, agent.server and agent.token_file are required")
	}
	if a.TLS.CAFile == "" || a.TLS.CertFile == "" || a.TLS.KeyFile == "" || a.TLS.ServerName == "" {
		return errors.New("agent.tls.ca_file, cert_file, key_file and server_name are required")
	}
	if err := validateEndpoint(a.Server, "agent.server"); err != nil {
		return err
	}
	if err := validateRemoteEndpoint(a.Server, "agent.server"); err != nil {
		return err
	}
	if len(a.Proxy.Inbounds) == 0 && len(a.Reverse) == 0 {
		return errors.New("agent must define proxy.inbounds or reverse mappings")
	}
	if a.Proxy.DefaultRoute == "" {
		a.Proxy.DefaultRoute = RouteGateway
	}
	if a.Proxy.DefaultRoute != RouteDirect && a.Proxy.DefaultRoute != RouteGateway {
		return errors.New("proxy.default_route must be direct or gateway")
	}
	seenTags := map[string]bool{}
	for _, in := range a.Proxy.Inbounds {
		if in.Tag == "" || seenTags[in.Tag] {
			return errors.New("proxy inbound tags must be non-empty and unique")
		}
		seenTags[in.Tag] = true
		if in.Protocol != "socks5" && in.Protocol != "http" {
			return fmt.Errorf("inbound %q protocol must be socks5 or http", in.Tag)
		}
		if _, err := net.ResolveTCPAddr("tcp", in.Listen); err != nil {
			return fmt.Errorf("inbound %q listen: %w", in.Tag, err)
		}
		if err := validateEndpoint(in.Listen, fmt.Sprintf("inbound %q listen", in.Tag)); err != nil {
			return err
		}
		if (in.User == "") != (in.Password == "") {
			return fmt.Errorf("inbound %q user and password must be set together", in.Tag)
		}
		if !isLoopbackListen(in.Listen) && in.User == "" {
			return fmt.Errorf("inbound %q must use credentials when not bound to loopback", in.Tag)
		}
	}
	seenNames := map[string]bool{}
	for _, t := range a.Reverse {
		if t.Name == "" || seenNames[t.Name] {
			return errors.New("reverse mapping names must be non-empty and unique")
		}
		seenNames[t.Name] = true
		if t.Protocol != "tcp" && t.Protocol != "udp" {
			return fmt.Errorf("mapping %q protocol must be tcp or udp", t.Name)
		}
		if t.GatewayPort == 0 {
			return fmt.Errorf("mapping %q gateway_port must be non-zero", t.Name)
		}
		if t.Protocol == "tcp" {
			if _, err := net.ResolveTCPAddr("tcp", t.Local); err != nil {
				return fmt.Errorf("mapping %q local: %w", t.Name, err)
			}
			if err := validateEndpoint(t.Local, fmt.Sprintf("mapping %q local", t.Name)); err != nil {
				return err
			}
			if err := validateRemoteEndpoint(t.Local, fmt.Sprintf("mapping %q local", t.Name)); err != nil {
				return err
			}
		} else if _, err := net.ResolveUDPAddr("udp", t.Local); err != nil {
			return fmt.Errorf("mapping %q local: %w", t.Name, err)
		} else if err := validateEndpoint(t.Local, fmt.Sprintf("mapping %q local", t.Name)); err != nil {
			return err
		} else if err := validateRemoteEndpoint(t.Local, fmt.Sprintf("mapping %q local", t.Name)); err != nil {
			return err
		}
	}
	for n, rule := range a.Proxy.Rules {
		if rule.Inbound != "" && !seenTags[rule.Inbound] {
			return fmt.Errorf("proxy rule %d references unknown inbound %q", n, rule.Inbound)
		}
		if rule.Route != RouteDirect && rule.Route != RouteGateway {
			return fmt.Errorf("proxy rule %d route must be direct or gateway", n)
		}
		for _, cidr := range rule.CIDRs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("proxy rule %d cidr: %w", n, err)
			}
		}
	}
	return nil
}

func validateACL(tcp, udp []string, kind, id string) error {
	if _, err := ParsePortRanges(tcp); err != nil {
		return fmt.Errorf("agent %q %s tcp ACL: %w", id, kind, err)
	}
	if _, err := ParsePortRanges(udp); err != nil {
		return fmt.Errorf("agent %q %s udp ACL: %w", id, kind, err)
	}
	return nil
}

func validateEgress(p EgressPolicy, id string) error {
	if !p.Enabled {
		return nil
	}
	if len(p.TCPPorts) == 0 && len(p.UDPPorts) == 0 {
		return fmt.Errorf("agent %q egress must define tcp_ports or udp_ports", id)
	}
	if err := validateACL(p.TCPPorts, p.UDPPorts, "egress", id); err != nil {
		return err
	}
	for _, cidr := range p.AllowCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("agent %q egress allow_cidr: %w", id, err)
		}
	}
	if p.MaxConnections < 0 || p.MaxConnections > 65536 {
		return fmt.Errorf("agent %q egress max_connections is out of range", id)
	}
	return nil
}

func validProfile(profile string) bool {
	return profile == ProfileStandard || profile == ProfileBalanced
}

func isLoopbackListen(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateEndpoint(address, name string) error {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("%s must use a non-zero port", name)
	}
	return nil
}

func validateRemoteEndpoint(address, name string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("%s must include a destination host", name)
	}
	return nil
}

func ParsePortRanges(values []string) ([]PortRange, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make([]PortRange, 0, len(values))
	for _, value := range values {
		parts := strings.Split(value, "-")
		if len(parts) > 2 || len(parts) == 0 {
			return nil, fmt.Errorf("invalid port range %q", value)
		}
		min, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 16)
		if err != nil || min == 0 {
			return nil, fmt.Errorf("invalid port range %q", value)
		}
		max := min
		if len(parts) == 2 {
			max, err = strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 16)
			if err != nil || max < min || max == 0 {
				return nil, fmt.Errorf("invalid port range %q", value)
			}
		}
		result = append(result, PortRange{Min: uint16(min), Max: uint16(max)})
	}
	return result, nil
}

func (p PortRange) Contains(port uint16) bool { return port >= p.Min && port <= p.Max }

func ReadToken(path string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) < 32 {
		return nil, errors.New("token must contain at least 32 bytes")
	}
	return b, nil
}

func TokenFingerprint(token []byte) string { return fmt.Sprintf("%x", sha256.Sum256(token))[:16] }
