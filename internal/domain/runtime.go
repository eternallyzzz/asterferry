package domain

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Runtime connection types are intentionally transport-oriented.  A session
// is the authenticated node-to-node channel; TCP connections and UDP flows
// are the operator-visible children that carry user traffic.
const (
	RuntimeConnectionSession = "session"
	RuntimeConnectionTCP     = "tcp"
	RuntimeConnectionUDP     = "udp_flow"
	RuntimeConnectionEgress  = "egress"

	RuntimeStateActive  = "active"
	RuntimeStateClosed  = "closed"
	RuntimeStateUnknown = "unknown"

	RuntimeClosePeer        = "peer_closed"
	RuntimeCloseOperator    = "operator_disconnect"
	RuntimeCloseGeneration  = "generation_replaced"
	RuntimeCloseSession     = "session_closed"
	RuntimeCloseIdle        = "idle_timeout"
	RuntimeCloseDialFailed  = "dial_failed"
	RuntimeCloseRejected    = "rejected"
	RuntimeCloseRateLimited = "rate_limited"
	RuntimeCloseController  = "controller_shutdown"
	RuntimeCloseUnknown     = "closed"

	RuntimeEventOpened      = "opened"
	RuntimeEventUpdated     = "updated"
	RuntimeEventClosed      = "closed"
	RuntimeEventRejected    = "rejected"
	RuntimeEventRateLimited = "rate_limited"
)

// RuntimeRateLimit describes an ephemeral operator policy.  Limits are kept
// in the node process and are deliberately not part of desired snapshots:
// they are an operational control, not configuration that should survive a
// node replacement or be replayed accidentally after a restart.
type RuntimeRateLimit struct {
	Direction      string    `json:"direction"`
	BytesPerSecond uint64    `json:"bytes_per_second"`
	BurstBytes     uint64    `json:"burst_bytes"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// RuntimeConnection is the safe, payload-free metadata exposed to operators.
// It contains addresses, protocol and counters, never application bytes.
type RuntimeConnection struct {
	ID              string     `json:"id"`
	Type            string     `json:"type"`
	NodeID          string     `json:"node_id"`
	PeerNodeID      string     `json:"peer_node_id,omitempty"`
	GatewayID       string     `json:"gateway_id,omitempty"`
	AgentID         string     `json:"agent_id,omitempty"`
	AssignmentID    string     `json:"assignment_id,omitempty"`
	ServiceID       string     `json:"service_id,omitempty"`
	Protocol        string     `json:"protocol"`
	SourceIP        string     `json:"source_ip,omitempty"`
	SourcePort      uint16     `json:"source_port,omitempty"`
	Target          string     `json:"target,omitempty"`
	ParentSessionID string     `json:"parent_session_id,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	LastActivityAt  time.Time  `json:"last_activity_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	State           string     `json:"state"`
	CloseReason     string     `json:"close_reason,omitempty"`
	BytesIn         uint64     `json:"bytes_in"`
	BytesOut        uint64     `json:"bytes_out"`
	// RateIn and RateOut are cumulative average byte rates since StartedAt,
	// rather than instantaneous or rolling-window rates.
	RateIn  float64           `json:"rate_in_bps"`
	RateOut float64           `json:"rate_out_bps"`
	Limit   *RuntimeRateLimit `json:"limit,omitempty"`
}

func (c RuntimeConnection) Validate() error {
	if err := ValidateID(c.ID, "runtime_connection.id"); err != nil {
		return err
	}
	if err := ValidateID(c.NodeID, "runtime_connection.node_id"); err != nil {
		return err
	}
	switch c.Type {
	case RuntimeConnectionSession, RuntimeConnectionTCP, RuntimeConnectionUDP, RuntimeConnectionEgress:
	default:
		return fmt.Errorf("runtime connection type %q is invalid", c.Type)
	}
	switch c.State {
	case RuntimeStateActive, RuntimeStateClosed, RuntimeStateUnknown:
	default:
		return fmt.Errorf("runtime connection state %q is invalid", c.State)
	}
	if c.Protocol != "" && c.Protocol != ProtocolTCP && c.Protocol != ProtocolUDP && c.Protocol != "quic" {
		return fmt.Errorf("runtime connection protocol %q is invalid", c.Protocol)
	}
	for name, value := range map[string]string{
		"peer_node_id": c.PeerNodeID, "gateway_id": c.GatewayID, "agent_id": c.AgentID,
		"assignment_id": c.AssignmentID, "service_id": c.ServiceID, "parent_session_id": c.ParentSessionID,
	} {
		if value != "" {
			if err := ValidateID(value, "runtime_connection."+name); err != nil {
				return err
			}
		}
	}
	if c.SourceIP != "" {
		if _, err := netip.ParseAddr(c.SourceIP); err != nil {
			return fmt.Errorf("runtime connection source_ip is invalid: %w", err)
		}
	}
	if len(c.Target) > 2048 || strings.ContainsAny(c.Target, "\x00\r\n") {
		return errors.New("runtime connection target is invalid")
	}
	if c.StartedAt.IsZero() || c.LastActivityAt.IsZero() {
		return errors.New("runtime connection timestamps are required")
	}
	if c.EndedAt != nil && c.EndedAt.Before(c.StartedAt) {
		return errors.New("runtime connection ended_at precedes started_at")
	}
	if c.Limit != nil {
		if err := c.Limit.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (l RuntimeRateLimit) Validate() error {
	if l.Direction != "in" && l.Direction != "out" && l.Direction != "both" {
		return errors.New("runtime rate limit direction must be in, out or both")
	}
	if l.BytesPerSecond == 0 || l.BytesPerSecond > 1<<40 {
		return errors.New("runtime rate limit bytes_per_second is out of range")
	}
	if l.BurstBytes == 0 || l.BurstBytes > 1<<40 {
		return errors.New("runtime rate limit burst_bytes is out of range")
	}
	if l.ExpiresAt.IsZero() {
		return errors.New("runtime rate limit expiration is required")
	}
	return nil
}

// RuntimeEvent is the node-to-controller telemetry envelope.  The full
// connection is included for opened/updated/closed events so the Controller
// can rebuild current state after an event retry without depending on event
// ordering.  Payloads remain metadata-only.
type RuntimeEvent struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	NodeID       string             `json:"node_id"`
	ConnectionID string             `json:"connection_id,omitempty"`
	Connection   *RuntimeConnection `json:"connection,omitempty"`
	Message      string             `json:"message,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
}

func (e RuntimeEvent) Validate() error {
	if err := ValidateID(e.ID, "runtime_event.id"); err != nil {
		return err
	}
	if err := ValidateID(e.NodeID, "runtime_event.node_id"); err != nil {
		return err
	}
	switch e.Type {
	case RuntimeEventOpened, RuntimeEventUpdated, RuntimeEventClosed, RuntimeEventRejected, RuntimeEventRateLimited:
	default:
		return fmt.Errorf("runtime event type %q is invalid", e.Type)
	}
	if e.ConnectionID == "" && e.Connection == nil && e.Type != RuntimeEventRejected {
		return errors.New("runtime event connection is required")
	}
	if e.ConnectionID != "" {
		if err := ValidateID(e.ConnectionID, "runtime_event.connection_id"); err != nil {
			return err
		}
	}
	if e.Connection != nil {
		if err := e.Connection.Validate(); err != nil {
			return err
		}
		if e.Connection.NodeID != e.NodeID {
			return errors.New("runtime event connection node does not match event node")
		}
		if e.ConnectionID != "" && e.Connection.ID != e.ConnectionID {
			return errors.New("runtime event connection id does not match event")
		}
	}
	if len(e.Message) > 1024 || strings.ContainsAny(e.Message, "\x00\r\n") {
		return errors.New("runtime event message is invalid")
	}
	if e.CreatedAt.IsZero() {
		return errors.New("runtime event timestamp is required")
	}
	return nil
}

type RuntimeSnapshot struct {
	NodeID      string              `json:"node_id"`
	ObservedAt  time.Time           `json:"observed_at"`
	Connections []RuntimeConnection `json:"connections,omitempty"`
	Metrics     map[string]float64  `json:"metrics,omitempty"`
}

func (s RuntimeSnapshot) Validate() error {
	if err := ValidateID(s.NodeID, "runtime_snapshot.node_id"); err != nil {
		return err
	}
	if s.ObservedAt.IsZero() {
		return errors.New("runtime snapshot timestamp is required")
	}
	if len(s.Connections) > 4096 || len(s.Metrics) > 128 {
		return errors.New("runtime snapshot is too large")
	}
	for _, connection := range s.Connections {
		if err := connection.Validate(); err != nil {
			return err
		}
		if connection.NodeID != s.NodeID {
			return errors.New("runtime snapshot connection node does not match snapshot node")
		}
	}
	for key, value := range s.Metrics {
		if len(key) > 128 || strings.ContainsAny(key, "\x00\r\n") || value != value || value > 1e18 || value < -1e18 {
			return errors.New("runtime snapshot metric is invalid")
		}
	}
	return nil
}
