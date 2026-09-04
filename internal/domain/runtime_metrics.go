package domain

import (
	"bytes"
	"encoding/json"
)

// RuntimeMetrics is the versioned, controller-visible set of node metrics.
//
// Metrics are deliberately explicit. Adding a metric is a control-contract
// change, so producers, consumers, validation and the API description cannot
// silently drift through an untyped string key.
type RuntimeMetrics struct {
	ActiveStreams                uint64 `json:"active_streams,omitempty"`
	ActiveSessions               uint64 `json:"active_sessions,omitempty"`
	ActiveEgress                 uint64 `json:"active_egress,omitempty"`
	UDPOversizeDrops             uint64 `json:"udp_oversize_drops,omitempty"`
	GeoIPUp                      bool   `json:"geoip_up,omitempty"`
	ActiveConnections            uint64 `json:"active_connections,omitempty"`
	ActiveFlows                  uint64 `json:"active_flows,omitempty"`
	RuntimeBytesInTotal          uint64 `json:"runtime_bytes_in_total,omitempty"`
	RuntimeBytesOutTotal         uint64 `json:"runtime_bytes_out_total,omitempty"`
	RuntimeOpenedTotal           uint64 `json:"runtime_opened_total,omitempty"`
	RuntimeClosedTotal           uint64 `json:"runtime_closed_total,omitempty"`
	RuntimeRejectedTotal         uint64 `json:"runtime_rejected_total,omitempty"`
	RuntimeRateLimitedTotal      uint64 `json:"runtime_rate_limited_total,omitempty"`
	RuntimeTelemetryDroppedTotal uint64 `json:"runtime_telemetry_dropped_total,omitempty"`
}

// UnmarshalJSON keeps the type strict even when callers use encoding/json
// directly instead of the repository's DecodeStrict helper.
func (m *RuntimeMetrics) UnmarshalJSON(data []byte) error {
	type plain RuntimeMetrics
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value plain
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*m = RuntimeMetrics(value)
	return nil
}
