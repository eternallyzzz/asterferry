package observability

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"asterferry/internal/dashboard"
)

type dashboardTestProvider struct {
	ready bool
}

func (p *dashboardTestProvider) IsReady() bool { return p.ready }
func (p *dashboardTestProvider) Status() any   { return map[string]any{"role": "test"} }
func (p *dashboardTestProvider) Dashboard() DashboardSnapshot {
	return DashboardSnapshot{Role: "agent", Ready: p.ready, State: "running", NodeID: "test-node"}
}

type dashboardTestActions struct {
	shutdown  atomic.Int32
	reconnect atomic.Int32
}

func (a *dashboardTestActions) RequestShutdown() error {
	a.shutdown.Add(1)
	return nil
}

func (a *dashboardTestActions) RequestReconnect() error {
	a.reconnect.Add(1)
	return nil
}

func TestEventHubSanitizesAndReplays(t *testing.T) {
	hub := NewEventHub(2)
	record := slog.NewRecord(time.Now(), slog.LevelWarn, "secret message", 0)
	record.AddAttrs(
		slog.String("event", "gateway.auth.rejected"),
		slog.String("role", "gateway"),
		slog.String("agent_id", "edge-a"),
		slog.String("error", "private detail"),
		slog.String("token", "must-not-appear"),
		slog.Bool("security_audit", true),
	)
	hub.Publish(record, nil)
	stream, err := hub.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if len(stream.Replay) != 1 {
		t.Fatalf("replay count = %d", len(stream.Replay))
	}
	event := stream.Replay[0]
	if event.Event != "gateway.auth.rejected" || event.Role != "gateway" || !event.SecurityAudit {
		t.Fatalf("event = %#v", event)
	}
	if event.Attributes["agent_id"] != "edge-a" || event.Attributes["error"] != "" {
		t.Fatalf("event attributes leaked or missing: %#v", event.Attributes)
	}
	if strings.Contains(string(mustJSON(t, event)), "secret") || strings.Contains(string(mustJSON(t, event)), "token") {
		t.Fatalf("sensitive data reached event JSON: %#v", event)
	}
}

func TestManagementDashboardActionsAndSSE(t *testing.T) {
	provider := &dashboardTestProvider{ready: true}
	actions := &dashboardTestActions{}
	hub := NewEventHub(1)
	server, err := Start("127.0.0.1:0", &Metrics{}, provider, []byte(testManagementToken), ServerOptions{
		Events:    hub,
		Actions:   actions,
		Dashboard: dashboard.Handler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	base := "http://" + server.listener.Addr().String()
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(request *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	response, err := client.Get(base + "/dashboard/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "AsterFerry Dashboard") {
		t.Fatalf("dashboard response = %d location=%q %q", response.StatusCode, response.Header.Get("Location"), body)
	}
	if got := response.Header.Get("Content-Security-Policy"); got == "" {
		t.Fatal("dashboard security headers missing")
	}

	response, err = client.Get(base + "/v1/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dashboard API = %d", response.StatusCode)
	}

	response = authorizedRequest(t, client, http.MethodGet, base+"/v1/dashboard", "")
	var snapshot DashboardSnapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || snapshot.SchemaVersion != DashboardSchemaVersion || snapshot.Metrics.Connections != 0 {
		t.Fatalf("snapshot = %#v, status=%d", snapshot, response.StatusCode)
	}

	response = authorizedRequest(t, client, http.MethodPost, base+"/v1/actions/shutdown", "")
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted || actions.shutdown.Load() != 1 {
		t.Fatalf("shutdown response=%d calls=%d", response.StatusCode, actions.shutdown.Load())
	}
	response = authorizedRequest(t, client, http.MethodPost, base+"/v1/actions/reconnect", "")
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted || actions.reconnect.Load() != 1 {
		t.Fatalf("reconnect response=%d calls=%d", response.StatusCode, actions.reconnect.Load())
	}

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "event", 0)
	record.AddAttrs(slog.String("event", "agent.session.connected"))
	hub.Publish(record, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testManagementToken)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || line != "id: 1\n" {
		t.Fatalf("SSE first line = status %d %q", response.StatusCode, line)
	}
}

func authorizedRequest(t *testing.T, client *http.Client, method, url, lastEventID string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testManagementToken)
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestActionErrorsRemainStable(t *testing.T) {
	provider := &dashboardTestProvider{}
	actions := &dashboardTestActions{}
	server, err := Start("127.0.0.1:0", nil, provider, []byte(testManagementToken), ServerOptions{Actions: actionUnsupported{}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := &http.Client{Timeout: time.Second}
	response := authorizedRequest(t, client, http.MethodPost, "http://"+server.listener.Addr().String()+"/v1/actions/shutdown", "")
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("unsupported action status = %d", response.StatusCode)
	}
	_ = actions
}

type actionUnsupported struct{}

func (actionUnsupported) RequestShutdown() error  { return ErrActionUnsupported }
func (actionUnsupported) RequestReconnect() error { return errors.New("unexpected") }
