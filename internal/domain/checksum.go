package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrMissingObfuscationKeyID means a snapshot reached checksum construction
// before the Controller converted its storage representation into a canonical
// identity.  A ciphertext is deliberately never a checksum identity.
var ErrMissingObfuscationKeyID = errors.New("checksum requires a canonical obfuscation key id")

// ObfuscationKeyID is the stable, non-secret identity of a data-plane key.
// Both the at-rest and wire representations use this value; ciphertext,
// nonces and serialization details never participate in the identity.
func ObfuscationKeyID(key []byte) string {
	if len(key) == 0 {
		return ""
	}
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:])
}

// ChecksumDocument is the only document serialized by ComputeChecksum.  It is
// intentionally separate from DesiredSnapshot so repository metadata and the
// two storage/wire forms of obfuscation keys cannot accidentally enter the
// checksum contract.
type ChecksumDocument struct {
	SchemaVersion uint32               `json:"schema_version"`
	NodeID        string               `json:"node_id"`
	Generation    uint64               `json:"generation"`
	Gateway       *ChecksumGateway     `json:"gateway,omitempty"`
	Agent         *ChecksumAgent       `json:"agent,omitempty"`
	Services      []ChecksumService    `json:"services,omitempty"`
	Assignments   []ChecksumAssignment `json:"assignments,omitempty"`
}

type ChecksumGateway struct {
	NodeID          string              `json:"node_id"`
	PublicEndpoints []string            `json:"public_endpoints"`
	Listeners       []Listener          `json:"listeners,omitempty"`
	Labels          map[string]string   `json:"labels,omitempty"`
	Capacity        Capacity            `json:"capacity"`
	PortPool        PortPool            `json:"port_pool"`
	Transport       TransportPolicy     `json:"transport"`
	Obfuscation     ChecksumObfuscation `json:"obfuscation"`
	Egress          EgressPolicy        `json:"egress"`
}

type ChecksumAgent struct {
	NodeID          string        `json:"node_id"`
	GatewaySelector Selector      `json:"gateway_selector"`
	Proxies         []ProxySpec   `json:"proxies,omitempty"`
	Routes          []RouteRule   `json:"routes,omitempty"`
	Limits          AgentLimits   `json:"limits"`
	Egress          EgressPolicy  `json:"egress"`
	Logging         LoggingPolicy `json:"logging"`
}

type ChecksumService struct {
	ID              string   `json:"id"`
	AgentID         string   `json:"agent_id"`
	Protocol        string   `json:"protocol"`
	LocalTarget     string   `json:"local_target"`
	PublicBind      string   `json:"public_bind"`
	PublicPort      uint16   `json:"public_port"`
	GatewaySelector Selector `json:"gateway_selector"`
	Enabled         bool     `json:"enabled"`
}

type ChecksumAssignment struct {
	ID             string              `json:"id"`
	GatewayID      string              `json:"gateway_id"`
	AgentID        string              `json:"agent_id"`
	ServiceIDs     []string            `json:"service_ids"`
	Bindings       []Binding           `json:"bindings,omitempty"`
	Generation     uint64              `json:"generation"`
	State          string              `json:"state"`
	PublicEndpoint string              `json:"public_endpoint,omitempty"`
	Obfuscation    ChecksumObfuscation `json:"obfuscation,omitempty"`
}

// ChecksumObfuscation contains only stable key identities and policy.  The
// plaintext wire key and Controller-at-rest ciphertext are intentionally not
// representable here.
type ChecksumObfuscation struct {
	Mode             string `json:"mode"`
	KeyID            string `json:"key_id,omitempty"`
	PreviousKeyID    string `json:"previous_key_id,omitempty"`
	MaxPaddingBytes  int    `json:"max_padding_bytes"`
	HandshakeShaping bool   `json:"handshake_shaping"`
}

// ChecksumDocument builds the canonical, metadata-free checksum input. The
// method normalizes resource ordering before copying values so callers cannot
// mutate the snapshot while it is being serialized.
func (s DesiredSnapshot) ChecksumDocument() (ChecksumDocument, error) {
	canonical := s.Normalize()
	document := ChecksumDocument{
		SchemaVersion: canonical.SchemaVersion,
		NodeID:        canonical.NodeID,
		Generation:    canonical.Generation,
	}
	if canonical.Gateway != nil {
		gateway := canonical.Gateway
		obfuscation, err := checksumObfuscation(gateway.Obfuscation)
		if err != nil {
			return ChecksumDocument{}, fmt.Errorf("gateway obfuscation: %w", err)
		}
		document.Gateway = &ChecksumGateway{
			NodeID:          gateway.NodeID,
			PublicEndpoints: append([]string(nil), gateway.PublicEndpoints...),
			Listeners:       append([]Listener(nil), gateway.Listeners...),
			Labels:          cloneLabels(gateway.Labels),
			Capacity:        gateway.Capacity,
			PortPool:        PortPool{TCP: append([]PortRange(nil), gateway.PortPool.TCP...), UDP: append([]PortRange(nil), gateway.PortPool.UDP...)},
			Transport:       gateway.Transport,
			Obfuscation:     obfuscation,
			Egress:          cloneEgress(gateway.Egress),
		}
	}
	if canonical.Agent != nil {
		agent := canonical.Agent
		routes := append([]RouteRule(nil), agent.Routes...)
		for index := range routes {
			routes[index].CIDRs = append([]string(nil), routes[index].CIDRs...)
			routes[index].Domains = append([]string(nil), routes[index].Domains...)
			routes[index].GeoIP = append([]string(nil), routes[index].GeoIP...)
		}
		document.Agent = &ChecksumAgent{
			NodeID:          agent.NodeID,
			GatewaySelector: Selector{MatchLabels: cloneLabels(agent.GatewaySelector.MatchLabels)},
			Proxies:         append([]ProxySpec(nil), agent.Proxies...),
			Routes:          routes,
			Limits:          agent.Limits,
			Egress:          cloneEgress(agent.Egress),
			Logging:         agent.Logging,
		}
	}
	for _, service := range canonical.Services {
		document.Services = append(document.Services, ChecksumService{
			ID:              service.ID,
			AgentID:         service.AgentID,
			Protocol:        service.Protocol,
			LocalTarget:     service.LocalTarget,
			PublicBind:      service.PublicBind,
			PublicPort:      service.PublicPort,
			GatewaySelector: Selector{MatchLabels: cloneLabels(service.GatewaySelector.MatchLabels)},
			Enabled:         service.Enabled,
		})
	}
	for _, assignment := range canonical.Assignments {
		obfuscation, err := checksumObfuscation(assignment.Obfuscation)
		if err != nil {
			return ChecksumDocument{}, fmt.Errorf("assignment %q obfuscation: %w", assignment.ID, err)
		}
		document.Assignments = append(document.Assignments, ChecksumAssignment{
			ID:             assignment.ID,
			GatewayID:      assignment.GatewayID,
			AgentID:        assignment.AgentID,
			ServiceIDs:     append([]string(nil), assignment.ServiceIDs...),
			Bindings:       append([]Binding(nil), assignment.Bindings...),
			Generation:     assignment.Generation,
			State:          assignment.State,
			PublicEndpoint: assignment.PublicEndpoint,
			Obfuscation:    obfuscation,
		})
	}
	return document, nil
}

func checksumObfuscation(value ObfuscationPolicy) (ChecksumObfuscation, error) {
	result := ChecksumObfuscation{
		Mode:             value.Mode,
		KeyID:            value.KeyID,
		PreviousKeyID:    value.PreviousKeyID,
		MaxPaddingBytes:  value.MaxPaddingBytes,
		HandshakeShaping: value.HandshakeShaping,
	}
	switch value.Mode {
	case "", "standard":
		return result, nil
	case "camouflage":
		if value.KeyID == "" {
			return ChecksumObfuscation{}, ErrMissingObfuscationKeyID
		}
		if value.PreviousKeyID == "" && (len(value.PreviousKey) > 0 || len(value.PreviousKeyCiphertext) > 0) {
			return ChecksumObfuscation{}, fmt.Errorf("%w: previous key", ErrMissingObfuscationKeyID)
		}
		return result, nil
	default:
		return ChecksumObfuscation{}, fmt.Errorf("unsupported obfuscation mode %q", value.Mode)
	}
}

// ComputeChecksum hashes only ChecksumDocument. Keeping the hash operation
// next to its input builder makes it impossible for callers to accidentally
// hash a repository document or a wire document directly.
func (s DesiredSnapshot) ComputeChecksum() (string, error) {
	document, err := s.ChecksumDocument()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}
