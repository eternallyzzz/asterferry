// Package domain contains the versioned control-plane model.  It deliberately
// has no imports from the controller, persistence, HTTP, or data-plane
// packages so it can be shared by all of the roles without creating a control
// plane dependency in the data path.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"asterferry/internal/addresspolicy"
)

const (
	SchemaVersion = 1
	DataALPN      = "asterferry-data/1"

	RoleGateway = "gateway"
	RoleAgent   = "agent"

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
	ID                string            `json:"id"`
	Role              string            `json:"role"`
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
	if n.Role != RoleGateway && n.Role != RoleAgent {
		return &ApplyError{Code: "invalid_role", Path: "node.role", Message: "node role must be gateway or agent"}
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

type Capacity struct {
	MaxAgents       int `json:"max_agents"`
	MaxConnections  int `json:"max_connections"`
	MaxServices     int `json:"max_services"`
	UsedAgents      int `json:"used_agents"`
	UsedConnections int `json:"used_connections"`
	UsedServices    int `json:"used_services"`
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
	Mode             string `json:"mode"`
	KeyCiphertext    []byte `json:"key_ciphertext,omitempty"`
	MaxPaddingBytes  int    `json:"max_padding_bytes"`
	HandshakeShaping bool   `json:"handshake_shaping"`
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
			if err != nil || max < min || max > 65535 {
				return &ApplyError{Code: "invalid_egress", Path: path, Message: "egress port range is invalid"}
			}
		}
		_ = max
	}
	return nil
}

type LoggingPolicy struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

type AgentSpec struct {
	NodeID          string        `json:"node_id"`
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
	ID             string    `json:"id"`
	GatewayID      string    `json:"gateway_id"`
	AgentID        string    `json:"agent_id"`
	ServiceIDs     []string  `json:"service_ids"`
	Bindings       []Binding `json:"bindings,omitempty"`
	Generation     uint64    `json:"generation"`
	State          string    `json:"state"`
	PublicEndpoint string    `json:"public_endpoint,omitempty"`
	Revision       int64     `json:"revision,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
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
	SchemaVersion     uint32             `json:"schema_version"`
	NodeID            string             `json:"node_id"`
	AppliedGeneration uint64             `json:"applied_generation"`
	Healthy           bool               `json:"healthy"`
	Degraded          bool               `json:"degraded"`
	LastError         *ApplyError        `json:"last_error,omitempty"`
	Sessions          []SessionSummary   `json:"sessions,omitempty"`
	Listeners         []ListenerState    `json:"listeners,omitempty"`
	Metrics           map[string]float64 `json:"metrics,omitempty"`
	ObservedAt        time.Time          `json:"observed_at"`
}

func (o ObservedState) Validate() error {
	if o.SchemaVersion != SchemaVersion {
		return &ApplyError{Code: "unknown_schema", Path: "schema_version", Message: "observed schema version is unsupported"}
	}
	if err := ValidateID(o.NodeID, "node_id"); err != nil {
		return err
	}
	if o.Healthy && o.Degraded {
		return &ApplyError{Code: "invalid_observed_state", Path: "healthy", Message: "observed state cannot be healthy and degraded at the same time"}
	}
	if o.LastError != nil {
		if strings.TrimSpace(o.LastError.Code) == "" || len(o.LastError.Code) > 128 || len(o.LastError.Message) > 2048 {
			return &ApplyError{Code: "invalid_observed_error", Path: "last_error", Message: "observed error fields are invalid"}
		}
	}
	if len(o.Sessions) > 4096 || len(o.Listeners) > 4096 || len(o.Metrics) > 4096 {
		return &ApplyError{Code: "observed_state_too_large", Message: "observed state contains too many entries"}
	}
	seenSessions := make(map[string]struct{}, len(o.Sessions))
	for i, session := range o.Sessions {
		if err := ValidateID(session.ID, fmt.Sprintf("sessions[%d].id", i)); err != nil {
			return err
		}
		if _, exists := seenSessions[session.ID]; exists {
			return &ApplyError{Code: "duplicate_session", Path: fmt.Sprintf("sessions[%d].id", i), Message: "session id is duplicated"}
		}
		seenSessions[session.ID] = struct{}{}
		if session.PeerID != "" {
			if err := ValidateID(session.PeerID, fmt.Sprintf("sessions[%d].peer_id", i)); err != nil {
				return err
			}
		}
		if session.Streams < 0 || session.Streams > 1<<20 {
			return &ApplyError{Code: "invalid_session", Path: fmt.Sprintf("sessions[%d].streams", i), Message: "session stream count is invalid"}
		}
	}
	seenListeners := make(map[string]struct{}, len(o.Listeners))
	for i, listener := range o.Listeners {
		if err := validateListenerState(listener); err != nil {
			return &ApplyError{Code: "invalid_listener_state", Path: fmt.Sprintf("listeners[%d]", i), Message: err.Error()}
		}
		key := fmt.Sprintf("%s|%s|%d", listener.Protocol, normalizedIP(listener.Bind), listener.Port)
		if _, exists := seenListeners[key]; exists {
			return &ApplyError{Code: "duplicate_listener_state", Path: fmt.Sprintf("listeners[%d]", i), Message: "listener state is duplicated"}
		}
		seenListeners[key] = struct{}{}
	}
	for key, value := range o.Metrics {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key || len(key) > 128 || containsControl(key) || math.IsNaN(value) || math.IsInf(value, 0) {
			return &ApplyError{Code: "invalid_metrics", Path: "metrics", Message: "observed metric is invalid"}
		}
	}
	return nil
}

func validateListenerState(listener ListenerState) error {
	if listener.Protocol != ProtocolTCP && listener.Protocol != ProtocolUDP {
		return errors.New("listener protocol must be tcp or udp")
	}
	if listener.Port == 0 {
		return errors.New("listener port must be non-zero")
	}
	if listener.Bind != strings.TrimSpace(listener.Bind) {
		return errors.New("listener bind must not contain surrounding whitespace")
	}
	if _, err := netip.ParseAddr(listener.Bind); err != nil {
		return errors.New("listener bind must be an IP address")
	}
	return nil
}

type ApplyError struct {
	Code      string `json:"code"`
	Path      string `json:"path,omitempty"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *ApplyError) Error() string {
	if e == nil {
		return ""
	}
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
}

// Validate performs structural validation and returns stable error fields
// suitable for both the REST API and the control protocol.
func (s DesiredSnapshot) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return &ApplyError{Code: "unknown_schema", Path: "schema_version", Message: fmt.Sprintf("schema version %d is unsupported", s.SchemaVersion)}
	}
	if err := ValidateID(s.NodeID, "node_id"); err != nil {
		return err
	}
	if s.Generation == 0 {
		return &ApplyError{Code: "invalid_generation", Path: "generation", Message: "generation must be positive"}
	}
	if len(s.Services) > maxSnapshotServices || len(s.Assignments) > maxSnapshotAssignments {
		return &ApplyError{Code: "snapshot_too_large", Message: "snapshot contains too many resources"}
	}
	if (s.Gateway == nil) == (s.Agent == nil) {
		return &ApplyError{Code: "invalid_role_spec", Message: "snapshot must contain exactly one gateway or agent spec"}
	}
	if s.Gateway != nil {
		if s.Gateway.NodeID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: "gateway.node_id", Message: "gateway node id does not match snapshot"}
		}
		if err := s.Gateway.Validate(); err != nil {
			return err
		}
	}
	if s.Agent != nil {
		if s.Agent.NodeID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: "agent.node_id", Message: "agent node id does not match snapshot"}
		}
		if err := s.Agent.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(s.Services))
	serviceAgents := make(map[string]string, len(s.Services))
	serviceProtocols := make(map[string]string, len(s.Services))
	serviceBinds := make(map[string]string, len(s.Services))
	servicePorts := make(map[string]uint16, len(s.Services))
	for i := range s.Services {
		if err := s.Services[i].Validate(); err != nil {
			return &ApplyError{Code: "invalid_service", Path: fmt.Sprintf("services[%d]", i), Message: err.Error()}
		}
		if _, ok := seen[s.Services[i].ID]; ok {
			return &ApplyError{Code: "duplicate_service", Path: fmt.Sprintf("services[%d].id", i), Message: "service id is duplicated"}
		}
		seen[s.Services[i].ID] = struct{}{}
		serviceAgents[s.Services[i].ID] = s.Services[i].AgentID
		serviceProtocols[s.Services[i].ID] = s.Services[i].Protocol
		serviceBinds[s.Services[i].ID] = normalizedIP(s.Services[i].PublicBind)
		servicePorts[s.Services[i].ID] = s.Services[i].PublicPort
		if s.Agent != nil && s.Services[i].AgentID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: fmt.Sprintf("services[%d].agent_id", i), Message: "agent snapshot may only contain services owned by its node"}
		}
	}
	assignmentIDs := make(map[string]struct{}, len(s.Assignments))
	assignedServices := make(map[string]string, len(s.Services))
	assignedBindings := make(map[string]string)
	// Gateway listeners occupy the same public bind namespace as assignment
	// bindings. Seed the set before checking assignments so a service cannot
	// claim a port already reserved by a listener in the same generation.
	if s.Gateway != nil {
		for _, listener := range s.Gateway.Listeners {
			bind := normalizedIP(listener.Bind)
			assignedBindings[fmt.Sprintf("%s|%s|%d", listener.Protocol, bind, listener.Port)] = "listener"
		}
	}
	for i := range s.Assignments {
		if err := s.Assignments[i].Validate(); err != nil {
			return &ApplyError{Code: "invalid_assignment", Path: fmt.Sprintf("assignments[%d]", i), Message: err.Error()}
		}
		assignment := s.Assignments[i]
		if _, exists := assignmentIDs[assignment.ID]; exists {
			return &ApplyError{Code: "duplicate_assignment", Path: fmt.Sprintf("assignments[%d].id", i), Message: "assignment id is duplicated"}
		}
		assignmentIDs[assignment.ID] = struct{}{}
		// Assignment generations are shared by both ends of a data-plane
		// session, while snapshot generations are node-local. A Gateway-only
		// spec change may advance its snapshot without changing the assignment;
		// reject only an assignment that is newer than the snapshot carrying it.
		if assignment.Generation > s.Generation {
			return &ApplyError{Code: "generation_mismatch", Path: fmt.Sprintf("assignments[%d].generation", i), Message: "assignment generation cannot be newer than snapshot generation"}
		}
		if s.Gateway != nil && assignment.GatewayID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: fmt.Sprintf("assignments[%d].gateway_id", i), Message: "gateway snapshot contains an assignment for another gateway"}
		}
		if s.Agent != nil && assignment.AgentID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: fmt.Sprintf("assignments[%d].agent_id", i), Message: "agent snapshot contains an assignment for another agent"}
		}
		assignmentServices := make(map[string]struct{}, len(assignment.ServiceIDs))
		for _, serviceID := range assignment.ServiceIDs {
			serviceAgent, ok := serviceAgents[serviceID]
			if !ok {
				return &ApplyError{Code: "unknown_service", Path: fmt.Sprintf("assignments[%d].service_ids", i), Message: fmt.Sprintf("service %q is not present in snapshot", serviceID)}
			}
			if serviceAgent != assignment.AgentID {
				return &ApplyError{Code: "service_agent_mismatch", Path: fmt.Sprintf("assignments[%d].service_ids", i), Message: fmt.Sprintf("service %q belongs to another agent", serviceID)}
			}
			assignmentServices[serviceID] = struct{}{}
			if previous, exists := assignedServices[serviceID]; exists && previous != assignment.ID {
				return &ApplyError{Code: "resource_conflict", Path: fmt.Sprintf("assignments[%d].service_ids", i), Message: fmt.Sprintf("service %q is assigned more than once", serviceID)}
			}
			assignedServices[serviceID] = assignment.ID
		}
		for _, binding := range assignment.Bindings {
			if _, ok := assignmentServices[binding.ServiceID]; !ok {
				return &ApplyError{Code: "unknown_service", Path: fmt.Sprintf("assignments[%d].bindings", i), Message: fmt.Sprintf("binding references service %q outside service_ids", binding.ServiceID)}
			}
			if serviceProtocols[binding.ServiceID] != binding.Protocol {
				return &ApplyError{Code: "protocol_mismatch", Path: fmt.Sprintf("assignments[%d].bindings", i), Message: fmt.Sprintf("binding protocol for service %q does not match the service", binding.ServiceID)}
			}
			if serviceBinds[binding.ServiceID] != normalizedIP(binding.Bind) {
				return &ApplyError{Code: "bind_mismatch", Path: fmt.Sprintf("assignments[%d].bindings", i), Message: fmt.Sprintf("binding address for service %q does not match the service public bind", binding.ServiceID)}
			}
			if requestedPort := servicePorts[binding.ServiceID]; requestedPort != 0 && requestedPort != binding.Port {
				return &ApplyError{Code: "port_mismatch", Path: fmt.Sprintf("assignments[%d].bindings", i), Message: fmt.Sprintf("binding port for service %q does not match the service public port", binding.ServiceID)}
			}
			bind := strings.TrimSpace(binding.Bind)
			if address, parseErr := netip.ParseAddr(bind); parseErr == nil {
				bind = address.Unmap().String()
			}
			key := fmt.Sprintf("%s|%s|%d", binding.Protocol, bind, binding.Port)
			if previous, exists := assignedBindings[key]; exists && previous != assignment.ID {
				return &ApplyError{Code: "resource_conflict", Path: fmt.Sprintf("assignments[%d].bindings", i), Message: "public binding is duplicated across assignments"}
			}
			assignedBindings[key] = assignment.ID
		}
	}
	if s.Checksum != "" {
		checksum, err := s.ComputeChecksum()
		if err != nil {
			return &ApplyError{Code: "checksum_error", Message: err.Error()}
		}
		if !strings.EqualFold(checksum, s.Checksum) {
			return &ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "snapshot checksum does not match its content"}
		}
	}
	return nil
}

func (g GatewaySpec) Validate() error {
	if err := ValidateID(g.NodeID, "gateway.node_id"); err != nil {
		return err
	}
	if len(g.PublicEndpoints) == 0 {
		return &ApplyError{Code: "missing_endpoint", Path: "gateway.public_endpoints", Message: "at least one public endpoint is required"}
	}
	for i, endpoint := range g.PublicEndpoints {
		if err := validateEndpoint(endpoint); err != nil {
			return &ApplyError{Code: "invalid_endpoint", Path: fmt.Sprintf("gateway.public_endpoints[%d]", i), Message: err.Error()}
		}
	}
	if len(g.PublicEndpoints) > 64 {
		return &ApplyError{Code: "too_many_endpoints", Path: "gateway.public_endpoints", Message: "gateway has too many public endpoints"}
	}
	seenEndpoints := make(map[string]struct{}, len(g.PublicEndpoints))
	for _, endpoint := range g.PublicEndpoints {
		endpoint = strings.TrimSpace(endpoint)
		if _, exists := seenEndpoints[endpoint]; exists {
			return &ApplyError{Code: "duplicate_endpoint", Path: "gateway.public_endpoints", Message: "gateway public endpoint is duplicated"}
		}
		seenEndpoints[endpoint] = struct{}{}
	}
	if err := validateLabels(g.Labels, "gateway.labels"); err != nil {
		return err
	}
	if g.Revision < 0 {
		return &ApplyError{Code: "invalid_revision", Path: "gateway.revision", Message: "revision cannot be negative"}
	}
	if len(g.PortPool.TCP) > 1024 || len(g.PortPool.UDP) > 1024 {
		return &ApplyError{Code: "invalid_port_pool", Path: "gateway.port_pool", Message: "gateway has too many port ranges"}
	}
	seenListeners := make(map[string]struct{}, len(g.Listeners))
	if len(g.Listeners) > 4096 {
		return &ApplyError{Code: "too_many_listeners", Path: "gateway.listeners", Message: "gateway has too many listeners"}
	}
	for i, listener := range g.Listeners {
		if err := validateListener(listener); err != nil {
			return &ApplyError{Code: "invalid_listener", Path: fmt.Sprintf("gateway.listeners[%d]", i), Message: err.Error()}
		}
		bind := strings.TrimSpace(listener.Bind)
		if address, parseErr := netip.ParseAddr(bind); parseErr == nil {
			bind = address.Unmap().String()
		}
		key := fmt.Sprintf("%s|%s|%d", listener.Protocol, bind, listener.Port)
		if _, exists := seenListeners[key]; exists {
			return &ApplyError{Code: "duplicate_listener", Path: fmt.Sprintf("gateway.listeners[%d]", i), Message: "listener binding is duplicated"}
		}
		seenListeners[key] = struct{}{}
	}
	if g.Capacity.MaxAgents < 0 || g.Capacity.MaxServices < 0 || g.Capacity.MaxConnections < 0 || g.Capacity.UsedAgents < 0 || g.Capacity.UsedServices < 0 || g.Capacity.UsedConnections < 0 {
		return &ApplyError{Code: "invalid_capacity", Path: "gateway.capacity", Message: "capacity values cannot be negative"}
	}
	if g.Capacity.MaxAgents > 1<<20 || g.Capacity.MaxServices > 1<<20 || g.Capacity.MaxConnections > 1<<20 || g.Capacity.UsedAgents > 1<<20 || g.Capacity.UsedServices > 1<<20 || g.Capacity.UsedConnections > 1<<20 {
		return &ApplyError{Code: "invalid_capacity", Path: "gateway.capacity", Message: "capacity values exceed the supported maximum"}
	}
	if err := g.PortPool.Validate(); err != nil {
		return err
	}
	if g.Transport.MaxStreams < 0 || g.Transport.MaxFrameBytes < 0 || g.Transport.MaxDatagramBytes < 0 || g.Transport.HandshakeTimeoutSeconds < 0 || g.Transport.IdleTimeoutSeconds < 0 {
		return &ApplyError{Code: "invalid_transport", Path: "gateway.transport", Message: "transport limits cannot be negative"}
	}
	if g.Transport.MaxStreams > 1<<20 || g.Transport.MaxFrameBytes > 64<<10 || g.Transport.MaxDatagramBytes > 64<<10 || g.Transport.HandshakeTimeoutSeconds > 24*60*60 || g.Transport.IdleTimeoutSeconds > 7*24*60*60 {
		return &ApplyError{Code: "invalid_transport", Path: "gateway.transport", Message: "transport limits exceed the supported maximum"}
	}
	if g.Transport.ALPN != "" && g.Transport.ALPN != DataALPN || len(g.Transport.ALPN) > 255 || containsControl(g.Transport.ALPN) {
		return &ApplyError{Code: "invalid_transport", Path: "gateway.transport.alpn", Message: "transport ALPN is invalid"}
	}
	if g.Obfuscation.Mode != "" && g.Obfuscation.Mode != "standard" && g.Obfuscation.Mode != "camouflage" {
		return &ApplyError{Code: "invalid_obfuscation", Path: "gateway.obfuscation.mode", Message: "obfuscation mode must be standard or camouflage"}
	}
	if g.Obfuscation.Mode == "camouflage" && len(g.Obfuscation.KeyCiphertext) == 0 {
		return &ApplyError{Code: "invalid_obfuscation", Path: "gateway.obfuscation.key_ciphertext", Message: "camouflage mode requires an encrypted key"}
	}
	if g.Obfuscation.MaxPaddingBytes < 0 || g.Obfuscation.MaxPaddingBytes > 64<<10 {
		return &ApplyError{Code: "invalid_obfuscation", Path: "gateway.obfuscation.max_padding_bytes", Message: "obfuscation padding limit is out of range"}
	}
	if len(g.Obfuscation.KeyCiphertext) > 128<<10 {
		return &ApplyError{Code: "invalid_obfuscation", Path: "gateway.obfuscation.key_ciphertext", Message: "obfuscation key ciphertext is too large"}
	}
	if err := g.Egress.Validate("gateway.egress"); err != nil {
		return err
	}
	return nil
}

func (a AgentSpec) Validate() error {
	if err := ValidateID(a.NodeID, "agent.node_id"); err != nil {
		return err
	}
	if a.Limits.MaxConnections < 0 || a.Limits.MaxStreams < 0 || a.Limits.MaxBufferBytes < 0 {
		return &ApplyError{Code: "invalid_limits", Path: "agent.limits", Message: "limit values cannot be negative"}
	}
	if a.Limits.MaxConnections > 1<<20 || a.Limits.MaxStreams > 1<<20 || a.Limits.MaxBufferBytes > 64<<20 {
		return &ApplyError{Code: "invalid_limits", Path: "agent.limits", Message: "agent limits exceed the supported maximum"}
	}
	if err := a.Egress.Validate("agent.egress"); err != nil {
		return err
	}
	if a.Revision < 0 {
		return &ApplyError{Code: "invalid_revision", Path: "agent.revision", Message: "revision cannot be negative"}
	}
	if err := a.GatewaySelector.Validate("agent.gateway_selector"); err != nil {
		return err
	}
	if len(a.Proxies) > 1024 || len(a.Routes) > 4096 {
		return &ApplyError{Code: "agent_spec_too_large", Path: "agent", Message: "agent spec contains too many entries"}
	}
	seenProxies := make(map[string]struct{}, len(a.Proxies))
	seenProxyBinds := make(map[string]struct{}, len(a.Proxies))
	for i, proxy := range a.Proxies {
		if err := ValidateID(proxy.ID, fmt.Sprintf("agent.proxies[%d].id", i)); err != nil || (proxy.Protocol != "http" && proxy.Protocol != "socks5") {
			return &ApplyError{Code: "invalid_proxy", Path: fmt.Sprintf("agent.proxies[%d]", i), Message: "proxy id and protocol are invalid"}
		}
		if _, exists := seenProxies[proxy.ID]; exists {
			return &ApplyError{Code: "duplicate_proxy", Path: fmt.Sprintf("agent.proxies[%d].id", i), Message: "proxy id is duplicated"}
		}
		seenProxies[proxy.ID] = struct{}{}
		if proxy.Bind == "" {
			return &ApplyError{Code: "invalid_proxy", Path: fmt.Sprintf("agent.proxies[%d].bind", i), Message: "proxy bind is required"}
		}
		if err := validateHostPort(proxy.Bind); err != nil {
			return &ApplyError{Code: "invalid_proxy", Path: fmt.Sprintf("agent.proxies[%d].bind", i), Message: "proxy bind must be host:port"}
		}
		// HTTP and SOCKS5 entrances cannot share one TCP socket even though
		// their protocol labels differ. Reserve the physical bind address as
		// the uniqueness key so the node can atomically recreate listeners.
		proxyBindKey := proxy.Bind
		if _, exists := seenProxyBinds[proxyBindKey]; exists {
			return &ApplyError{Code: "duplicate_proxy_bind", Path: fmt.Sprintf("agent.proxies[%d].bind", i), Message: "proxy bind is duplicated"}
		}
		seenProxyBinds[proxyBindKey] = struct{}{}
		if proxy.Route != "" && proxy.Route != "direct" && proxy.Route != "gateway" {
			return &ApplyError{Code: "invalid_proxy", Path: fmt.Sprintf("agent.proxies[%d].route", i), Message: "proxy route must be direct or gateway"}
		}
	}
	seenRoutes := make(map[string]struct{}, len(a.Routes))
	for i, route := range a.Routes {
		if err := ValidateID(route.Name, fmt.Sprintf("agent.routes[%d].name", i)); err != nil {
			return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].name", i), Message: err.Error()}
		}
		if _, exists := seenRoutes[route.Name]; exists {
			return &ApplyError{Code: "duplicate_route", Path: fmt.Sprintf("agent.routes[%d].name", i), Message: "route name is duplicated"}
		}
		seenRoutes[route.Name] = struct{}{}
		if strings.TrimSpace(route.Destination) == "" || len(route.Destination) > 2048 || containsControl(route.Destination) {
			return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].destination", i), Message: "route destination is invalid"}
		}
		if route.Destination != "direct" && route.Destination != "gateway" {
			return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].destination", i), Message: "route destination must be direct or gateway"}
		}
		if len(route.CIDRs) > 1024 || len(route.Domains) > 1024 {
			return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d]", i), Message: "route contains too many match entries"}
		}
		for _, cidr := range route.CIDRs {
			if _, err := netip.ParsePrefix(strings.TrimSpace(cidr)); err != nil {
				return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].cidrs", i), Message: "route CIDR is invalid"}
			}
		}
		for _, domainName := range route.Domains {
			if strings.TrimSpace(domainName) == "" || strings.TrimSpace(domainName) != domainName || len(domainName) > 253 || containsControl(domainName) || strings.ContainsAny(domainName, " \t") {
				return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].domains", i), Message: "route domain is invalid"}
			}
		}
	}
	return nil
}

func validateLabels(labels map[string]string, path string) error {
	if len(labels) > 128 {
		return &ApplyError{Code: "invalid_labels", Path: path, Message: "too many labels"}
	}
	for key, value := range labels {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key || len(key) > 128 || containsControl(key) || len(value) > 256 || containsControl(value) {
			return &ApplyError{Code: "invalid_labels", Path: path, Message: "label is invalid"}
		}
	}
	return nil
}

func (s Service) Validate() error {
	if err := ValidateID(s.ID, "service.id"); err != nil {
		return err
	}
	if err := ValidateID(s.AgentID, "service.agent_id"); err != nil {
		return err
	}
	if s.Revision < 0 {
		return &ApplyError{Code: "invalid_revision", Path: "service.revision", Message: "revision cannot be negative"}
	}
	if s.Protocol != ProtocolTCP && s.Protocol != ProtocolUDP {
		return errors.New("service protocol must be tcp or udp")
	}
	if err := s.GatewaySelector.Validate("service.gateway_selector"); err != nil {
		return err
	}
	if strings.TrimSpace(s.LocalTarget) == "" {
		return errors.New("service local_target is required")
	}
	if len(s.LocalTarget) > 2048 || containsControl(s.LocalTarget) {
		return errors.New("service local_target is invalid")
	}
	if strings.TrimSpace(s.PublicBind) == "" {
		return errors.New("service public_bind is required")
	}
	if s.PublicBind != strings.TrimSpace(s.PublicBind) {
		return errors.New("service public_bind must not contain surrounding whitespace")
	}
	if _, err := netip.ParseAddr(s.PublicBind); err != nil {
		return errors.New("service public_bind must be an IP address")
	}
	// A zero public port requests allocation from the selected Gateway's
	// protocol-specific port pool. Explicit ports are checked by the scheduler
	// inside the same transaction that records the assignment.
	if s.PublicPort != 0 {
		if _, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(s.PublicBind, fmt.Sprint(s.PublicPort))); err != nil && s.Protocol == ProtocolTCP {
			return fmt.Errorf("service public bind: %w", err)
		}
	}
	if err := validateHostPort(s.LocalTarget); err != nil {
		return fmt.Errorf("service local_target must be host:port: %w", err)
	}
	return nil
}

func (a Assignment) Validate() error {
	if err := ValidateID(a.ID, "assignment.id"); err != nil {
		return err
	}
	if err := ValidateID(a.GatewayID, "assignment.gateway_id"); err != nil {
		return err
	}
	if err := ValidateID(a.AgentID, "assignment.agent_id"); err != nil {
		return err
	}
	if a.Generation == 0 {
		return errors.New("assignment generation must be positive")
	}
	if a.State != "" && a.State != AssignmentPending && a.State != AssignmentApplied && a.State != AssignmentDegraded && a.State != AssignmentDraining {
		return &ApplyError{Code: "invalid_assignment_state", Path: "assignment.state", Message: "assignment state is invalid"}
	}
	if a.PublicEndpoint != "" {
		if err := validateEndpoint(a.PublicEndpoint); err != nil {
			return &ApplyError{Code: "invalid_endpoint", Path: "assignment.public_endpoint", Message: err.Error()}
		}
	}
	if len(a.ServiceIDs) > maxAssignmentServices || len(a.Bindings) > maxAssignmentBindings {
		return &ApplyError{Code: "assignment_too_large", Message: "assignment contains too many services or bindings"}
	}
	serviceIDs := make(map[string]struct{}, len(a.ServiceIDs))
	for _, serviceID := range a.ServiceIDs {
		if err := ValidateID(serviceID, "assignment.service_ids"); err != nil {
			return err
		}
		if _, ok := serviceIDs[serviceID]; ok {
			return errors.New("assignment contains duplicate service ids")
		}
		serviceIDs[serviceID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(a.Bindings))
	seenBindingServices := make(map[string]struct{}, len(a.Bindings))
	for _, binding := range a.Bindings {
		if err := ValidateID(binding.ServiceID, "assignment.binding.service_id"); err != nil {
			return err
		}
		if binding.Protocol != ProtocolTCP && binding.Protocol != ProtocolUDP {
			return errors.New("assignment binding protocol must be tcp or udp")
		}
		if binding.Port == 0 {
			return errors.New("assignment binding port must be non-zero")
		}
		if binding.Bind != strings.TrimSpace(binding.Bind) {
			return errors.New("assignment binding bind must not contain surrounding whitespace")
		}
		if _, err := netip.ParseAddr(binding.Bind); err != nil {
			return errors.New("assignment binding bind must be an IP address")
		}
		if _, exists := seenBindingServices[binding.ServiceID]; exists {
			return errors.New("assignment contains multiple bindings for one service")
		}
		seenBindingServices[binding.ServiceID] = struct{}{}
		bind := strings.TrimSpace(binding.Bind)
		if address, err := netip.ParseAddr(bind); err == nil {
			bind = address.Unmap().String()
		}
		key := fmt.Sprintf("%s|%s|%d", binding.Protocol, bind, binding.Port)
		if _, ok := seen[key]; ok {
			return errors.New("assignment contains duplicate public bindings")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateListener(l Listener) error {
	if l.Protocol != ProtocolTCP && l.Protocol != ProtocolUDP {
		return errors.New("listener protocol must be tcp or udp")
	}
	if l.Port == 0 {
		return errors.New("listener port must be non-zero")
	}
	if strings.TrimSpace(l.Bind) == "" {
		return errors.New("listener bind is required")
	}
	if l.Bind != strings.TrimSpace(l.Bind) {
		return errors.New("listener bind must not contain surrounding whitespace")
	}
	if _, err := netip.ParseAddr(l.Bind); err != nil {
		return fmt.Errorf("listener bind must be an IP address: %w", err)
	}
	return nil
}

func normalizedIP(value string) string {
	value = strings.TrimSpace(value)
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Unmap().String()
	}
	return value
}

func validateEndpoint(value string) error {
	if value != strings.TrimSpace(value) {
		return errors.New("endpoint must not contain surrounding whitespace")
	}
	if value == "" || len(value) > 2048 {
		return errors.New("endpoint is empty or too long")
	}
	return validateHostPort(value)
}

func validateHostPort(value string) error {
	if containsControl(value) || len(value) > 2048 {
		return errors.New("host:port contains invalid characters")
	}
	if value != strings.TrimSpace(value) {
		return errors.New("host:port must not contain surrounding whitespace")
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return err
	}
	if strings.TrimSpace(host) == "" || host != strings.TrimSpace(host) || strings.ContainsAny(host, "\t ") || strings.ContainsAny(portText, "\t ") {
		return errors.New("host is required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func (p PortPool) Validate() error {
	for name, ranges := range map[string][]PortRange{"tcp": p.TCP, "udp": p.UDP} {
		for _, r := range ranges {
			if r.Min == 0 || r.Max < r.Min {
				return &ApplyError{Code: "invalid_port_pool", Path: "gateway.port_pool." + name, Message: "port range is invalid"}
			}
		}
		ordered := append([]PortRange(nil), ranges...)
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].Min != ordered[j].Min {
				return ordered[i].Min < ordered[j].Min
			}
			return ordered[i].Max < ordered[j].Max
		})
		for i := 1; i < len(ordered); i++ {
			if ordered[i].Min <= ordered[i-1].Max {
				return &ApplyError{Code: "invalid_port_pool", Path: "gateway.port_pool." + name, Message: "port ranges overlap"}
			}
		}
	}
	return nil
}

func ValidateID(value, path string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed != value {
		return &ApplyError{Code: "invalid_id", Path: path, Message: "id must not contain surrounding whitespace"}
	}
	value = trimmed
	if len(value) < 1 || len(value) > 128 {
		return &ApplyError{Code: "invalid_id", Path: path, Message: "id must contain 1 to 128 characters"}
	}
	for i, r := range value {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') || i == 0 && (r == '-' || r == '_' || r == '.') {
			return &ApplyError{Code: "invalid_id", Path: path, Message: "id contains an invalid character"}
		}
	}
	last := value[len(value)-1]
	if last == '-' || last == '_' || last == '.' {
		return &ApplyError{Code: "invalid_id", Path: path, Message: "id must not end with punctuation"}
	}
	return nil
}

// ComputeChecksum hashes a canonical copy with Checksum and repository-owned
// metadata cleared. Resource revisions/timestamps describe the Controller's
// storage history, not data-plane behavior; including them would advance a
// desired generation merely because a row was rewritten during a transaction.
// JSON is sufficient here because all remaining fields are deterministic and
// maps are sorted by encoding/json.
func (s DesiredSnapshot) ComputeChecksum() (string, error) {
	s = s.Normalize()
	s.Checksum = ""
	if s.Gateway != nil {
		s.Gateway.Revision = 0
	}
	if s.Agent != nil {
		s.Agent.Revision = 0
	}
	for i := range s.Services {
		s.Services[i].Revision = 0
		s.Services[i].UpdatedAt = time.Time{}
	}
	for i := range s.Assignments {
		s.Assignments[i].Revision = 0
		s.Assignments[i].UpdatedAt = time.Time{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

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
	b, err := json.Marshal(s)
	if err != nil {
		return DesiredSnapshot{}
	}
	var clone DesiredSnapshot
	if err := json.Unmarshal(b, &clone); err != nil {
		return DesiredSnapshot{}
	}
	return clone
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
