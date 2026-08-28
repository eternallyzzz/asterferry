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
