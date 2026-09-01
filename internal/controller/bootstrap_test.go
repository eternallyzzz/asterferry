package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"asterferry/internal/domain"
)

func TestNodeBootstrapCommandIncludesReleaseAndEnrollmentInputs(t *testing.T) {
	dir := t.TempDir()
	config := DefaultConfig(dir)
	config.GRPCAdvertise = "controller.example.com:9443"
	config.ReleaseBaseURL = "https://mirror.example.com/asterferry/releases/download"
	config.ReleaseVersion = "1.2.3"
	caPEM := []byte("-----BEGIN CERTIFICATE-----\ncontroller-ca\n-----END CERTIFICATE-----\n")
	if err := os.MkdirAll(filepath.Dir(config.CACertPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.CACertPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	node := domain.Node{ID: "gw-public", Role: domain.RoleGateway}

	linux, err := buildNodeInstallCommand(config, node, "linux", "amd64", "afn_token", caPEM)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"https://mirror.example.com/asterferry/releases/download/v1.2.3/install-node.sh",
		"--role 'gateway'",
		"--node-id 'gw-public'",
		"--controller 'controller.example.com:9443'",
		"--token 'afn_token'",
		"--version '1.2.3'",
		"--arch 'amd64'",
	} {
		if !strings.Contains(linux.Command, want) {
			t.Fatalf("Linux install command omitted %q: %s", want, linux.Command)
		}
	}
	if !strings.Contains(linux.Command, "--ca-pem-b64 '") {
		t.Fatalf("Linux install command omitted embedded CA: %s", linux.Command)
	}

	windows, err := buildNodeInstallCommand(config, node, "windows", "amd64", "afn_token", caPEM)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"install-node.ps1",
		"-Role 'gateway'",
		"-NodeId 'gw-public'",
		"-Controller 'controller.example.com:9443'",
		"-Token 'afn_token'",
		"-Version '1.2.3'",
		"-Arch 'amd64'",
	} {
		if !strings.Contains(windows.Command, want) {
			t.Fatalf("Windows install command omitted %q: %s", want, windows.Command)
		}
	}
}

func TestNodeBootstrapConfigurationRejectsUnsafeReleaseSource(t *testing.T) {
	dir := t.TempDir()
	config := DefaultConfig(dir)
	config.GRPCAdvertise = "controller.example.com:9443"
	config.ReleaseVersion = "1.2.3"
	config.ReleaseBaseURL = "http://mirror.example.com/releases"
	if err := os.MkdirAll(filepath.Dir(config.CACertPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.CACertPath, []byte("ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateBootstrapConfiguration(config); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("unsafe release source error = %v", err)
	}
}

func TestNodeBootstrapConfigurationRejectsUnspecifiedControllerAddress(t *testing.T) {
	dir := t.TempDir()
	config := DefaultConfig(dir)
	config.GRPCAdvertise = "0.0.0.0:9443"
	config.ReleaseVersion = "1.2.3"
	if err := os.MkdirAll(filepath.Dir(config.CACertPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.CACertPath, []byte("ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateBootstrapConfiguration(config); err == nil || !strings.Contains(err.Error(), "reachable host") {
		t.Fatalf("unspecified Controller address error = %v", err)
	}
}

func TestNodeEnrollmentTokenIsBoundToNode(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, node := range []domain.Node{
		{ID: "agent-a", Role: domain.RoleAgent, Name: "agent-a", Enabled: true},
		{ID: "agent-b", Role: domain.RoleAgent, Name: "agent-b", Enabled: true},
	} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	plain, _, err := store.CreateNodeEnrollmentToken(ctx, "agent-a", domain.RoleAgent, EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	boundNode, bound, err := parseNodeEnrollmentToken(plain)
	if err != nil || !bound || boundNode != "agent-a" {
		t.Fatalf("parsed node binding = %q, %v, %v", boundNode, bound, err)
	}
	if _, err := store.IssueNodeCertificate(ctx, Config{}, plain, domain.RoleAgent, "agent-b", nil); !errors.Is(err, ErrEnrollmentNodeMismatch) {
		t.Fatalf("cross-node enrollment error = %v, want ErrEnrollmentNodeMismatch", err)
	}
}

func TestNodeBootstrapEndpointCreatesSpecAndOneTimeCommand(t *testing.T) {
	dir := t.TempDir()
	config := DefaultConfig(dir)
	config.GRPCAdvertise = "controller.example.com:9443"
	config.ReleaseVersion = "1.2.3"
	if err := os.MkdirAll(filepath.Dir(config.CACertPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.CACertPath, []byte("ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openTestStore(filepath.Join(dir, "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "gw", Role: domain.RoleGateway, Name: "Gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	admin, err := store.CreateUser(ctx, "bootstrap-admin", "a-very-long-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	authToken, _, err := store.CreateAPIToken(ctx, admin.ID, "bootstrap-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	body, err := json.Marshal(NodeBootstrapRequest{
		Platform: "linux",
		Arch:     "amd64",
		GatewaySpec: &domain.GatewaySpec{
			NodeID:          "gw",
			PublicEndpoints: []string{"gateway.example.com:4433"},
			PortPool:        domain.PortPool{TCP: []domain.PortRange{{Min: 28080, Max: 28999}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/gw/bootstrap", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+authToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "bootstrap-command-once")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("bootstrap response = %d, body=%s", response.Code, response.Body.String())
	}
	var result NodeBootstrapResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.NodeID != "gw" || result.Role != domain.RoleGateway || result.Command == "" || result.ExpiresAt == "" {
		t.Fatalf("bootstrap response = %#v", result)
	}
	if _, err := store.GetGatewaySpec(ctx, "gw"); err != nil {
		t.Fatalf("bootstrap did not persist gateway spec: %v", err)
	}

	retry := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/gw/bootstrap", bytes.NewReader(body))
	retry.Header.Set("Authorization", "Bearer "+authToken)
	retry.Header.Set("Content-Type", "application/json")
	retry.Header.Set("Idempotency-Key", "bootstrap-command-once")
	retryResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(retryResponse, retry)
	if retryResponse.Code != http.StatusConflict || strings.Contains(retryResponse.Body.String(), `"command"`) {
		t.Fatalf("bootstrap retry = %d, body=%s", retryResponse.Code, retryResponse.Body.String())
	}
}

func TestReconcileAssignmentsForAgentsSchedulesNewServicesWithoutStaleIdempotency(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, node := range []domain.Node{
		{ID: "gw", Role: domain.RoleGateway, Name: "gateway", Enabled: true},
		{ID: "agent", Role: domain.RoleAgent, Name: "agent", Enabled: true},
	} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gw", PublicEndpoints: []string{"gw.example.com:4433"}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 28080, Max: 28999}}}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, service := range []domain.Service{
		{ID: "svc-a", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", PublicPort: 28080, Enabled: true},
	} {
		if err := store.PutService(ctx, service, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if assignments, err := store.ReconcileAssignmentsForAgents(ctx, "agent"); err != nil || len(assignments) != 1 {
		t.Fatalf("first reconciliation = %#v, err=%v", assignments, err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "svc-b", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8081", PublicBind: "0.0.0.0", PublicPort: 28081, Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	assignments, err := store.ReconcileAssignmentsForAgents(ctx, "agent")
	if err != nil {
		t.Fatalf("second reconciliation failed: %v", err)
	}
	if len(assignments) != 2 {
		t.Fatalf("second reconciliation = %#v, want two assignments", assignments)
	}
	serviceCount := 0
	for _, assignment := range assignments {
		serviceCount += len(assignment.ServiceIDs)
	}
	if serviceCount != 2 {
		t.Fatalf("second reconciliation assigned %d services, want 2: %#v", serviceCount, assignments)
	}
}
