package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigMetricsListenDefaultAndLegacyLoad(t *testing.T) {
	root := filepath.Join(t.TempDir(), "controller")
	defaults := DefaultConfig(root)
	if defaults.MetricsListen != "127.0.0.1:9090" {
		t.Fatalf("default metrics listen = %q", defaults.MetricsListen)
	}

	encoded, err := json.Marshal(defaults)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "metrics_listen")
	encoded, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "controller.json")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MetricsListen != defaults.MetricsListen {
		t.Fatalf("legacy config metrics listen = %q, want %q", loaded.MetricsListen, defaults.MetricsListen)
	}
}

func TestConfigMetricsListenCanBeDisabledAndMustBeValid(t *testing.T) {
	config := DefaultConfig(t.TempDir())
	config.MetricsListen = ""
	if err := config.Validate(); err != nil {
		t.Fatalf("disabled metrics endpoint rejected: %v", err)
	}
	config.MetricsListen = "metrics without port"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "metrics_listen") {
		t.Fatalf("invalid metrics endpoint error = %v", err)
	}
}

func TestMetricsListenerIsAnonymousAndManagementMetricsRemainAuthenticated(t *testing.T) {
	repositories, err := openTestRepositories(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(t.TempDir())
	config.HTTPListen = "127.0.0.1:0"
	config.MetricsListen = "127.0.0.1:0"
	server, err := NewServer(config, repositories)
	if err != nil {
		_ = repositories.Close()
		t.Fatal(err)
	}
	defer func() {
		_ = server.Close()
		_ = repositories.Close()
	}()

	metrics := httptest.NewRecorder()
	server.metricsOnlyHandler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK {
		t.Fatalf("anonymous internal metrics status = %d, want 200", metrics.Code)
	}
	if metrics.Body.Len() == 0 {
		t.Fatal("anonymous internal metrics response is empty")
	}
	listener, err := server.MetricsListener()
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ServeMetrics(listener) }()
	response, err := http.Get("http://" + listener.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("served internal metrics status = %d, want 200", response.StatusCode)
	}
	_ = response.Body.Close()

	notMetrics := httptest.NewRecorder()
	server.metricsOnlyHandler().ServeHTTP(notMetrics, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notMetrics.Code != http.StatusNotFound {
		t.Fatalf("internal metrics /readyz status = %d, want 404", notMetrics.Code)
	}

	management := httptest.NewRecorder()
	server.Handler().ServeHTTP(management, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if management.Code != http.StatusUnauthorized {
		t.Fatalf("management /metrics status = %d, want 401", management.Code)
	}
	_ = server.Close()
	if err := <-serveErr; err != nil && err != http.ErrServerClosed {
		t.Fatalf("metrics server close error = %v", err)
	}
}

func TestDisabledMetricsListenerDoesNotBind(t *testing.T) {
	repositories, err := openTestRepositories(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(t.TempDir())
	config.MetricsListen = ""
	server, err := NewServer(config, repositories)
	if err != nil {
		_ = repositories.Close()
		t.Fatal(err)
	}
	defer func() {
		_ = server.Close()
		_ = repositories.Close()
	}()
	listener, err := server.MetricsListener()
	if err != nil {
		t.Fatal(err)
	}
	if listener != nil {
		_ = listener.Close()
		t.Fatal("disabled metrics endpoint returned a listener")
	}
}
