package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"asterferry/internal/addresspolicy"
	"asterferry/internal/cluster"
	"asterferry/internal/protocol"
)

const (
	ConfigVersion = protocol.Version

	RoleGateway = "gateway"
	RoleAgent   = "agent"

	RouteDirect  = "direct"
	RouteGateway = "gateway"

	ProfileStandard = "standard"
	ProfileBalanced = "balanced"

	relayRecordHeaderBytes = 12
)

// Config is a strict role-specific document. The selected role must be the
// only role section present; this keeps accepted configuration and runtime
// behavior aligned.
type Config struct {
	Version     int               `yaml:"version"`
	Role        string            `yaml:"role"`
	Transport   TransportConfig   `yaml:"transport"`
	Management  ManagementConfig  `yaml:"management"`
	Shutdown    ShutdownConfig    `yaml:"shutdown"`
	Cluster     ClusterConfig     `yaml:"cluster"`
	Limits      Limits            `yaml:"limits"`
	Obfuscation ObfuscationConfig `yaml:"obfuscation"`
	Logging     LoggingConfig     `yaml:"logging"`
	Gateway     *GatewayConfig    `yaml:"gateway"`
	Agent       *AgentConfig      `yaml:"agent"`
}

var ErrLegacyField = errors.New("legacy configuration field")

// LegacyFieldError identifies a configuration field that must be migrated
// before the strict runtime loader can accept the document.
type LegacyFieldError struct {
	Path string
}

func (e *LegacyFieldError) Error() string {
	if e == nil || e.Path == "" {
		return ErrLegacyField.Error()
	}
	return fmt.Sprintf("%s: %s", ErrLegacyField, e.Path)
}

func (e *LegacyFieldError) Unwrap() error { return ErrLegacyField }

type LoggingConfig struct {
	Level               string         `yaml:"level"`
	Format              string         `yaml:"format"`
	Sampling            SamplingConfig `yaml:"sampling"`
	ExposeDomainAtDebug bool           `yaml:"expose_domain_at_debug"`
}

type SamplingConfig struct {
	Enabled            *bool `yaml:"enabled"`
	RatePerSecond      int64 `yaml:"rate_per_second"`
	Burst              int64 `yaml:"burst"`
	SummaryIntervalSec int64 `yaml:"summary_interval_seconds"`
	MaxKeys            int64 `yaml:"max_keys"`
}

type TransportConfig struct {
	ALPN                 string `yaml:"alpn"`
	MaxBidiRemoteStreams int64  `yaml:"max_bidi_remote_streams"`
	// Initial receive windows control how much data can be in flight before
	// quic-go's autotuner grows the window. Zero keeps quic-go's default.
	InitialStreamReceiveWindow     int64 `yaml:"initial_stream_receive_window_bytes"`
	InitialConnectionReceiveWindow int64 `yaml:"initial_connection_receive_window_bytes"`
	// These names are retained for configuration continuity. They are QUIC
	// receive-window caps, rather than operating-system socket buffers.
	MaxStreamReadBuffer int64 `yaml:"max_stream_read_buffer"`
	// Deprecated: quic-go controls send-side flow control internally. A
	// non-zero value is rejected during validation instead of ignored.
	MaxStreamWriteBuffer int64 `yaml:"max_stream_write_buffer"`
	MaxConnReadBuffer    int64 `yaml:"max_conn_read_buffer"`
	// Optional overrides for the UDP socket buffers. quic-go already applies
	// a platform-specific default; these are useful for high-BDP links.
	UDPReadBufferBytes  int64 `yaml:"udp_read_buffer_bytes"`
	UDPWriteBufferBytes int64 `yaml:"udp_write_buffer_bytes"`
	HandshakeTimeoutSec int64 `yaml:"handshake_timeout_seconds"`
	IdleTimeoutSec      int64 `yaml:"idle_timeout_seconds"`
	KeepAliveSec        int64 `yaml:"keep_alive_seconds"`
}

type ManagementConfig struct {
	Listen string               `yaml:"listen"`
	Auth   ManagementAuthConfig `yaml:"auth"`
	Web    ManagementWebConfig  `yaml:"web"`
	TLS    ManagementTLSConfig  `yaml:"tls"`
}

// ManagementAuthConfig separates read-only dashboard access from operations
// that can change runtime state or configuration. Viewer and admin files may
// explicitly point at the same file when a deployment intentionally uses
// one credential, but init always generates separate credentials.
type ManagementAuthConfig struct {
	AdminTokenFile  string `yaml:"admin_token_file"`
	ViewerTokenFile string `yaml:"viewer_token_file"`
}

// ManagementWebConfig controls the optional embedded Dashboard. A pointer is
// used so old configurations which omit the section retain the historical
// enabled-by-default behavior.
type ManagementWebConfig struct {
	Enabled *bool `yaml:"enabled"`
}

// ManagementTLSConfig configures TLS for the management listener. The CA is
// used by local CLI clients when they need to verify a private management
// certificate; it is not a client-authentication CA.
type ManagementTLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	CAFile   string `yaml:"ca_file"`
}

// ShutdownConfig bounds the graceful drain performed after SIGTERM/SIGINT.
// Zero is normalized to the safe 30-second default during validation.
type ShutdownConfig struct {
	GracePeriodSec int64 `yaml:"grace_period_seconds"`
}

// ClusterConfig is identity metadata for future Gateway coordination. It is
// intentionally not an enable switch: setting node_id does not change the
// single-node data-plane behavior or add an external dependency.
type ClusterConfig struct {
	NodeID string `yaml:"node_id"`
}

type Limits struct {
	MaxAgents              int64 `yaml:"max_agents"`
	MaxConnectionsPerAgent int64 `yaml:"max_connections_per_agent"`
	MaxStreamsPerAgent     int64 `yaml:"max_streams_per_agent"`
	MaxPendingHandshakes   int64 `yaml:"max_pending_handshakes"`
	MaxInboundConnections  int64 `yaml:"max_inbound_connections"`
	MaxFrameBytes          int64 `yaml:"max_frame_bytes"`
	MaxRecordBytes         int64 `yaml:"max_record_bytes"`
	MaxWriteBatchBytes     int64 `yaml:"max_write_batch_bytes"`
	MaxUDPBytes            int64 `yaml:"max_udp_bytes"`
	DialTimeoutSec         int64 `yaml:"dial_timeout_seconds"`
	UDPIdleTimeoutSec      int64 `yaml:"udp_idle_timeout_seconds"`
	RelayIdleTimeoutSec    int64 `yaml:"relay_idle_timeout_seconds"`
}

type ObfuscationConfig struct {
	ProxyProfile    string                     `yaml:"proxy_profile"`
	ReverseProfile  string                     `yaml:"reverse_profile"`
	MaxPaddingBytes int64                      `yaml:"max_padding_bytes"`
	Transport       TransportObfuscationConfig `yaml:"transport"`
}

// TransportObfuscationConfig controls the optional datagram layer that runs
// outside QUIC. It is deliberately separate from relay padding: relay
// padding hides application record boundaries, while this layer hides QUIC
// packet bytes and (optionally) handshake shape.
type TransportObfuscationConfig struct {
	Mode             string `yaml:"mode"`
	KeyFile          string `yaml:"key_file"`
	PreviousKeyFile  string `yaml:"previous_key_file"`
	HandshakeShaping *bool  `yaml:"handshake_shaping"`
	MinFragmentBytes int64  `yaml:"min_fragment_bytes"`
	MaxFragmentBytes int64  `yaml:"max_fragment_bytes"`
	// MaxWirePacketBytes bounds each shaped handshake fragment. QUIC data
	// datagrams preserve native coalescing and may be larger than one fragment.
	MaxWirePacketBytes int64 `yaml:"max_wire_packet_bytes"`
}

const (
	TransportObfuscationStandard   = "standard"
	TransportObfuscationCamouflage = "camouflage"
)

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
	Enabled           bool     `yaml:"enabled"`
	TCPPorts          []string `yaml:"tcp_ports"`
	UDPPorts          []string `yaml:"udp_ports"`
	AllowCIDRs        []string `yaml:"allow_cidrs"`
	AllowSpecialCIDRs []string `yaml:"allow_special_cidrs"`
	MaxConnections    int64    `yaml:"max_connections"`
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
	Sniff        SniffConfig `yaml:"sniff"`
}

type SniffConfig struct {
	Enabled       *bool `yaml:"enabled"`
	MaxBytes      int64 `yaml:"max_bytes"`
	TimeoutMillis int64 `yaml:"timeout_milliseconds"`
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
	GatewayBind string `yaml:"gateway_bind"`
}

const DefaultReverseGatewayBind = "127.0.0.1"

type PortRange struct{ Min, Max uint16 }

func Load(path string) (*Config, error) {
	cleanPath := filepath.Clean(path)
	b, err := os.ReadFile(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return LoadBytes(b, cleanPath)
}

// LoadRuntime loads a configuration and applies the supported environment
// overrides before the runtime resolves file-backed secrets.
func LoadRuntime(path string) (*Config, error) {
	c, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := ApplyEnv(c); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadBytes parses a configuration document as if it had been read from
// path. This is used by the management plane to validate a pending document
// before it is written to disk.
func LoadBytes(b []byte, path string) (*Config, error) {
	cleanPath := filepath.Clean(path)
	if legacy := findLegacyField(b); legacy != nil {
		return nil, fmt.Errorf("parse config: %w", legacy)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("parse config: multiple YAML documents are not allowed")
		}
		return nil, fmt.Errorf("parse config: trailing document: %w", err)
	}
	baseDir, err := filepath.Abs(filepath.Dir(cleanPath))
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	resolveFilePaths(&c, baseDir)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func findLegacyField(raw []byte) *LegacyFieldError {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil || len(doc.Content) != 1 {
		return nil
	}
	root := doc.Content[0]
	management := yamlMappingValue(root, "management")
	if management == nil || management.Kind != yaml.MappingNode {
		return nil
	}
	for _, field := range []string{"auth_token_file", "viewer_token_file"} {
		if yamlMappingValue(management, field) != nil {
			return &LegacyFieldError{Path: "management." + field}
		}
	}
	return nil
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// resolveFilePaths makes file references portable when a configuration is
// started from a directory other than the process working directory. Absolute
// paths retain their meaning; only deployment-material paths are rebased.
func resolveFilePaths(c *Config, baseDir string) {
	if c == nil {
		return
	}
	resolve := func(path string) string {
		path = strings.TrimSpace(path)
		if path == "" {
			return ""
		}
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Clean(filepath.Join(baseDir, path))
	}
	c.Management.Auth.AdminTokenFile = resolve(c.Management.Auth.AdminTokenFile)
	c.Management.Auth.ViewerTokenFile = resolve(c.Management.Auth.ViewerTokenFile)
	c.Management.TLS.CertFile = resolve(c.Management.TLS.CertFile)
	c.Management.TLS.KeyFile = resolve(c.Management.TLS.KeyFile)
	c.Management.TLS.CAFile = resolve(c.Management.TLS.CAFile)
	c.Obfuscation.Transport.KeyFile = resolve(c.Obfuscation.Transport.KeyFile)
	c.Obfuscation.Transport.PreviousKeyFile = resolve(c.Obfuscation.Transport.PreviousKeyFile)
	if c.Gateway != nil {
		c.Gateway.TLS.CertFile = resolve(c.Gateway.TLS.CertFile)
		c.Gateway.TLS.KeyFile = resolve(c.Gateway.TLS.KeyFile)
		c.Gateway.TLS.ClientCAFile = resolve(c.Gateway.TLS.ClientCAFile)
		for i := range c.Gateway.Agents {
			c.Gateway.Agents[i].TokenFile = resolve(c.Gateway.Agents[i].TokenFile)
		}
	}
	if c.Agent != nil {
		c.Agent.TokenFile = resolve(c.Agent.TokenFile)
		c.Agent.TLS.CAFile = resolve(c.Agent.TLS.CAFile)
		c.Agent.TLS.CertFile = resolve(c.Agent.TLS.CertFile)
		c.Agent.TLS.KeyFile = resolve(c.Agent.TLS.KeyFile)
	}
}

// ApplyEnv applies supported ASTERFERRY_* overrides after YAML loading. It is
// intentionally explicit and strict: a malformed override is a startup error
// instead of silently falling back to a potentially unsafe configuration.
func ApplyEnv(c *Config) error {
	return ApplyEnvLookup(c, os.LookupEnv)
}

func ApplyEnvLookup(c *Config, lookup func(string) (string, bool)) error {
	if c == nil {
		return errors.New("config is required")
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	setString := func(name string, dst *string) error {
		if value, ok := lookup(name); ok {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s must not be empty", name)
			}
			*dst = value
		}
		return nil
	}
	setBool := func(name string, dst *bool) error {
		value, ok := lookup(name)
		if !ok {
			return nil
		}
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%s must be true or false", name)
		}
		*dst = parsed
		return nil
	}
	setBoolPtr := func(name string, dst **bool) error {
		value, ok := lookup(name)
		if !ok {
			return nil
		}
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%s must be true or false", name)
		}
		*dst = &parsed
		return nil
	}
	setInt := func(name string, dst *int64) error {
		value, ok := lookup(name)
		if !ok {
			return nil
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return fmt.Errorf("%s must be an integer", name)
		}
		*dst = parsed
		return nil
	}
	if err := setString("ASTERFERRY_LOG_LEVEL", &c.Logging.Level); err != nil {
		return err
	}
	if err := setString("ASTERFERRY_LOG_FORMAT", &c.Logging.Format); err != nil {
		return err
	}
	if err := setBoolPtr("ASTERFERRY_LOG_SAMPLING_ENABLED", &c.Logging.Sampling.Enabled); err != nil {
		return err
	}
	if err := setInt("ASTERFERRY_LOG_SAMPLE_RATE", &c.Logging.Sampling.RatePerSecond); err != nil {
		return err
	}
	if err := setInt("ASTERFERRY_LOG_SAMPLE_BURST", &c.Logging.Sampling.Burst); err != nil {
		return err
	}
	if err := setInt("ASTERFERRY_LOG_SAMPLE_SUMMARY_INTERVAL", &c.Logging.Sampling.SummaryIntervalSec); err != nil {
		return err
	}
	if err := setInt("ASTERFERRY_LOG_SAMPLE_MAX_KEYS", &c.Logging.Sampling.MaxKeys); err != nil {
		return err
	}
	if err := setInt("ASTERFERRY_SHUTDOWN_GRACE_PERIOD", &c.Shutdown.GracePeriodSec); err != nil {
		return err
	}
	if err := setBool("ASTERFERRY_LOG_EXPOSE_DOMAIN_DEBUG", &c.Logging.ExposeDomainAtDebug); err != nil {
		return err
	}
	if c.Role == RoleAgent && c.Agent != nil {
		if err := setBoolPtr("ASTERFERRY_SNIFF_ENABLED", &c.Agent.Proxy.Sniff.Enabled); err != nil {
			return err
		}
		if err := setInt("ASTERFERRY_SNIFF_MAX_BYTES", &c.Agent.Proxy.Sniff.MaxBytes); err != nil {
			return err
		}
		if err := setInt("ASTERFERRY_SNIFF_TIMEOUT_MS", &c.Agent.Proxy.Sniff.TimeoutMillis); err != nil {
			return err
		}
	}
	return c.Validate()
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
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	c.Logging.Level = strings.ToLower(strings.TrimSpace(c.Logging.Level))
	if c.Logging.Level != "debug" && c.Logging.Level != "info" && c.Logging.Level != "warn" && c.Logging.Level != "error" {
		return errors.New("logging.level must be debug, info, warn or error")
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	c.Logging.Format = strings.ToLower(strings.TrimSpace(c.Logging.Format))
	if c.Logging.Format != "json" && c.Logging.Format != "text" {
		return errors.New("logging.format must be json or text")
	}
	if c.Logging.Sampling.Enabled == nil {
		c.Logging.Sampling.Enabled = boolPtr(true)
	}
	if c.Logging.Sampling.RatePerSecond == 0 {
		c.Logging.Sampling.RatePerSecond = 5
	}
	if c.Logging.Sampling.Burst == 0 {
		c.Logging.Sampling.Burst = 20
	}
	if c.Logging.Sampling.SummaryIntervalSec == 0 {
		c.Logging.Sampling.SummaryIntervalSec = 60
	}
	if c.Logging.Sampling.MaxKeys == 0 {
		c.Logging.Sampling.MaxKeys = 4096
	}
	if c.Logging.Sampling.RatePerSecond < 1 || c.Logging.Sampling.RatePerSecond > 10000 || c.Logging.Sampling.Burst < 1 || c.Logging.Sampling.Burst > 100000 || c.Logging.Sampling.SummaryIntervalSec < 1 || c.Logging.Sampling.SummaryIntervalSec > 86400 || c.Logging.Sampling.MaxKeys < 64 || c.Logging.Sampling.MaxKeys > 1<<20 {
		return errors.New("logging.sampling values are out of range")
	}
	if c.Management.Listen == "" {
		if c.Role == RoleGateway {
			c.Management.Listen = "127.0.0.1:9090"
		} else {
			c.Management.Listen = "127.0.0.1:9091"
		}
	}
	if err := validateEndpoint(c.Management.Listen, "management.listen"); err != nil {
		return err
	}
	adminTokenFile := strings.TrimSpace(c.Management.Auth.AdminTokenFile)
	viewerTokenFile := strings.TrimSpace(c.Management.Auth.ViewerTokenFile)
	if adminTokenFile == "" {
		return errors.New("management.auth.admin_token_file is required")
	}
	if viewerTokenFile == "" {
		return errors.New("management.auth.viewer_token_file is required")
	}
	c.Management.Auth.AdminTokenFile = filepath.Clean(adminTokenFile)
	c.Management.Auth.ViewerTokenFile = filepath.Clean(viewerTokenFile)
	if c.Management.Web.Enabled == nil {
		enabled := true
		c.Management.Web.Enabled = &enabled
	}
	c.Management.TLS.CertFile = strings.TrimSpace(c.Management.TLS.CertFile)
	c.Management.TLS.KeyFile = strings.TrimSpace(c.Management.TLS.KeyFile)
	c.Management.TLS.CAFile = strings.TrimSpace(c.Management.TLS.CAFile)
	hasCert := c.Management.TLS.CertFile != ""
	hasKey := c.Management.TLS.KeyFile != ""
	if hasCert != hasKey {
		return errors.New("management.tls.cert_file and key_file must be configured together")
	}
	if c.Management.TLS.CAFile != "" && !hasCert {
		return errors.New("management.tls.ca_file requires management.tls.cert_file and key_file")
	}
	if !isLoopbackListen(c.Management.Listen) && !hasCert {
		return errors.New("management.tls.cert_file and key_file are required for non-loopback management.listen")
	}
	if c.Shutdown.GracePeriodSec == 0 {
		c.Shutdown.GracePeriodSec = 30
	}
	if c.Shutdown.GracePeriodSec < 1 || c.Shutdown.GracePeriodSec > 3600 {
		return errors.New("shutdown.grace_period_seconds must be between 1 and 3600")
	}
	if nodeID := strings.TrimSpace(c.Cluster.NodeID); nodeID != "" {
		if err := cluster.ValidateNodeID(nodeID); err != nil {
			return errors.New("cluster.node_id is invalid")
		}
		c.Cluster.NodeID = nodeID
	}
	if c.Transport.MaxBidiRemoteStreams == 0 {
		// Reserve one bidirectional stream for the control channel while
		// retaining a default budget of 256 data streams.
		c.Transport.MaxBidiRemoteStreams = 257
	}
	if c.Transport.MaxBidiRemoteStreams < 2 || c.Transport.MaxBidiRemoteStreams > 65536 {
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
	if c.Transport.HandshakeTimeoutSec > 60 {
		return errors.New("transport.handshake_timeout_seconds must not exceed 60")
	}
	if c.Transport.InitialStreamReceiveWindow < 0 || c.Transport.InitialConnectionReceiveWindow < 0 || c.Transport.MaxStreamReadBuffer < 0 || c.Transport.MaxStreamWriteBuffer < 0 || c.Transport.MaxConnReadBuffer < 0 || c.Transport.UDPReadBufferBytes < 0 || c.Transport.UDPWriteBufferBytes < 0 {
		return errors.New("transport buffer limits cannot be negative")
	}
	if c.Transport.InitialStreamReceiveWindow > 0 && c.Transport.InitialStreamReceiveWindow < 1024 {
		return errors.New("transport.initial_stream_receive_window_bytes is too small")
	}
	if c.Transport.InitialConnectionReceiveWindow > 0 && c.Transport.InitialConnectionReceiveWindow < 1024 {
		return errors.New("transport.initial_connection_receive_window_bytes is too small")
	}
	if c.Transport.MaxStreamReadBuffer > 0 && c.Transport.MaxStreamReadBuffer < 1024 {
		return errors.New("transport.max_stream_read_buffer is too small")
	}
	if c.Transport.MaxConnReadBuffer > 0 && c.Transport.MaxConnReadBuffer < 1024 {
		return errors.New("transport.max_conn_read_buffer is too small")
	}
	if c.Transport.InitialStreamReceiveWindow > 1<<30 || c.Transport.InitialConnectionReceiveWindow > 1<<30 || c.Transport.MaxStreamReadBuffer > 1<<30 || c.Transport.MaxConnReadBuffer > 1<<30 {
		return errors.New("transport receive window is out of range")
	}
	if c.Transport.InitialStreamReceiveWindow > 0 && c.Transport.MaxStreamReadBuffer > 0 && c.Transport.InitialStreamReceiveWindow > c.Transport.MaxStreamReadBuffer {
		return errors.New("transport.initial_stream_receive_window_bytes exceeds max_stream_read_buffer")
	}
	if c.Transport.InitialConnectionReceiveWindow > 0 && c.Transport.MaxConnReadBuffer > 0 && c.Transport.InitialConnectionReceiveWindow > c.Transport.MaxConnReadBuffer {
		return errors.New("transport.initial_connection_receive_window_bytes exceeds max_conn_read_buffer")
	}
	if c.Transport.InitialStreamReceiveWindow > 0 && c.Transport.InitialConnectionReceiveWindow > 0 && c.Transport.InitialConnectionReceiveWindow < c.Transport.InitialStreamReceiveWindow {
		return errors.New("transport.initial_connection_receive_window_bytes is smaller than the stream window")
	}
	if c.Transport.MaxStreamReadBuffer > 0 && c.Transport.MaxConnReadBuffer > 0 && c.Transport.MaxConnReadBuffer < c.Transport.MaxStreamReadBuffer {
		return errors.New("transport.max_conn_read_buffer is smaller than max_stream_read_buffer")
	}
	if c.Transport.UDPReadBufferBytes > 256<<20 || c.Transport.UDPWriteBufferBytes > 256<<20 {
		return errors.New("transport UDP buffer size is out of range")
	}
	if c.Transport.UDPReadBufferBytes > 0 && c.Transport.UDPReadBufferBytes < 64<<10 {
		return errors.New("transport.udp_read_buffer_bytes must be at least 64KiB")
	}
	if c.Transport.UDPWriteBufferBytes > 0 && c.Transport.UDPWriteBufferBytes < 64<<10 {
		return errors.New("transport.udp_write_buffer_bytes must be at least 64KiB")
	}
	if c.Transport.MaxStreamWriteBuffer > 0 {
		return errors.New("transport.max_stream_write_buffer is not supported by quic-go; remove it")
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
	if c.Limits.MaxPendingHandshakes == 0 {
		c.Limits.MaxPendingHandshakes = 2 * c.Limits.MaxAgents
		if c.Limits.MaxPendingHandshakes < 32 {
			c.Limits.MaxPendingHandshakes = 32
		}
		if c.Limits.MaxPendingHandshakes > 4096 {
			c.Limits.MaxPendingHandshakes = 4096
		}
	}
	if c.Limits.MaxInboundConnections == 0 {
		c.Limits.MaxInboundConnections = 1024
	}
	if c.Limits.MaxFrameBytes == 0 {
		c.Limits.MaxFrameBytes = 16 << 20
	}
	if c.Limits.MaxRecordBytes == 0 {
		c.Limits.MaxRecordBytes = 64 << 10
	}
	if c.Limits.MaxWriteBatchBytes == 0 {
		c.Limits.MaxWriteBatchBytes = 256 << 10
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
	if c.Limits.RelayIdleTimeoutSec == 0 {
		c.Limits.RelayIdleTimeoutSec = 1800
	}
	if c.Limits.MaxAgents < 1 || c.Limits.MaxAgents > 100000 || c.Limits.MaxConnectionsPerAgent < 1 || c.Limits.MaxConnectionsPerAgent > 65536 || c.Limits.MaxStreamsPerAgent < 1 || c.Limits.MaxStreamsPerAgent > 65536 || c.Limits.MaxPendingHandshakes < 8 || c.Limits.MaxPendingHandshakes > 4096 || c.Limits.MaxInboundConnections < 1 || c.Limits.MaxInboundConnections > 65536 {
		return errors.New("limits must be positive")
	}
	if c.Limits.MaxFrameBytes < 1024 || c.Limits.MaxFrameBytes > 64<<20 {
		return errors.New("limits.max_frame_bytes must be between 1KiB and 64MiB")
	}
	if c.Limits.MaxRecordBytes < 1024 || c.Limits.MaxRecordBytes > c.Limits.MaxFrameBytes {
		return errors.New("limits.max_record_bytes is out of range")
	}
	if c.Limits.MaxWriteBatchBytes < c.Limits.MaxRecordBytes || c.Limits.MaxWriteBatchBytes > 4<<20 {
		return errors.New("limits.max_write_batch_bytes must be at least max_record_bytes and no more than 4MiB")
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
	if c.Limits.RelayIdleTimeoutSec < 60 || c.Limits.RelayIdleTimeoutSec > 86400 {
		return errors.New("limits.relay_idle_timeout_seconds must be between 60 and 86400")
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
	if c.Obfuscation.Transport.Mode == "" {
		c.Obfuscation.Transport.Mode = TransportObfuscationCamouflage
	}
	if c.Obfuscation.Transport.Mode != TransportObfuscationStandard && c.Obfuscation.Transport.Mode != TransportObfuscationCamouflage {
		return errors.New("obfuscation.transport.mode must be standard or camouflage")
	}
	if c.Obfuscation.Transport.MinFragmentBytes == 0 {
		c.Obfuscation.Transport.MinFragmentBytes = 512
	}
	if c.Obfuscation.Transport.MaxFragmentBytes == 0 {
		c.Obfuscation.Transport.MaxFragmentBytes = 1200
	}
	if c.Obfuscation.Transport.MaxWirePacketBytes == 0 {
		c.Obfuscation.Transport.MaxWirePacketBytes = 1280
	}
	if c.Obfuscation.Transport.Mode == TransportObfuscationCamouflage {
		if strings.TrimSpace(c.Obfuscation.Transport.KeyFile) == "" {
			return errors.New("obfuscation.transport.key_file is required in camouflage mode")
		}
		if c.Obfuscation.Transport.HandshakeShaping == nil {
			c.Obfuscation.Transport.HandshakeShaping = boolPtr(true)
		}
	} else {
		if c.Obfuscation.Transport.HandshakeShaping == nil {
			c.Obfuscation.Transport.HandshakeShaping = boolPtr(false)
		}
		if *c.Obfuscation.Transport.HandshakeShaping {
			return errors.New("obfuscation.transport.handshake_shaping requires camouflage mode")
		}
	}
	if c.Obfuscation.Transport.MinFragmentBytes < 256 || c.Obfuscation.Transport.MinFragmentBytes > 1200 || c.Obfuscation.Transport.MaxFragmentBytes < c.Obfuscation.Transport.MinFragmentBytes || c.Obfuscation.Transport.MaxFragmentBytes > 1200 {
		return errors.New("obfuscation.transport fragment sizes are out of range")
	}
	if c.Obfuscation.Transport.MaxWirePacketBytes < c.Obfuscation.Transport.MaxFragmentBytes+32 || c.Obfuscation.Transport.MaxWirePacketBytes > 1472 {
		return errors.New("obfuscation.transport.max_wire_packet_bytes is out of range")
	}
	if c.Role == RoleGateway {
		return c.validateGateway()
	}
	return c.validateAgent()
}

// UsableStreamLimit reserves one incoming bidirectional stream for the
// control stream. The result is shared by both roles so the application-level
// admission limit cannot exceed the peer's QUIC stream budget.
func UsableStreamLimit(transport TransportConfig, limits Limits) int64 {
	limit := limits.MaxStreamsPerAgent
	if limit <= 0 {
		return 0
	}
	quicLimit := transport.MaxBidiRemoteStreams - 1
	if quicLimit > 0 && quicLimit < limit {
		return quicLimit
	}
	return limit
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
	for index := range g.Agents {
		agent := &g.Agents[index]
		if err := validateIdentifier(agent.ID, "gateway agent id", 128); err != nil {
			return err
		}
		if seen[agent.ID] {
			return errors.New("gateway agent IDs must be unique")
		}
		seen[agent.ID] = true
		if agent.TokenFile == "" {
			return fmt.Errorf("agent %q has no token_file", agent.ID)
		}
		if err := validateACL(agent.Reverse.TCPPorts, agent.Reverse.UDPPorts, "reverse", agent.ID); err != nil {
			return err
		}
		if agent.Egress.Enabled {
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
	if err := validateIdentifier(a.ID, "agent.id", 128); err != nil {
		return err
	}
	if a.Server == "" || a.TokenFile == "" {
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
	if a.Proxy.Sniff.Enabled == nil {
		a.Proxy.Sniff.Enabled = boolPtr(true)
	}
	if a.Proxy.Sniff.MaxBytes == 0 {
		a.Proxy.Sniff.MaxBytes = 16 << 10
	}
	if a.Proxy.Sniff.TimeoutMillis == 0 {
		a.Proxy.Sniff.TimeoutMillis = 250
	}
	if a.Proxy.Sniff.MaxBytes < 1024 || a.Proxy.Sniff.MaxBytes > 1<<20 {
		return errors.New("agent.proxy.sniff.max_bytes must be between 1KiB and 1MiB")
	}
	if a.Proxy.Sniff.TimeoutMillis < 10 || a.Proxy.Sniff.TimeoutMillis > 10000 {
		return errors.New("agent.proxy.sniff.timeout_milliseconds must be between 10 and 10000")
	}
	seenTags := map[string]bool{}
	for _, in := range a.Proxy.Inbounds {
		if err := validateIdentifier(in.Tag, "proxy inbound tag", 128); err != nil {
			return err
		}
		if seenTags[in.Tag] {
			return errors.New("proxy inbound tags must be unique")
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
	for i := range a.Reverse {
		t := &a.Reverse[i]
		if err := validateIdentifier(t.Name, "reverse mapping name", 128); err != nil {
			return err
		}
		if seenNames[t.Name] {
			return errors.New("reverse mapping names must be unique")
		}
		seenNames[t.Name] = true
		if t.Protocol != "tcp" && t.Protocol != "udp" {
			return fmt.Errorf("mapping %q protocol must be tcp or udp", t.Name)
		}
		if t.GatewayPort == 0 {
			return fmt.Errorf("mapping %q gateway_port must be non-zero", t.Name)
		}
		if strings.TrimSpace(t.GatewayBind) == "" {
			t.GatewayBind = DefaultReverseGatewayBind
		} else {
			bind, err := netip.ParseAddr(strings.TrimSpace(t.GatewayBind))
			if err != nil || strings.Contains(t.GatewayBind, "%") {
				return fmt.Errorf("mapping %q gateway_bind must be an IP address", t.Name)
			}
			t.GatewayBind = bind.String()
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
	for _, cidr := range p.AllowSpecialCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return fmt.Errorf("agent %q egress allow_special_cidr: %w", id, err)
		}
		if !addresspolicy.IsSpecialPrefix(prefix) {
			return fmt.Errorf("agent %q egress allow_special_cidr must be within a special-use range", id)
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

func validateIdentifier(value, field string, max int) error {
	if value == "" || len(value) > max {
		return fmt.Errorf("%s must be between 1 and %d bytes", field, max)
	}
	if !isASCIIAlphaNumeric(value[0]) {
		return fmt.Errorf("%s must start with an ASCII letter or digit", field)
	}
	for i := 1; i < len(value); i++ {
		if !isASCIIAlphaNumeric(value[i]) && value[i] != '-' && value[i] != '_' && value[i] != '.' {
			return fmt.Errorf("%s contains an invalid character", field)
		}
	}
	return nil
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
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
	b, err := readSecretFile(path, "token")
	if err != nil {
		return nil, err
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) < 32 {
		return nil, errors.New("token must contain at least 32 bytes")
	}
	return b, nil
}

// ReadSecret reads a deployment secret from a text file. A single trailing
// newline is accepted so secrets generated by command-line tools remain easy
// to provision, while the bounded size prevents accidental key-file misuse.
func ReadSecret(path string) ([]byte, error) {
	b, err := readSecretFile(path, "secret")
	if err != nil {
		return nil, err
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) < 32 {
		return nil, errors.New("secret must contain at least 32 bytes")
	}
	if len(b) > 128 {
		return nil, errors.New("secret must not contain more than 128 bytes")
	}
	return b, nil
}

func readSecretFile(path, kind string) ([]byte, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "." || clean == "" {
		return nil, fmt.Errorf("read %s: path is required", kind)
	}
	info, err := os.Stat(clean)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", kind, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read %s: path must be a regular file", kind)
	}
	if err := ValidateSecretFilePermissions(clean, info); err != nil {
		return nil, fmt.Errorf("read %s: %w", kind, err)
	}
	b, err := os.ReadFile(clean)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", kind, err)
	}
	return b, nil
}

func TokenFingerprint(token []byte) string { return fmt.Sprintf("%x", sha256.Sum256(token))[:16] }

func boolPtr(value bool) *bool { return &value }
