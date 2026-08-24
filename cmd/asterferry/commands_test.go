package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"asterferry/internal/bootstrap"
	"asterferry/internal/config"
	"asterferry/internal/diagnostics"
	"gopkg.in/yaml.v3"
)

const testStatusToken = "test-management-token-01234567890123456789"

func TestRootHelpDoesNotReadConfiguration(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand(&out, &out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Available Commands:") || !strings.Contains(out.String(), "init") {
		t.Fatalf("help output is incomplete: %s", out.String())
	}
}

func TestInitRequiresExplicitProfile(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand(&out, &out)
	root.SetArgs([]string{"init", "--dir", filepath.Join(t.TempDir(), "bundle")})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--profile is required") {
		t.Fatalf("unexpected init error: %v", err)
	}
	if exitCode(err) != 2 {
		t.Fatalf("init usage error code = %d", exitCode(err))
	}
}

func TestCommandOutputAndDiagnostics(t *testing.T) {
	var version bytes.Buffer
	root := newRootCommand(&version, &version)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(version.String(), "protocol:") {
		t.Fatalf("version output is incomplete: %s", version.String())
	}
	var completion bytes.Buffer
	root = newRootCommand(&completion, &completion)
	root.SetArgs([]string{"completion", "bash"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(completion.String(), "__start_asterferry") {
		t.Fatalf("completion output is incomplete: %s", completion.String())
	}

	report := diagnostics.Report{Role: "gateway", Findings: []diagnostics.Finding{{Severity: diagnostics.SeverityWarn, Code: "test.warning", Path: "field", Message: "warning", Hint: "fix it"}}}
	var human bytes.Buffer
	if err := writeDiagnosticReport(&human, report, false); err != nil || !strings.Contains(human.String(), "WARNING") {
		t.Fatalf("human diagnostics output = %q, err=%v", human.String(), err)
	}
	var encoded bytes.Buffer
	if err := writeDiagnosticReport(&encoded, report, true); err != nil || !strings.Contains(encoded.String(), "test.warning") {
		t.Fatalf("JSON diagnostics output = %q, err=%v", encoded.String(), err)
	}
}

func TestInitDoctorAndRuntimeCommandFailures(t *testing.T) {
	rootDir := t.TempDir()
	bundleDir := filepath.Join(rootDir, "bundle")
	var output bytes.Buffer
	root := newRootCommand(&output, &output)
	root.SetArgs([]string{"init", "--dir", bundleDir, "--profile", "dev"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	root = newRootCommand(&output, &output)
	root.SetArgs([]string{"doctor", "--config", filepath.Join(bundleDir, "config", "gateway.yaml"), "--skip-ports"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	prodDir := filepath.Join(rootDir, "prod")
	if _, err := bootstrap.Generate(bootstrap.Options{Dir: prodDir, Profile: bootstrap.ProfileProd}); err != nil {
		t.Fatal(err)
	}
	root = newRootCommand(&output, &output)
	root.SetArgs([]string{"gateway", "--config", filepath.Join(prodDir, "config", "gateway.yaml")})
	if err := root.Execute(); err == nil {
		t.Fatal("gateway with unsigned production material should fail")
	}
	root = newRootCommand(&output, &output)
	root.SetArgs([]string{"agent", "--config", filepath.Join(prodDir, "config", "agent.yaml")})
	if err := root.Execute(); err == nil {
		t.Fatal("agent with unsigned production material should fail")
	}
}

func TestStartupSummariesDoNotRequireRuntime(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	logGatewayStarted(logger, "gateway.yaml", &config.GatewayOptions{Gateway: config.GatewayConfig{Listen: "127.0.0.1:4433"}, Management: config.ManagementConfig{Listen: "127.0.0.1:9090"}, Agents: []config.GatewayAgentOptions{{}}})
	logAgentStarted(logger, "agent.yaml", &config.AgentOptions{Agent: config.AgentRuntime{ID: "edge-a", Server: "127.0.0.1:4433"}, Management: config.ManagementConfig{Listen: "127.0.0.1:9091"}})
	if !strings.Contains(output.String(), "runtime.started") || !strings.Contains(output.String(), "edge-a") {
		t.Fatalf("startup summaries missing: %s", output.String())
	}
}

func TestQueryStatusHumanAndJSON(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := bootstrap.Generate(bootstrap.Options{Dir: root, Profile: bootstrap.ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testStatusToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"gateway","ready":true,"agents":1}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	c, err := config.Load(result.GatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Management.AuthTokenFile, []byte(testStatusToken), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Management.Listen = listener.Addr().String()
	data, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result.GatewayConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var human bytes.Buffer
	if err := queryStatus(&human, result.GatewayConfig, false, time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "mode: gateway") || !strings.Contains(human.String(), "ready: true") {
		t.Fatalf("unexpected human status: %s", human.String())
	}
	var formatted bytes.Buffer
	if err := queryStatus(&formatted, result.GatewayConfig, true, time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(formatted.String(), "\"agents\": 1") {
		t.Fatalf("unexpected JSON status: %s", formatted.String())
	}
}

func TestQueryStatusUnauthorized(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := bootstrap.Generate(bootstrap.Options{Dir: root, Profile: bootstrap.ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(result.GatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Management.AuthTokenFile, []byte(testStatusToken), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	c.Management.Listen = listener.Addr().String()
	data, _ := yaml.Marshal(c)
	if err := os.WriteFile(result.GatewayConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = queryStatus(&bytes.Buffer{}, result.GatewayConfig, false, time.Second)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("unexpected unauthorized error: %v", err)
	}
}

func TestRuntimeConfigErrorIncludesDoctorHint(t *testing.T) {
	err := runtimeConfigError(fmt.Errorf("missing token"), "agent.yaml")
	if !strings.Contains(err.Error(), "asterferry doctor --config \"agent.yaml\"") {
		t.Fatalf("missing doctor hint: %v", err)
	}
}

func TestHealthcheckCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusNoContent)
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	var output bytes.Buffer
	if err := runHealthcheck(&output, server.URL+"/ok", time.Second); err != nil {
		t.Fatal(err)
	}
	if output.String() != "ok\n" {
		t.Fatalf("healthcheck output = %q", output.String())
	}
	if err := runHealthcheck(&bytes.Buffer{}, server.URL+"/unready", time.Second); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("unready healthcheck error = %v", err)
	}
	if err := runHealthcheck(&bytes.Buffer{}, server.URL+"/redirect", time.Second); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect healthcheck error = %v", err)
	}
	for _, value := range []string{"", "127.0.0.1:9090/healthz", "ftp://127.0.0.1/healthz", "http://user:pass@127.0.0.1/healthz"} {
		if healthcheckURLIsSafe(value) {
			t.Fatalf("unsafe healthcheck URL accepted: %q", value)
		}
	}
}
