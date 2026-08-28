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
	metrics.observeNode("node-secret", domain.RoleGateway, domain.ObservedState{
		SchemaVersion:     domain.SchemaVersion,
		NodeID:            "node-secret",
		AppliedGeneration: 7,
		Healthy:           true,
		ObservedAt:        time.Now().UTC(),
		Metrics:           map[string]float64{"active_streams": 2, "active_sessions": 1, "active_egress": 3},
		Listeners:         []domain.ListenerState{{Protocol: domain.ProtocolTCP}},
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

func TestControllerMetricsAggregateGeoIPByRole(t *testing.T) {
	metrics := newControllerMetrics()
	observed := domain.ObservedState{
		SchemaVersion: domain.SchemaVersion,
		Healthy:       true,
		Metrics:       map[string]float64{"geoip_up": 1},
	}
	metrics.observeNode("gateway-up", domain.RoleGateway, observed)

	observed.Metrics["geoip_up"] = 0
	metrics.observeNode("gateway-down", domain.RoleGateway, observed)
	observed.Metrics["geoip_up"] = 1
	metrics.observeNode("agent-up", domain.RoleAgent, observed)

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, `asterferry_controller_geoip_up{role="gateway"} 1`) {
		t.Fatalf("gateway GeoIP availability count missing or incorrect:\n%s", body)
	}
	if !strings.Contains(body, `asterferry_controller_geoip_up{role="agent"} 1`) {
		t.Fatalf("agent GeoIP availability count missing or incorrect:\n%s", body)
	}

	// A later observation without geoip_up must clear the previous node's
	// availability instead of leaving a stale value in the aggregate.
	observed.Metrics = map[string]float64{}
	metrics.observeNode("gateway-up", domain.RoleGateway, observed)
	recorder = httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(recorder.Body.String(), `asterferry_controller_geoip_up{role="gateway"} 0`) {
		t.Fatalf("gateway GeoIP availability did not clear after missing observation:\n%s", recorder.Body.String())
	}
}
