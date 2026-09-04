package domain

import (
	"bytes"
	"encoding/json"
)

// RuntimeMetricsSchemaVersion versions the typed metric catalog independently
// from the control protocol and the Controller database schema. A catalog
// change is an intentional control-contract change even when the wire
// protocol version remains compatible.
const RuntimeMetricsSchemaVersion uint32 = 1

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

type RuntimeMetricKind string

const (
	RuntimeMetricGauge   RuntimeMetricKind = "gauge"
	RuntimeMetricCounter RuntimeMetricKind = "counter"
	RuntimeMetricBoolean RuntimeMetricKind = "boolean"
)

// RuntimeMetricDescriptor is the single catalog entry for a persisted core
// metric. JSONName is the API/control name and SQLColumn is the normalized
// observed_states column. The catalog is deliberately finite: experimental,
// high-cardinality values belong in Prometheus instrumentation rather than
// silently extending the persisted control contract.
type RuntimeMetricDescriptor struct {
	JSONName  string
	SQLColumn string
	Kind      RuntimeMetricKind
}

var runtimeMetricCatalog = [...]RuntimeMetricDescriptor{
	{JSONName: "active_streams", SQLColumn: "active_streams", Kind: RuntimeMetricGauge},
	{JSONName: "active_sessions", SQLColumn: "active_sessions", Kind: RuntimeMetricGauge},
	{JSONName: "active_egress", SQLColumn: "active_egress", Kind: RuntimeMetricGauge},
	{JSONName: "udp_oversize_drops", SQLColumn: "udp_oversize_drops", Kind: RuntimeMetricCounter},
	{JSONName: "geoip_up", SQLColumn: "geoip_up", Kind: RuntimeMetricBoolean},
	{JSONName: "active_connections", SQLColumn: "active_connections", Kind: RuntimeMetricGauge},
	{JSONName: "active_flows", SQLColumn: "active_flows", Kind: RuntimeMetricGauge},
	{JSONName: "runtime_bytes_in_total", SQLColumn: "runtime_bytes_in_total", Kind: RuntimeMetricCounter},
	{JSONName: "runtime_bytes_out_total", SQLColumn: "runtime_bytes_out_total", Kind: RuntimeMetricCounter},
	{JSONName: "runtime_opened_total", SQLColumn: "runtime_opened_total", Kind: RuntimeMetricCounter},
	{JSONName: "runtime_closed_total", SQLColumn: "runtime_closed_total", Kind: RuntimeMetricCounter},
	{JSONName: "runtime_rejected_total", SQLColumn: "runtime_rejected_total", Kind: RuntimeMetricCounter},
	{JSONName: "runtime_rate_limited_total", SQLColumn: "runtime_rate_limited_total", Kind: RuntimeMetricCounter},
	{JSONName: "runtime_telemetry_dropped_total", SQLColumn: "runtime_telemetry_dropped_total", Kind: RuntimeMetricCounter},
}

// RuntimeMetricCatalog returns a copy so callers can validate or render the
// contract without mutating package state.
func RuntimeMetricCatalog() []RuntimeMetricDescriptor {
	return append([]RuntimeMetricDescriptor(nil), runtimeMetricCatalog[:]...)
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
