package observability

import (
	"errors"
	"time"
)

const DashboardSchemaVersion = 1

// DashboardSnapshot is the stable, browser-oriented view of a running role.
// The existing /v1/status and /metrics endpoints remain the compatibility
// interfaces for scripts and Prometheus-style collectors.
type DashboardSnapshot struct {
	SchemaVersion int                        `json:"schema_version"`
	GeneratedAt   time.Time                  `json:"generated_at"`
	Role          string                     `json:"role"`
	State         string                     `json:"state"`
	Ready         bool                       `json:"ready"`
	NodeID        string                     `json:"node_id"`
	Transport     DashboardTransportSnapshot `json:"transport"`
	Metrics       MetricsSnapshot            `json:"metrics"`
	Gateway       *GatewayDashboardSnapshot  `json:"gateway,omitempty"`
	Agent         *AgentDashboardSnapshot    `json:"agent,omitempty"`
}

type DashboardTransportSnapshot struct {
	Protocol        int    `json:"protocol"`
	ObfuscationMode string `json:"obfuscation_mode"`
	KeyFingerprint  string `json:"key_fingerprint,omitempty"`
}

type GatewayDashboardSnapshot struct {
	Agents   []GatewayAgentSnapshot   `json:"agents"`
	Mappings []GatewayMappingSnapshot `json:"mappings"`
}

type GatewayAgentSnapshot struct {
	AgentID      string `json:"agent_id"`
	SessionID    string `json:"session_id"`
	NodeID       string `json:"node_id"`
	Connected    bool   `json:"connected"`
	MappingCount int    `json:"mapping_count"`
}

type GatewayMappingSnapshot struct {
	Name        string `json:"name"`
	AgentID     string `json:"agent_id"`
	Protocol    string `json:"protocol"`
	GatewayPort uint16 `json:"gateway_port"`
	Profile     string `json:"profile"`
	State       string `json:"state"`
}

type AgentDashboardSnapshot struct {
	AgentID         string                 `json:"agent_id"`
	Connected       bool                   `json:"connected"`
	SessionID       string                 `json:"session_id,omitempty"`
	Reconnects      int64                  `json:"reconnects"`
	Inbounds        []AgentInboundSnapshot `json:"inbounds"`
	ReverseMappings []AgentReverseSnapshot `json:"reverse_mappings"`
}

type AgentInboundSnapshot struct {
	Tag      string `json:"tag"`
	Protocol string `json:"protocol"`
	Listen   string `json:"listen"`
}

type AgentReverseSnapshot struct {
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	GatewayPort uint16 `json:"gateway_port"`
	Local       string `json:"local"`
}

type MetricsSnapshot struct {
	Connections                    int64               `json:"connections"`
	ActiveStreams                  int64               `json:"active_streams"`
	Draining                       bool                `json:"draining"`
	Shutdowns                      uint64              `json:"shutdowns_total"`
	ForcedShutdowns                uint64              `json:"forced_shutdowns_total"`
	BytesIn                        uint64              `json:"bytes_in_total"`
	BytesOut                       uint64              `json:"bytes_out_total"`
	AuthFailures                   uint64              `json:"auth_failures_total"`
	ManagementAuthFailures         uint64              `json:"management_auth_failures_total"`
	ManagementAuthRateLimited      uint64              `json:"management_auth_rate_limited_total"`
	ManagementActionsAccepted      uint64              `json:"management_actions_accepted_total"`
	ManagementActionsRejected      uint64              `json:"management_actions_rejected_total"`
	ManagementEventStreamsRejected uint64              `json:"management_event_stream_rejections_total"`
	ManagementEventSubscribers     int64               `json:"management_event_subscribers"`
	MappingFailures                uint64              `json:"mapping_failures_total"`
	ObfuscationAccepted            uint64              `json:"obfuscation_packets_accepted_total"`
	ObfuscationRejected            uint64              `json:"obfuscation_packets_rejected_total"`
	ObfuscationPreviousKey         uint64              `json:"obfuscation_previous_key_total"`
	ObfuscationFragmentsDropped    uint64              `json:"obfuscation_fragments_dropped_total"`
	QUIC                           QUICMetricsSnapshot `json:"quic"`
}

type QUICMetricsSnapshot struct {
	RTTMicros       uint64 `json:"rtt_microseconds"`
	BytesSent       uint64 `json:"bytes_sent"`
	BytesReceived   uint64 `json:"bytes_received"`
	BytesLost       uint64 `json:"bytes_lost"`
	PacketsSent     uint64 `json:"packets_sent"`
	PacketsReceived uint64 `json:"packets_received"`
	PacketsLost     uint64 `json:"packets_lost"`
	GSO             bool   `json:"gso"`
	StatsSamples    uint64 `json:"stats_samples"`
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	return MetricsSnapshot{
		Connections:                    m.Connections.Load(),
		ActiveStreams:                  m.ActiveStreams.Load(),
		Draining:                       m.Draining.Load(),
		Shutdowns:                      m.Shutdowns.Load(),
		ForcedShutdowns:                m.ForcedShutdowns.Load(),
		BytesIn:                        m.BytesIn.Load(),
		BytesOut:                       m.BytesOut.Load(),
		AuthFailures:                   m.AuthFailures.Load(),
		ManagementAuthFailures:         m.ManagementAuthFailures.Load(),
		ManagementAuthRateLimited:      m.ManagementAuthRateLimited.Load(),
		ManagementActionsAccepted:      m.ManagementActionsAccepted.Load(),
		ManagementActionsRejected:      m.ManagementActionsRejected.Load(),
		ManagementEventStreamsRejected: m.ManagementEventStreamsRejected.Load(),
		ManagementEventSubscribers:     m.ManagementEventSubscribers.Load(),
		MappingFailures:                m.MappingFailures.Load(),
		ObfuscationAccepted:            m.ObfuscationAccepted.Load(),
		ObfuscationRejected:            m.ObfuscationRejected.Load(),
		ObfuscationPreviousKey:         m.ObfuscationPreviousKey.Load(),
		ObfuscationFragmentsDropped:    m.ObfuscationFragmentsDropped.Load(),
		QUIC: QUICMetricsSnapshot{
			RTTMicros:       m.QUICRTTMicros.Load(),
			BytesSent:       m.QUICBytesSent.Load(),
			BytesReceived:   m.QUICBytesReceived.Load(),
			BytesLost:       m.QUICBytesLost.Load(),
			PacketsSent:     m.QUICPacketsSent.Load(),
			PacketsReceived: m.QUICPacketsReceived.Load(),
			PacketsLost:     m.QUICPacketsLost.Load(),
			GSO:             m.QUICGSO.Load(),
			StatsSamples:    m.QUICStatsSamples.Load(),
		},
	}
}

type DashboardProvider interface {
	StatusProvider
	Dashboard() DashboardSnapshot
}

type ActionProvider interface {
	RequestShutdown() error
	RequestReconnect() error
}

// DeferredShutdownProvider lets the HTTP handler flush its 202 response
// before the runtime closes the management listener.
type DeferredShutdownProvider interface {
	ActionProvider
	CanShutdown() error
	TriggerShutdown()
}

var (
	ErrActionUnsupported = errors.New("management action is unsupported")
	ErrActionUnavailable = errors.New("management action is unavailable")
	ErrActionBusy        = errors.New("management action is already in progress")
)
