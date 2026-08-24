package observability

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testManagementToken = "management-token-0123456789abcdef"

type testStatusProvider struct {
	ready bool
	value any
}

func (p *testStatusProvider) IsReady() bool { return p.ready }
func (p *testStatusProvider) Status() any   { return p.value }

func TestManagementServerEndpoints(t *testing.T) {
	provider := &testStatusProvider{value: map[string]any{"mode": "test"}}
	server, err := Start("127.0.0.1:0", nil, provider, []byte(testManagementToken))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	status, body := getManagement(t, server.listener.Addr().String(), "/healthz")
	if status != http.StatusOK || body != "ok\n" {
		t.Fatalf("health response = %d %q", status, body)
	}
	status, body = getManagement(t, server.listener.Addr().String(), "/readyz")
	if status != http.StatusServiceUnavailable || body != "not ready\n" {
		t.Fatalf("not-ready response = %d %q", status, body)
	}
	provider.ready = true
	status, body = getManagement(t, server.listener.Addr().String(), "/readyz")
	if status != http.StatusOK || body != "ready\n" {
		t.Fatalf("ready response = %d %q", status, body)
	}
	status, body = getManagement(t, server.listener.Addr().String(), "/metrics")
	if status != http.StatusOK || !strings.Contains(body, "asterferry_draining 0") {
		t.Fatalf("metrics response = %d %q", status, body)
	}
	status, body = getManagement(t, server.listener.Addr().String(), "/v1/status")
	if status != http.StatusOK {
		t.Fatalf("status response = %d %q", status, body)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil || decoded["mode"] != "test" {
		t.Fatalf("status JSON = %#v, err=%v", decoded, err)
	}

	provider.value = func() {}
	status, body = getManagement(t, server.listener.Addr().String(), "/v1/status")
	if status != http.StatusInternalServerError || !strings.Contains(body, "status_unavailable") || strings.Contains(body, "unsupported type") {
		t.Fatalf("unmarshalable status response = %d %q", status, body)
	}
}

func TestManagementAuthFailureLimitAndAudit(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	metrics := &Metrics{}
	server, err := Start("127.0.0.1:0", metrics, nil, []byte(testManagementToken), ServerOptions{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := &http.Client{Timeout: time.Second}
	url := "http://" + server.listener.Addr().String() + "/v1/status"
	for attempt := 0; attempt < managementAuthFailureLimit; attempt++ {
		request, requestErr := http.NewRequest(http.MethodGet, url, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.Header.Set("Authorization", "Bearer deliberately-invalid-management-token")
		response, requestErr := client.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("invalid attempt %d returned %d", attempt+1, response.StatusCode)
		}
	}
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer deliberately-invalid-management-token")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") == "" {
		t.Fatalf("rate-limited response = %d retry-after=%q", response.StatusCode, response.Header.Get("Retry-After"))
	}
	if response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("management security headers missing: %#v", response.Header)
	}
	if metrics.ManagementAuthFailures.Load() != managementAuthFailureLimit || metrics.ManagementAuthRateLimited.Load() != 1 {
		t.Fatalf("management auth metrics = failures %d limited %d", metrics.ManagementAuthFailures.Load(), metrics.ManagementAuthRateLimited.Load())
	}
	if !strings.Contains(logs.String(), "management.auth.rejected") || !strings.Contains(logs.String(), "management.auth.rate_limited") {
		t.Fatalf("management audit events missing: %s", logs.String())
	}
	if strings.Contains(logs.String(), "deliberately-invalid-management-token") {
		t.Fatalf("management token leaked into logs: %s", logs.String())
	}
	validRequest, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	validRequest.Header.Set("Authorization", "Bearer "+testManagementToken)
	validResponse, err := client.Do(validRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = validResponse.Body.Close()
	if validResponse.StatusCode != http.StatusOK {
		t.Fatalf("valid token should clear the cooldown, got %d", validResponse.StatusCode)
	}
}

func TestManagementServerProtectsSensitiveEndpoints(t *testing.T) {
	server, err := Start("127.0.0.1:0", nil, nil, []byte(testManagementToken))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := &http.Client{Timeout: time.Second}
	for _, path := range []string{"/metrics", "/v1/status"} {
		response, err := client.Get("http://" + server.listener.Addr().String() + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s returned %d", path, response.StatusCode)
		}
	}
	if _, err := Start("127.0.0.1:0", nil, nil, []byte("short")); err == nil {
		t.Fatal("short management token should fail startup")
	}
}

func TestManagementServerNilProviderAndInvalidListen(t *testing.T) {
	if _, err := Start("not-an-address", nil, nil, []byte(testManagementToken)); err == nil {
		t.Fatal("invalid management address should fail")
	}
	server, err := Start("127.0.0.1:0", nil, nil, []byte(testManagementToken))
	if err != nil {
		t.Fatal(err)
	}
	status, body := getManagement(t, server.listener.Addr().String(), "/v1/status")
	if status != http.StatusOK || strings.TrimSpace(body) != "{}" {
		t.Fatalf("nil provider status = %d %q", status, body)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal("close must be idempotent: ", err)
	}
	if err := (*Server)(nil).Close(); err != nil {
		t.Fatal(err)
	}
}

func getManagement(t *testing.T, address, path string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	url := "http://" + address + path
	var response *http.Response
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		request, requestErr := http.NewRequest(http.MethodGet, url, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.Header.Set("Authorization", "Bearer "+testManagementToken)
		response, err = client.Do(request)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}
