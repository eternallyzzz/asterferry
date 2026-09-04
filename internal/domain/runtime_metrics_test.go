package domain

import (
	"encoding/json"
	"testing"
)

func TestRuntimeMetricsRejectsUnknownFields(t *testing.T) {
	var metrics RuntimeMetrics
	if err := json.Unmarshal([]byte(`{"active_streams":1,"future_metric":2}`), &metrics); err == nil {
		t.Fatal("unknown runtime metric was accepted")
	}
}

func TestRuntimeMetricsRoundTripKeepsTypedFields(t *testing.T) {
	want := RuntimeMetrics{
		ActiveStreams:                1,
		ActiveSessions:               2,
		ActiveEgress:                 3,
		UDPOversizeDrops:             4,
		GeoIPUp:                      true,
		ActiveConnections:            5,
		ActiveFlows:                  6,
		RuntimeBytesInTotal:          7,
		RuntimeBytesOutTotal:         8,
		RuntimeOpenedTotal:           9,
		RuntimeClosedTotal:           10,
		RuntimeRejectedTotal:         11,
		RuntimeRateLimitedTotal:      12,
		RuntimeTelemetryDroppedTotal: 13,
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got RuntimeMetrics
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("runtime metrics round trip = %#v, want %#v", got, want)
	}
}
