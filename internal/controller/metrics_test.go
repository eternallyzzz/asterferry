package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"asterferry/internal/domain"
)

func TestControllerMetricsExposeRuntimeSignalsWithoutResourceLabels(t *testing.T) {
	metrics := newControllerMetrics()
	metrics.observeHTTP(http.MethodGet, "/api/v1/nodes", http.StatusOK, time.Millisecond)
	metrics.observeGRPC("Connect", "OK")
	metrics.observeNode("node-secret", string(domain.NodeSpecGateway), domain.ObservedState{
		SchemaVersion:     domain.CurrentControlProtocolVersion,
		NodeID:            "node-secret",
		AppliedGeneration: 7,
		Healthy:           true,
		ObservedAt:        time.Now().UTC(),
		Metrics: domain.RuntimeMetrics{
			ActiveStreams:  2,
			ActiveSessions: 1,
			ActiveEgress:   3,
		},
		Listeners: []domain.ListenerState{{Protocol: domain.ProtocolTCP}},
	})

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, metric := range []string{
		"asterferry_controller_http_requests_total",
		"asterferry_controller_grpc_requests_total",
		"asterferry_controller_snapshot_generation",
		"asterferry_controller_node_active_streams",
		"asterferry_controller_node_listeners",
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("metrics output does not contain %q:\n%s", metric, body)
		}
	}
	if strings.Contains(body, "node-secret") {
		t.Fatal("metrics output leaked a resource identifier")
	}
}

func TestControllerMetricsAggregateGeoIPByKind(t *testing.T) {
	metrics := newControllerMetrics()
	observed := domain.ObservedState{
		SchemaVersion: domain.CurrentControlProtocolVersion,
		Healthy:       true,
		Metrics:       domain.RuntimeMetrics{GeoIPUp: true},
	}
	metrics.observeNode("gateway-up", string(domain.NodeSpecGateway), observed)

	observed.Metrics.GeoIPUp = false
	metrics.observeNode("gateway-down", string(domain.NodeSpecGateway), observed)
	observed.Metrics.GeoIPUp = true
	metrics.observeNode("agent-up", string(domain.NodeSpecAgent), observed)

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, `asterferry_controller_geoip_up{kind="gateway"} 1`) {
		t.Fatalf("gateway GeoIP availability count missing or incorrect:\n%s", body)
	}
	if !strings.Contains(body, `asterferry_controller_geoip_up{kind="agent"} 1`) {
		t.Fatalf("agent GeoIP availability count missing or incorrect:\n%s", body)
	}

	// A later observation without geoip_up must clear the previous node's
	// availability instead of leaving a stale value in the aggregate.
	observed.Metrics = domain.RuntimeMetrics{}
	metrics.observeNode("gateway-up", string(domain.NodeSpecGateway), observed)
	recorder = httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(recorder.Body.String(), `asterferry_controller_geoip_up{kind="gateway"} 0`) {
		t.Fatalf("gateway GeoIP availability did not clear after missing observation:\n%s", recorder.Body.String())
	}
}
