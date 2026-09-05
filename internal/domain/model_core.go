package domain

import (
	"asterferry/internal/addresspolicy"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	DataALPN = "asterferry-data/2"

	CertificateActive  = "active"
	CertificateRevoked = "revoked"
	CertificateExpired = "expired"
	CertificatePending = "pending"

	ProtocolTCP = "tcp"
	ProtocolUDP = "udp"

	maxSnapshotServices    = 32768
	maxSnapshotAssignments = 32768
	maxAssignmentServices  = 4096
	maxAssignmentBindings  = 4096
)

// Node is the identity and lifecycle record for a control-plane participant.
// ID is immutable once a node has enrolled.
type Node struct {
	ID string `json:"id"`
	// SpecKind is a read-only projection used by list/detail clients. It is
	// derived from node_specs and is intentionally not accepted as Node
	// identity input.
	SpecKind          NodeSpecKind      `json:"spec_kind,omitempty"`
	Name              string            `json:"name"`
	Labels            map[string]string `json:"labels,omitempty"`
	Enabled           bool              `json:"enabled"`
	CertificateState  string            `json:"certificate_state"`
	CertificateSerial string            `json:"certificate_serial,omitempty"`
	Revision          int64             `json:"revision"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

func (n Node) Validate() error {
	if err := ValidateID(n.ID, "node.id"); err != nil {
		return err
	}
	if name := strings.TrimSpace(n.Name); name == "" || len(name) > 128 || containsControl(name) {
		return &ApplyError{Code: "invalid_name", Path: "node.name", Message: "name must contain 1 to 128 printable characters"}
	}
	if n.CertificateState != "" && n.CertificateState != CertificatePending && n.CertificateState != CertificateActive && n.CertificateState != CertificateRevoked && n.CertificateState != CertificateExpired {
		return &ApplyError{Code: "invalid_certificate_state", Path: "node.certificate_state", Message: "certificate state is invalid"}
	}
	if n.CertificateState == CertificateActive && strings.TrimSpace(n.CertificateSerial) == "" {
		return &ApplyError{Code: "invalid_certificate_serial", Path: "node.certificate_serial", Message: "an active node must have a certificate serial"}
	}
	if len(n.CertificateSerial) > 256 || containsControl(n.CertificateSerial) {
		return &ApplyError{Code: "invalid_certificate_serial", Path: "node.certificate_serial", Message: "certificate serial is invalid"}
	}
	if n.Revision < 0 {
		return &ApplyError{Code: "invalid_revision", Path: "node.revision", Message: "revision cannot be negative"}
	}
	if len(n.Labels) > 128 {
		return &ApplyError{Code: "invalid_labels", Path: "node.labels", Message: "node has too many labels"}
	}
	for key, value := range n.Labels {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key || len(key) > 128 || containsControl(key) || len(value) > 256 || containsControl(value) {
			return &ApplyError{Code: "invalid_labels", Path: "node.labels", Message: "node label is invalid"}
		}
	}
	return nil
}

// NodeSpecKind identifies the behavior configured for a Node. A node may be
// enrolled without a spec; it then remains connected and waits for the
// operator to choose a behavior. Keeping this discriminator in the spec (and
// not in Node) makes one daemon and one lifecycle sufficient for both data
// plane modes.
type NodeSpecKind string

const (
	NodeSpecGateway NodeSpecKind = "gateway"
	NodeSpecAgent   NodeSpecKind = "agent"
)

// NodeSpec is the single persisted configuration envelope for a node. The
// typed GatewaySpec and AgentSpec documents remain deliberately explicit so
// their validation and snapshot shapes do not become a weak map[string]any.
type NodeSpec struct {
	NodeID    string       `json:"node_id"`
	Kind      NodeSpecKind `json:"kind"`
	Gateway   *GatewaySpec `json:"gateway,omitempty"`
	Agent     *AgentSpec   `json:"agent,omitempty"`
	Revision  int64        `json:"revision,omitempty"`
	UpdatedAt time.Time    `json:"updated_at,omitempty"`
}

func (s NodeSpec) Validate() error {
	if err := ValidateID(s.NodeID, "node_spec.node_id"); err != nil {
		return err
	}
	if s.Kind != NodeSpecGateway && s.Kind != NodeSpecAgent {
		return &ApplyError{Code: "invalid_spec_kind", Path: "node_spec.kind", Message: "node spec kind must be gateway or agent"}
	}
	if (s.Gateway == nil) == (s.Agent == nil) {
		return &ApplyError{Code: "invalid_node_spec", Path: "node_spec", Message: "node spec must contain exactly one typed configuration"}
	}
	if s.Kind == NodeSpecGateway {
		if s.Gateway == nil {
			return &ApplyError{Code: "invalid_node_spec", Path: "node_spec.gateway", Message: "gateway configuration is required"}
		}
		if s.Gateway.NodeID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: "node_spec.gateway.node_id", Message: "gateway spec node id does not match node spec"}
		}
		if err := s.Gateway.Validate(); err != nil {
			return err
		}
	} else {
		if s.Agent == nil {
			return &ApplyError{Code: "invalid_node_spec", Path: "node_spec.agent", Message: "agent configuration is required"}
		}
		if s.Agent.NodeID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: "node_spec.agent.node_id", Message: "agent spec node id does not match node spec"}
		}
		if err := s.Agent.Validate(); err != nil {
			return err
		}
	}
	if s.Revision < 0 {
		return &ApplyError{Code: "invalid_revision", Path: "node_spec.revision", Message: "node spec revision cannot be negative"}
	}
	return nil
}

func NewGatewayNodeSpec(spec GatewaySpec) NodeSpec {
	return NodeSpec{NodeID: spec.NodeID, Kind: NodeSpecGateway, Gateway: &spec, Revision: spec.Revision}
}

func NewAgentNodeSpec(spec AgentSpec) NodeSpec {
	return NodeSpec{NodeID: spec.NodeID, Kind: NodeSpecAgent, Agent: &spec, Revision: spec.Revision}
}

func (s NodeSpec) RuntimeKind() string { return string(s.Kind) }

type Capacity struct {
	MaxAgents      int `json:"max_agents"`
	MaxConnections int `json:"max_connections"`
	MaxServices    int `json:"max_services"`
}

type PortPool struct {
	TCP []PortRange `json:"tcp,omitempty"`
	UDP []PortRange `json:"udp,omitempty"`
}

type PortRange struct {
	Min uint16 `json:"min"`
	Max uint16 `json:"max"`
}

type TransportPolicy struct {
	ALPN                    string `json:"alpn"`
	MaxStreams              int    `json:"max_streams"`
	MaxFrameBytes           int    `json:"max_frame_bytes"`
	MaxDatagramBytes        int    `json:"max_datagram_bytes"`
	HandshakeTimeoutSeconds int    `json:"handshake_timeout_seconds"`
	IdleTimeoutSeconds      int    `json:"idle_timeout_seconds"`
}

type ObfuscationPolicy struct {
	Mode string `json:"mode"`
	// KeyCiphertext and PreviousKeyCiphertext are Controller-at-rest values.
	// They are never used directly by AFDP; the Controller decrypts them before
	// constructing a node wire snapshot.
	KeyCiphertext         []byte `json:"key_ciphertext,omitempty"`
	PreviousKeyCiphertext []byte `json:"previous_key_ciphertext,omitempty"`
	KeyID                 string `json:"key_id,omitempty"`
	PreviousKeyID         string `json:"previous_key_id,omitempty"`
	// Key and PreviousKey are transient data-plane values. They are accepted
	// at the API boundary so the Controller can encrypt them immediately, and
	// are included only in the in-memory/wire snapshot after decryption.
	Key              []byte `json:"key,omitempty"`
	PreviousKey      []byte `json:"previous_key,omitempty"`
	MaxPaddingBytes  int    `json:"max_padding_bytes"`
	HandshakeShaping bool   `json:"handshake_shaping"`
}

func (o ObfuscationPolicy) Validate(path string) error {
	if path == "" {
		path = "obfuscation"
	}
	if o.Mode != "" && o.Mode != "standard" && o.Mode != "camouflage" {
		return &ApplyError{Code: "invalid_obfuscation", Path: path + ".mode", Message: "obfuscation mode must be standard or camouflage"}
	}
	if o.Mode == "camouflage" && len(o.Key) == 0 && len(o.KeyCiphertext) == 0 {
		return &ApplyError{Code: "invalid_obfuscation", Path: path + ".key_ciphertext", Message: "camouflage mode requires a protected key"}
	}
	for keyPath, key := range map[string][]byte{"key": o.Key, "previous_key": o.PreviousKey} {
		if len(key) != 0 && len(key) != 32 {
			return &ApplyError{Code: "invalid_obfuscation", Path: path + "." + keyPath, Message: "data-plane obfuscation keys must contain exactly 32 bytes"}
		}
	}
	if len(o.Key) > 0 && o.KeyID != "" && o.KeyID != ObfuscationKeyID(o.Key) {
		return &ApplyError{Code: "invalid_obfuscation", Path: path + ".key_id", Message: "key_id does not identify key"}
	}
	if len(o.PreviousKey) > 0 && o.PreviousKeyID != "" && o.PreviousKeyID != ObfuscationKeyID(o.PreviousKey) {
		return &ApplyError{Code: "invalid_obfuscation", Path: path + ".previous_key_id", Message: "previous_key_id does not identify previous_key"}
	}
	for idPath, id := range map[string]string{"key_id": o.KeyID, "previous_key_id": o.PreviousKeyID} {
		if len(id) > 128 || strings.ContainsAny(id, "\x00\r\n") {
			return &ApplyError{Code: "invalid_obfuscation", Path: path + "." + idPath, Message: "obfuscation key id is invalid"}
		}
	}
	if o.MaxPaddingBytes < 0 || o.MaxPaddingBytes > 64<<10 {
		return &ApplyError{Code: "invalid_obfuscation", Path: path + ".max_padding_bytes", Message: "obfuscation padding limit is out of range"}
	}
	if len(o.KeyCiphertext) > 128<<10 {
		return &ApplyError{Code: "invalid_obfuscation", Path: path + ".key_ciphertext", Message: "obfuscation key ciphertext is too large"}
	}
	if len(o.PreviousKeyCiphertext) > 128<<10 {
		return &ApplyError{Code: "invalid_obfuscation", Path: path + ".previous_key_ciphertext", Message: "previous obfuscation key ciphertext is too large"}
	}
	return nil
}

type Listener struct {
	Protocol string `json:"protocol"`
	Bind     string `json:"bind"`
	Port     uint16 `json:"port"`
	Enabled  bool   `json:"enabled"`
}

type GatewaySpec struct {
	NodeID          string            `json:"node_id"`
	PublicEndpoints []string          `json:"public_endpoints"`
	Listeners       []Listener        `json:"listeners,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Capacity        Capacity          `json:"capacity"`
	PortPool        PortPool          `json:"port_pool"`
	Transport       TransportPolicy   `json:"transport"`
	Obfuscation     ObfuscationPolicy `json:"obfuscation"`
	Egress          EgressPolicy      `json:"egress"`
	Revision        int64             `json:"revision,omitempty"`
}

type Selector struct {
	MatchLabels map[string]string `json:"match_labels,omitempty"`
}

func (s Selector) Matches(labels map[string]string) bool {
	for key, value := range s.MatchLabels {
		if labels == nil || labels[key] != value {
			return false
		}
	}
	return true
}

func (s Selector) Validate(path string) error {
	if len(s.MatchLabels) > 128 {
		return &ApplyError{Code: "invalid_selector", Path: path, Message: "selector contains too many labels"}
	}
	for key, value := range s.MatchLabels {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key || len(key) > 128 || containsControl(key) {
			return &ApplyError{Code: "invalid_selector", Path: path, Message: "selector label key is invalid"}
		}
		if len(value) > 256 || containsControl(value) {
			return &ApplyError{Code: "invalid_selector", Path: path, Message: "selector label value is invalid"}
		}
	}
	return nil
}

type ProxySpec struct {
	ID       string `json:"id"`
	Protocol string `json:"protocol"`
	Bind     string `json:"bind"`
	Route    string `json:"route"`
	Enabled  bool   `json:"enabled"`
}

type RouteRule struct {
	Name        string   `json:"name"`
	CIDRs       []string `json:"cidrs,omitempty"`
	Domains     []string `json:"domains,omitempty"`
	GeoIP       []string `json:"geoip,omitempty"`
	Destination string   `json:"destination"`
	Enabled     bool     `json:"enabled"`
}

type AgentLimits struct {
	MaxConnections int `json:"max_connections"`
	MaxStreams     int `json:"max_streams"`
	MaxBufferBytes int `json:"max_buffer_bytes"`
}

// EgressPolicy is the typed, node-scoped outbound policy used by Agent data
// paths. Port ranges intentionally stay textual (for example "443" or
// "8000-8080") at the API boundary so the Controller can preserve the
// operator's declarative document; validation compiles their syntax before a
// snapshot is accepted. When Enabled is false, outbound dialing is direct and
// unrestricted by this policy.
type EgressPolicy struct {
	Enabled           bool     `json:"enabled"`
	TCPPorts          []string `json:"tcp_ports,omitempty"`
	UDPPorts          []string `json:"udp_ports,omitempty"`
	AllowCIDRs        []string `json:"allow_cidrs,omitempty"`
	AllowSpecialCIDRs []string `json:"allow_special_cidrs,omitempty"`
	MaxConnections    int      `json:"max_connections"`
}

func (p EgressPolicy) Validate(path string) error {
	if strings.TrimSpace(path) == "" {
		path = "agent.egress"
	}
	if p.MaxConnections < 0 || p.MaxConnections > 1<<20 {
		return &ApplyError{Code: "invalid_egress", Path: path + ".max_connections", Message: "egress connection limit is out of range"}
	}
	if len(p.TCPPorts) > 4096 || len(p.UDPPorts) > 4096 || len(p.AllowCIDRs) > 4096 || len(p.AllowSpecialCIDRs) > 4096 {
		return &ApplyError{Code: "invalid_egress", Path: path, Message: "egress policy contains too many entries"}
	}
	if !p.Enabled {
		// Keep disabled policies cheap to carry in snapshots, but still reject
		// malformed values so enabling a later generation cannot resurrect an
		// invalid document from an old cache.
	} else if len(p.TCPPorts) == 0 && len(p.UDPPorts) == 0 {
		return &ApplyError{Code: "invalid_egress", Path: path, Message: "enabled egress requires TCP or UDP port ranges"}
	}
	if err := validateEgressPortRanges(p.TCPPorts, path+".tcp_ports"); err != nil {
		return err
	}
	if err := validateEgressPortRanges(p.UDPPorts, path+".udp_ports"); err != nil {
		return err
	}
	for _, value := range p.AllowCIDRs {
		if strings.TrimSpace(value) != value || len(value) > 64 {
			return &ApplyError{Code: "invalid_egress", Path: path + ".allow_cidrs", Message: "egress CIDR is invalid"}
		}
		if _, err := netip.ParsePrefix(value); err != nil {
			return &ApplyError{Code: "invalid_egress", Path: path + ".allow_cidrs", Message: "egress CIDR is invalid"}
		}
	}
	for _, value := range p.AllowSpecialCIDRs {
		if strings.TrimSpace(value) != value || len(value) > 64 {
			return &ApplyError{Code: "invalid_egress", Path: path + ".allow_special_cidrs", Message: "special-use egress CIDR is invalid"}
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil || !addresspolicy.IsSpecialPrefix(prefix) {
			return &ApplyError{Code: "invalid_egress", Path: path + ".allow_special_cidrs", Message: "special-use egress CIDR must be within a special-use range"}
		}
	}
	return nil
}

func validateEgressPortRanges(values []string, path string) error {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if value != trimmed || trimmed == "" || len(trimmed) > 11 {
			return &ApplyError{Code: "invalid_egress", Path: path, Message: "egress port range is invalid"}
		}
		parts := strings.Split(trimmed, "-")
		if len(parts) > 2 || parts[0] == "" {
			return &ApplyError{Code: "invalid_egress", Path: path, Message: "egress port range is invalid"}
		}
		min, err := strconv.Atoi(parts[0])
		if err != nil || min < 1 || min > 65535 {
			return &ApplyError{Code: "invalid_egress", Path: path, Message: "egress port range is invalid"}
		}
		max := min
		if len(parts) == 2 {
			max, err = strconv.Atoi(parts[1])
			if err != nil || max > 65535 {
				return &ApplyError{Code: "invalid_egress", Path: path, Message: "egress port range is invalid"}
			}
		}
		if max < min {
			return &ApplyError{Code: "invalid_egress", Path: path, Message: "egress port range is invalid"}
		}
	}
	return nil
}

type LoggingPolicy struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

type AgentSpec struct {
	NodeID string `json:"node_id"`
	// GatewayID is an optional exact placement constraint. Empty keeps legacy
	// selector-based Agent specifications working; new Controller API writes
	// require an enrolled Gateway to be selected explicitly.
	GatewayID       string        `json:"gateway_id,omitempty"`
	GatewaySelector Selector      `json:"gateway_selector"`
	Proxies         []ProxySpec   `json:"proxies,omitempty"`
	Routes          []RouteRule   `json:"routes,omitempty"`
	Limits          AgentLimits   `json:"limits"`
	Egress          EgressPolicy  `json:"egress"`
	Logging         LoggingPolicy `json:"logging"`
	Revision        int64         `json:"revision,omitempty"`
}

type Service struct {
	ID              string    `json:"id"`
	AgentID         string    `json:"agent_id"`
	Protocol        string    `json:"protocol"`
	LocalTarget     string    `json:"local_target"`
	PublicBind      string    `json:"public_bind"`
	PublicPort      uint16    `json:"public_port"`
	GatewaySelector Selector  `json:"gateway_selector"`
	Enabled         bool      `json:"enabled"`
	Revision        int64     `json:"revision"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Assignment struct {
	ID             string            `json:"id"`
	GatewayID      string            `json:"gateway_id"`
	AgentID        string            `json:"agent_id"`
	ServiceIDs     []string          `json:"service_ids"`
	Bindings       []Binding         `json:"bindings,omitempty"`
	Generation     uint64            `json:"generation"`
	State          string            `json:"state"`
	PublicEndpoint string            `json:"public_endpoint,omitempty"`
	Obfuscation    ObfuscationPolicy `json:"obfuscation,omitempty"`
	Revision       int64             `json:"revision,omitempty"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type Binding struct {
	ServiceID string `json:"service_id"`
	Protocol  string `json:"protocol"`
	Bind      string `json:"bind"`
	Port      uint16 `json:"port"`
}

const (
	AssignmentPending  = "pending"
	AssignmentApplied  = "applied"
	AssignmentDegraded = "degraded"
	AssignmentDraining = "draining"
)

type DesiredSnapshot struct {
	SchemaVersion uint32       `json:"schema_version"`
	NodeID        string       `json:"node_id"`
	Generation    uint64       `json:"generation"`
	Checksum      string       `json:"checksum"`
	Gateway       *GatewaySpec `json:"gateway,omitempty"`
	Agent         *AgentSpec   `json:"agent,omitempty"`
	Services      []Service    `json:"services,omitempty"`
	Assignments   []Assignment `json:"assignments,omitempty"`
}

func cloneLabels(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	clone := make(map[string]string, len(value))
	for key, item := range value {
		clone[key] = item
	}
	return clone
}

func cloneEgress(value EgressPolicy) EgressPolicy {
	value.TCPPorts = append([]string(nil), value.TCPPorts...)
	value.UDPPorts = append([]string(nil), value.UDPPorts...)
	value.AllowCIDRs = append([]string(nil), value.AllowCIDRs...)
	value.AllowSpecialCIDRs = append([]string(nil), value.AllowSpecialCIDRs...)
	return value
}

type SessionSummary struct {
	ID        string    `json:"id"`
	PeerID    string    `json:"peer_id"`
	StartedAt time.Time `json:"started_at"`
	Streams   int       `json:"streams"`
}

type ListenerState struct {
	Protocol string `json:"protocol"`
	Bind     string `json:"bind"`
	Port     uint16 `json:"port"`
	Ready    bool   `json:"ready"`
}

type ObservedState struct {
	SchemaVersion     uint32           `json:"schema_version"`
	NodeID            string           `json:"node_id"`
	AppliedGeneration uint64           `json:"applied_generation"`
	Healthy           bool             `json:"healthy"`
	Degraded          bool             `json:"degraded"`
	LastError         *ApplyError      `json:"last_error,omitempty"`
	Sessions          []SessionSummary `json:"sessions,omitempty"`
	Listeners         []ListenerState  `json:"listeners,omitempty"`
	Metrics           RuntimeMetrics   `json:"metrics,omitempty"`
	ObservedAt        time.Time        `json:"observed_at"`
}
