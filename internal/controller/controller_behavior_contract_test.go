package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"asterferry/internal/domain"
)

func TestObfuscationRotationPropagatesToAssignmentsContract(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, node := range []domain.Node{
		{ID: "gw", Name: "gateway", Enabled: true},
		{ID: "agent", Name: "agent", Enabled: true},
	} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	oldKey := []byte("01234567890123456789012345678901")
	newKey := []byte("abcdefghijklmnopqrstuvwxyz123456")
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{
		NodeID:          "gw",
		PublicEndpoints: []string{"gw.example:4433"},
		PortPool:        domain.PortPool{TCP: []domain.PortRange{{Min: 18080, Max: 18080}}},
		Obfuscation:     domain.ObfuscationPolicy{Mode: "camouflage", Key: oldKey},
	}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", PublicPort: 18080, Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	assignments, err := schedulerForTest(store).ScheduleAgent(ctx, "agent", WriteOptions{})
	if err != nil || len(assignments) != 1 {
		t.Fatalf("initial schedule = %#v, err=%v", assignments, err)
	}
	before := assignments[0]
	if before.Obfuscation.KeyID != obfuscationKeyID(oldKey) {
		t.Fatalf("initial assignment key id = %q, want %q", before.Obfuscation.KeyID, obfuscationKeyID(oldKey))
	}

	spec, err := store.GetGatewaySpec(ctx, "gw")
	if err != nil {
		t.Fatal(err)
	}
	spec.Obfuscation.Key = append([]byte(nil), newKey...)
	spec.Obfuscation.KeyCiphertext = nil
	spec.Obfuscation.KeyID = ""
	spec.Obfuscation.PreviousKey = append([]byte(nil), oldKey...)
	spec.Obfuscation.PreviousKeyCiphertext = nil
	spec.Obfuscation.PreviousKeyID = ""
	if err := store.PutGatewaySpec(ctx, spec, WriteOptions{IfMatch: spec.Revision}); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetAssignment(ctx, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation+1 {
		t.Fatalf("assignment generation = %d, want %d", after.Generation, before.Generation+1)
	}
	if after.Obfuscation.KeyID != obfuscationKeyID(newKey) {
		t.Fatalf("rotated assignment key id = %q, want %q", after.Obfuscation.KeyID, obfuscationKeyID(newKey))
	}
	if after.Obfuscation.PreviousKeyID != obfuscationKeyID(oldKey) {
		t.Fatalf("rotated assignment previous key id = %q, want %q", after.Obfuscation.PreviousKeyID, obfuscationKeyID(oldKey))
	}
	if len(after.Obfuscation.KeyCiphertext) == 0 || len(after.Obfuscation.Key) != 0 {
		t.Fatalf("rotated assignment contains invalid key representation: %#v", after.Obfuscation)
	}
	for _, nodeID := range []string{"gw", "agent"} {
		snapshot, snapshotErr := store.EnsureDesiredSnapshot(ctx, nodeID)
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		var document domain.DesiredSnapshot
		if err := json.Unmarshal(snapshot.Document, &document); err != nil {
			t.Fatal(err)
		}
		if len(document.Assignments) != 1 || document.Assignments[0].Obfuscation.KeyID != obfuscationKeyID(newKey) {
			t.Fatalf("%s snapshot retained old obfuscation policy: %#v", nodeID, document.Assignments)
		}
	}
}

func TestSchedulingPreservesDisjointAssignmentsContract(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, node := range []domain.Node{
		{ID: "gw-a", Name: "gateway-a", Enabled: true},
		{ID: "gw-b", Name: "gateway-b", Enabled: true},
		{ID: "agent", Name: "agent", Enabled: true},
	} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, gatewayID := range []string{"gw-a", "gw-b"} {
		if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: gatewayID, PublicEndpoints: []string{gatewayID + ":4433"}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 18080, Max: 18081}}}}, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, service := range []domain.Service{
		{ID: "svc-a", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", PublicPort: 18080, Enabled: true},
		{ID: "svc-b", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8081", PublicBind: "0.0.0.0", PublicPort: 18081, Enabled: true},
	} {
		if err := store.PutService(ctx, service, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, assignment := range []domain.Assignment{
		{ID: "agent-gw-a-svc-a", GatewayID: "gw-a", AgentID: "agent", ServiceIDs: []string{"svc-a"}, Bindings: []domain.Binding{{ServiceID: "svc-a", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}}, PublicEndpoint: "gw-a:4433", Generation: 1, State: domain.AssignmentPending},
		{ID: "agent-gw-b-svc-b", GatewayID: "gw-b", AgentID: "agent", ServiceIDs: []string{"svc-b"}, Bindings: []domain.Binding{{ServiceID: "svc-b", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18081}}, PublicEndpoint: "gw-b:4433", Generation: 1, State: domain.AssignmentPending},
	} {
		if err := store.PutAssignment(ctx, assignment, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	assignments, err := schedulerForTest(store).ScheduleAgent(ctx, "agent", WriteOptions{})
	if err != nil {
		t.Fatalf("rescheduling disjoint assignments failed: %v", err)
	}
	if len(assignments) != 2 {
		t.Fatalf("rescheduling returned %d assignments, want 2: %#v", len(assignments), assignments)
	}
	byService := make(map[string]string)
	for _, assignment := range assignments {
		for _, serviceID := range assignment.ServiceIDs {
			byService[serviceID] = assignment.GatewayID
		}
	}
	if byService["svc-a"] != "gw-a" || byService["svc-b"] != "gw-b" {
		t.Fatalf("disjoint placements were not preserved: %#v", byService)
	}
}

func TestOneTimeTokenRetryDoesNotReturnPlaintextContract(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	user, err := store.CreateUser(ctx, "token-owner", "a-very-long-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	plain, first, err := store.CreateAPITokenWithOptions(ctx, user.ID, "retryable", nil, WriteOptions{IdempotencyKey: "api-token-once"})
	if err != nil || plain == "" {
		t.Fatalf("first API token = %q, %#v, err=%v", plain, first, err)
	}
	retryPlain, retry, err := store.CreateAPITokenWithOptions(ctx, user.ID, "retryable", nil, WriteOptions{IdempotencyKey: "api-token-once"})
	if !errors.Is(err, ErrSecretAlreadyCreated) || retryPlain != "" || retry.ID != first.ID {
		t.Fatalf("API token retry = %q, %#v, err=%v", retryPlain, retry, err)
	}

	enrollmentPlain, firstEnrollment, err := store.CreateEnrollmentTokenWithOptions(ctx, time.Minute, WriteOptions{IdempotencyKey: "enrollment-token-once"})
	if err != nil || enrollmentPlain == "" {
		t.Fatalf("first enrollment token = %q, %#v, err=%v", enrollmentPlain, firstEnrollment, err)
	}
	retryEnrollmentPlain, retryEnrollment, err := store.CreateEnrollmentTokenWithOptions(ctx, time.Minute, WriteOptions{IdempotencyKey: "enrollment-token-once"})
	if !errors.Is(err, ErrSecretAlreadyCreated) || retryEnrollmentPlain != "" || retryEnrollment.ID != firstEnrollment.ID {
		t.Fatalf("enrollment token retry = %q, %#v, err=%v", retryEnrollmentPlain, retryEnrollment, err)
	}
}

func TestTokenRetryAPIReportsConflictMetadataContract(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	user, err := store.CreateUser(ctx, "api-admin", "a-very-long-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	authToken, _, err := store.CreateAPIToken(ctx, user.ID, "admin-auth", nil)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, sessions: sync.Map{}, metrics: newControllerMetrics()}
	request := func(path, body, idempotencyKey string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idempotencyKey)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, req)
		return recorder
	}
	if response := request("/api/v1/users/"+user.ID+"/tokens", `{"name":"api-retry"}`, "api-handler-once"); response.Code != http.StatusCreated {
		t.Fatalf("first API token response = %d, body=%s", response.Code, response.Body.String())
	}
	response := request("/api/v1/users/"+user.ID+"/tokens", `{"name":"api-retry"}`, "api-handler-once")
	assertAlreadyCreatedResponse(t, response, "metadata")
	if response := request("/api/v1/enrollment-tokens", `{"ttl_seconds":60}`, "enrollment-handler-once"); response.Code != http.StatusCreated {
		t.Fatalf("first enrollment response = %d, body=%s", response.Code, response.Body.String())
	}
	response = request("/api/v1/enrollment-tokens", `{"ttl_seconds":60}`, "enrollment-handler-once")
	assertAlreadyCreatedResponse(t, response, "token_metadata")
}

func assertAlreadyCreatedResponse(t *testing.T, response *httptest.ResponseRecorder, metadataField string) {
	t.Helper()
	if response.Code != http.StatusConflict {
		t.Fatalf("retry response = %d, body=%s", response.Code, response.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode retry response: %v; body=%s", err, response.Body.String())
	}
	if _, exists := value["token"]; exists {
		t.Fatalf("retry response exposed a token field: %s", response.Body.String())
	}
	if recoverable, ok := value["token_recoverable"].(bool); !ok || recoverable {
		t.Fatalf("token_recoverable = %#v, want false", value["token_recoverable"])
	}
	if _, ok := value[metadataField]; !ok {
		t.Fatalf("retry response omitted %q: %s", metadataField, response.Body.String())
	}
	if errorValue, ok := value["error"].(map[string]any); !ok || errorValue["code"] != "already_created" {
		t.Fatalf("retry error = %#v", value["error"])
	}
}

func TestObservedHeartbeatAcceptsOlderGenerationContract(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "agent", Name: "agent", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureDesiredSnapshot(ctx, "agent"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	makeRecord := func(generation uint64) ObservedRecord {
		document, marshalErr := json.Marshal(domain.ObservedState{SchemaVersion: domain.CurrentControlProtocolVersion, NodeID: "agent", AppliedGeneration: generation, Healthy: true, ObservedAt: now})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return ObservedRecord{NodeID: "agent", Generation: generation, Document: document, UpdatedAt: now}
	}
	if err := store.SaveObserved(ctx, makeRecord(1)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveObserved(ctx, makeRecord(0)); !IsRevisionConflict(err) {
		t.Fatalf("strict lower-generation write = %v, want revision conflict", err)
	}
	if err := store.SaveObservedHeartbeat(ctx, makeRecord(0)); err != nil {
		t.Fatalf("heartbeat lower-generation write failed: %v", err)
	}
	record, err := store.LoadObserved(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if record.Generation != 0 {
		t.Fatalf("stored heartbeat generation = %d, want 0", record.Generation)
	}
}

func TestLoginAndLogoutCookiesRemainSecureContract(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.CreateUser(context.Background(), "cookie-user", "a-very-long-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, sessions: sync.Map{}, loginLimiter: newLoginLimiter()}
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"cookie-user","password":"a-very-long-password"}`))
	loginRequest.Header.Set("Content-Type", "application/json")
	loginResponse := httptest.NewRecorder()
	server.login(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	cookies := loginResponse.Result().Cookies()
	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == "af_session" {
			value := *cookie
			sessionCookie = &value
		}
		if cookie.Name == "af_csrf" {
			value := *cookie
			csrfCookie = &value
		}
		if !cookie.Secure {
			t.Fatalf("login cookie %q was not Secure", cookie.Name)
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatal("login did not issue both session cookies")
	}
	logoutRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logoutRequest.AddCookie(sessionCookie)
	logoutRequest.AddCookie(csrfCookie)
	logoutRequest.Header.Set("X-CSRF-Token", csrfCookie.Value)
	logoutResponse := httptest.NewRecorder()
	server.logout(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, body=%s", logoutResponse.Code, logoutResponse.Body.String())
	}
	for _, cookie := range logoutResponse.Result().Cookies() {
		if !cookie.Secure {
			t.Fatalf("logout cookie %q was not Secure", cookie.Name)
		}
	}
	_ = user
}
