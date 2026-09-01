package controller

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"asterferry/internal/domain"
	nodepkg "asterferry/internal/node"
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

func TestNodeInstallationCreatesIdentityOnlyAfterEnrollment(t *testing.T) {
	root := t.TempDir()
	initResult, err := Init(context.Background(), InitOptions{
		Dir: root, Password: "a-very-long-admin-password",
		GRPCAdvertise: "controller.example.com:9443", ReleaseVersion: "1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := LoadOrCreateMasterKey(initResult.Config.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(initResult.Config.DatabasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	adminToken, _, err := store.CreateAPIToken(context.Background(), initResult.Admin.ID, "bootstrap-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(initResult.Config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	body, err := json.Marshal(NodeInstallationRequest{
		NodeID: "gw-install", Role: domain.RoleGateway, Name: "Gateway install", Labels: map[string]string{"site": "east"},
		Platform: "linux", Arch: "amd64",
		GatewaySpec: &domain.GatewaySpec{NodeID: "gw-install", PublicEndpoints: []string{"gateway.example.com:4433"}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 28080, Max: 28999}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/node-installations", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "pending-install-once")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("pending installation response = %d, body=%s", response.Code, response.Body.String())
	}
	var result NodeBootstrapResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.State != "pending" || result.InstallationID != "gw-install" || result.Command == "" {
		t.Fatalf("pending installation response = %#v", result)
	}
	if _, err := store.GetNode(context.Background(), "gw-install"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("node exists before enrollment: %v", err)
	}
	if _, err := store.GetGatewaySpec(context.Background(), "gw-install"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("gateway spec exists before enrollment: %v", err)
	}
	if _, err := store.GetPendingNodeBootstrap(context.Background(), "gw-install"); err != nil {
		t.Fatalf("pending bootstrap was not stored: %v", err)
	}
	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/node-installations", nil)
	listRequest.Header.Set("Authorization", "Bearer "+adminToken)
	listResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || strings.Contains(listResponse.Body.String(), "token_hash") || strings.Contains(listResponse.Body.String(), "command") {
		t.Fatalf("pending installation list = %d, body=%s", listResponse.Code, listResponse.Body.String())
	}

	const tokenMarker = "--token '"
	start := strings.Index(result.Command, tokenMarker)
	if start < 0 {
		t.Fatalf("install command has no token: %s", result.Command)
	}
	start += len(tokenMarker)
	end := strings.Index(result.Command[start:], "'")
	if end < 0 {
		t.Fatalf("install command token is unterminated: %s", result.Command)
	}
	originalToken := result.Command[start : start+end]
	retry := httptest.NewRequest(http.MethodPost, "/api/v1/node-installations", bytes.NewReader(body))
	retry.Header.Set("Authorization", "Bearer "+adminToken)
	retry.Header.Set("Content-Type", "application/json")
	retry.Header.Set("Idempotency-Key", "pending-install-once")
	retryResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(retryResponse, retry)
	if retryResponse.Code != http.StatusConflict || strings.Contains(retryResponse.Body.String(), `"command"`) {
		t.Fatalf("pending installation retry = %d, body=%s", retryResponse.Code, retryResponse.Body.String())
	}
	reissue := httptest.NewRequest(http.MethodPost, "/api/v1/node-installations/gw-install/reissue", nil)
	reissue.Header.Set("Authorization", "Bearer "+adminToken)
	reissue.Header.Set("Idempotency-Key", "pending-install-reissue")
	reissueResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(reissueResponse, reissue)
	if reissueResponse.Code != http.StatusOK {
		t.Fatalf("pending installation reissue = %d, body=%s", reissueResponse.Code, reissueResponse.Body.String())
	}
	var replacement NodeBootstrapResponse
	if err := json.Unmarshal(reissueResponse.Body.Bytes(), &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.State != "pending" || replacement.Command == result.Command {
		t.Fatalf("pending installation replacement = %#v", replacement)
	}
	start = strings.Index(replacement.Command, tokenMarker)
	if start < 0 {
		t.Fatalf("replacement command has no token: %s", replacement.Command)
	}
	start += len(tokenMarker)
	end = strings.Index(replacement.Command[start:], "'")
	if end < 0 {
		t.Fatalf("replacement command token is unterminated: %s", replacement.Command)
	}
	token := replacement.Command[start : start+end]
	if token == originalToken {
		t.Fatal("reissued installation reused the previous one-time token")
	}
	csr, _, err := nodepkg.GenerateCSR("gw-install", domain.RoleGateway)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueNodeCertificate(context.Background(), initResult.Config, token, domain.RoleGateway, "gw-install", csr); err != nil {
		t.Fatalf("enroll pending node: %v", err)
	}
	node, err := store.GetNode(context.Background(), "gw-install")
	if err != nil {
		t.Fatal(err)
	}
	if node.CertificateState != domain.CertificateActive || node.CertificateSerial == "" {
		t.Fatalf("enrolled node identity = %#v", node)
	}
	if _, err := store.GetPendingNodeBootstrap(context.Background(), "gw-install"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("pending bootstrap remained after enrollment: %v", err)
	}
	if spec, err := store.GetGatewaySpec(context.Background(), "gw-install"); err != nil || len(spec.PublicEndpoints) != 1 {
		t.Fatalf("enrolled gateway spec = %#v, err=%v", spec, err)
	}

	// The supported install-first path is behavior-neutral. A Node identity is
	// created only after enrollment, and the command must not smuggle a role
	// hint into the daemon; behavior is selected later through NodeSpec.
	genericBody, err := json.Marshal(NodeInstallationRequest{
		NodeID: "generic-install", Name: "Generic install", Platform: "linux", Arch: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	genericRequest := httptest.NewRequest(http.MethodPost, "/api/v1/node-installations", bytes.NewReader(genericBody))
	genericRequest.Header.Set("Authorization", "Bearer "+adminToken)
	genericRequest.Header.Set("Content-Type", "application/json")
	genericRequest.Header.Set("Idempotency-Key", "generic-install-once")
	genericResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(genericResponse, genericRequest)
	if genericResponse.Code != http.StatusCreated {
		t.Fatalf("generic installation response = %d, body=%s", genericResponse.Code, genericResponse.Body.String())
	}
	var genericResult NodeBootstrapResponse
	if err := json.Unmarshal(genericResponse.Body.Bytes(), &genericResult); err != nil {
		t.Fatal(err)
	}
	if genericResult.Role != "" || strings.Contains(genericResult.Command, "--role") || strings.Contains(genericResult.Command, "-Role") {
		t.Fatalf("generic installation command selected a behavior: %#v", genericResult)
	}
	if _, err := store.GetNode(context.Background(), "generic-install"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("generic node exists before enrollment: %v", err)
	}
}

func TestDeleteNodeAllowsUnusedOwnedSpec(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "unused-gateway", Role: domain.RoleGateway, Name: "unused", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "unused-gateway", PublicEndpoints: []string{"gateway.example.com:4433"}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteNode(ctx, "unused-gateway", WriteOptions{IfMatch: 1}); err != nil {
		t.Fatalf("delete unused node with owned spec: %v", err)
	}
	if _, err := store.GetGatewaySpec(ctx, "unused-gateway"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("owned gateway spec remained after node deletion: %v", err)
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
