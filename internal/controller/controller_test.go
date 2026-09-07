package controller

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"asterferry/internal/domain"
)

func TestStoreRevisionAndAtomicSnapshot(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateNode(ctx, domain.Node{ID: "gw-1", Name: "gateway", Enabled: true, Labels: map[string]string{"region": "east"}, CreatedAt: now}, WriteOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateNode(ctx, domain.Node{ID: "idempotent", Name: "idempotent", Enabled: true}, WriteOptions{Actor: "test", IdempotencyKey: "create-once"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateNode(ctx, domain.Node{ID: "idempotent", Name: "idempotent", Enabled: true}, WriteOptions{Actor: "test", IdempotencyKey: "create-once"}); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if err := store.CreateNode(ctx, domain.Node{ID: "agent-1", Name: "agent", Enabled: true}, WriteOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutNodeSpec(ctx, domain.NewAgentNodeSpec(domain.AgentSpec{NodeID: "agent-1"}), WriteOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	service := domain.Service{ID: "svc-1", AgentID: "agent-1", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", PublicPort: 0, Enabled: true}
	if err := store.PutService(ctx, service, WriteOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutNodeSpec(ctx, domain.NewGatewayNodeSpec(domain.GatewaySpec{NodeID: "gw-1", PublicEndpoints: []string{"gw.example:4433"}}), WriteOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	assignment := domain.Assignment{ID: "as-1", GatewayID: "gw-1", AgentID: "agent-1", ServiceIDs: []string{"svc-1"}, Bindings: []domain.Binding{{ServiceID: "svc-1", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}}, Generation: 1, State: domain.AssignmentPending}
	if err := store.PutAssignment(ctx, assignment, WriteOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureDesiredSnapshot(ctx, "gw-1"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadSnapshot(ctx, "gw-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Generation != 1 || loaded.Checksum == "" {
		t.Fatalf("unexpected snapshot: %#v", loaded)
	}
	updated := domain.Node{ID: "gw-1", Name: "gateway-updated", Enabled: true}
	if err := store.UpdateNode(ctx, updated, WriteOptions{IfMatch: 1, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateNode(ctx, updated, WriteOptions{IfMatch: 1, Actor: "test"}); !IsRevisionConflict(err) {
		t.Fatalf("stale node error = %v", err)
	}
	if _, err := store.GetAssignment(ctx, "as-1"); err != nil {
		t.Fatal(err)
	}
}

func TestCryptoAndEnrollmentToken(t *testing.T) {
	hash, err := HashPassword("a-very-long-admin-password")
	if err != nil || !VerifyPassword(hash, "a-very-long-admin-password") || VerifyPassword(hash, "wrong-password") {
		t.Fatalf("password crypto failed: %v", err)
	}
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateNode(context.Background(), domain.Node{ID: "agent-1", Name: "agent", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateEnrollmentToken(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.consumeEnrollmentToken(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := store.consumeEnrollmentToken(context.Background(), token); err == nil {
		t.Fatal("token reuse accepted")
	}
	key := []byte("01234567890123456789012345678901")
	ciphertext, err := EncryptSecret(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := DecryptSecret(key, ciphertext)
	if err != nil || string(plaintext) != "secret" {
		t.Fatalf("secret crypto failed: %v", err)
	}
	if _, err := DecryptSecret(key, append(ciphertext, 1)); err == nil {
		t.Fatal("tampered secret accepted")
	}
}

func TestUserRevisionAndIdempotency(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	user, err := store.CreateUserWithOptions(ctx, "operator", "a-very-long-password", RoleOperator, WriteOptions{IdempotencyKey: "user-create"})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.CreateUserWithOptions(ctx, "operator", "a-very-long-password", RoleOperator, WriteOptions{IdempotencyKey: "user-create"})
	if err != nil || retry.ID != user.ID {
		t.Fatalf("idempotent user create failed: %#v %v", retry, err)
	}
	enabled := false
	updated, err := store.UpdateUser(ctx, user.ID, UserUpdate{Enabled: &enabled}, WriteOptions{IfMatch: user.Revision})
	if err != nil || updated.Revision != user.Revision+1 {
		t.Fatalf("user update failed: %#v %v", updated, err)
	}
	if _, err := store.UpdateUser(ctx, user.ID, UserUpdate{Enabled: &enabled}, WriteOptions{IfMatch: user.Revision}); !IsRevisionConflict(err) {
		t.Fatalf("stale user update error = %v", err)
	}
}

func TestSnapshotPersistenceRejectsStaleOrConflictingGeneration(t *testing.T) {
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
	snapshot, err := store.BuildDesiredSnapshot(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(snapshot)
	record := SnapshotRecord{NodeID: snapshot.NodeID, Generation: snapshot.Generation, Checksum: snapshot.Checksum, Document: data}
	if err := store.SaveSnapshot(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, record); err != nil {
		t.Fatalf("same snapshot retry should be idempotent: %v", err)
	}
	changed := snapshot
	changed.Agent.Logging.Level = "debug"
	changed, err = changed.WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	changedData, _ := json.Marshal(changed)
	if err := store.SaveSnapshot(ctx, SnapshotRecord{NodeID: changed.NodeID, Generation: changed.Generation, Checksum: changed.Checksum, Document: changedData}); !IsRevisionConflict(err) {
		t.Fatalf("same-generation replacement was accepted: %v", err)
	}
}

func TestScheduleAgentUpdatesBothNodeSnapshots(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "gw", Name: "gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateNode(ctx, domain.Node{ID: "agent", Name: "agent", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gw", PublicEndpoints: []string{"gw.example:4433"}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 18080, Max: 18081}}}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	assignments, err := schedulerForTest(store).ScheduleAgent(ctx, "agent", WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected one assignment, got %d", len(assignments))
	}
	assignment := assignments[0]
	if assignment.PublicEndpoint != "gw.example:4433" || assignment.Bindings[0].Port != 18080 {
		t.Fatalf("unexpected assignment: %#v", assignment)
	}
	gwSnapshot, err := store.GetSnapshot(ctx, "gw")
	if err != nil {
		t.Fatal(err)
	}
	agentSnapshot, err := store.GetSnapshot(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if gwSnapshot.Generation != agentSnapshot.Generation || len(gwSnapshot.Assignments) != 1 || len(agentSnapshot.Assignments) != 1 {
		t.Fatalf("snapshots are not materialized consistently: gw=%#v agent=%#v", gwSnapshot, agentSnapshot)
	}
}

func TestAssignmentRequiresBothNodeAcks(t *testing.T) {
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
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gw", PublicEndpoints: []string{"gw.example:4433"}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	assignment := domain.Assignment{ID: "assignment", GatewayID: "gw", AgentID: "agent", ServiceIDs: []string{"svc"}, Bindings: []domain.Binding{{ServiceID: "svc", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}}, Generation: 1, State: domain.AssignmentPending}
	if err := store.PutAssignment(ctx, assignment, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.applyNodeResult(ctx, "gw", 1, true, "test"); err != nil || len(changed) != 0 {
		t.Fatalf("one-sided acknowledgement changed assignment: changed=%#v err=%v", changed, err)
	}
	current, err := store.GetAssignment(ctx, assignment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != domain.AssignmentPending {
		t.Fatalf("one-sided acknowledgement opened assignment: %#v", current)
	}
	if changed, err := store.applyNodeResult(ctx, "agent", 1, true, "test"); err != nil || len(changed) != 1 {
		t.Fatalf("second acknowledgement did not apply assignment: changed=%#v err=%v", changed, err)
	}
	current, err = store.GetAssignment(ctx, assignment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != domain.AssignmentApplied {
		t.Fatalf("assignment did not become applied after both acknowledgements: %#v", current)
	}
}

func TestServiceUpdateReopensAssignmentGeneration(t *testing.T) {
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
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gw", PublicEndpoints: []string{"gw.example:4433"}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	service := domain.Service{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", Enabled: true}
	if err := store.PutService(ctx, service, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	assignment := domain.Assignment{ID: "assignment", GatewayID: "gw", AgentID: "agent", ServiceIDs: []string{"svc"}, Bindings: []domain.Binding{{ServiceID: "svc", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}}, Generation: 1, State: domain.AssignmentPending}
	if err := store.PutAssignment(ctx, assignment, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.applyNodeResult(ctx, "gw", 1, true, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.applyNodeResult(ctx, "agent", 1, true, "test"); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetAssignment(ctx, assignment.ID)
	if err != nil || current.State != domain.AssignmentApplied {
		t.Fatalf("assignment did not become applied: %#v err=%v", current, err)
	}
	service.LocalTarget = "127.0.0.1:8081"
	if err := store.PutService(ctx, service, WriteOptions{IfMatch: 1}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetAssignment(ctx, assignment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Generation != current.Generation+1 || updated.State != domain.AssignmentPending || updated.Revision != current.Revision+1 {
		t.Fatalf("service edit did not invalidate assignment generation: before=%#v after=%#v", current, updated)
	}
}

func TestPutAssignmentCannotBypassAcknowledgementBarrier(t *testing.T) {
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
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gw", PublicEndpoints: []string{"gw.example:4433"}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	err = store.PutAssignment(ctx, domain.Assignment{ID: "assignment", GatewayID: "gw", AgentID: "agent", ServiceIDs: []string{"svc"}, Bindings: []domain.Binding{{ServiceID: "svc", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}}, Generation: 1, State: domain.AssignmentApplied}, WriteOptions{})
	var applyErr *domain.ApplyError
	if !errors.As(err, &applyErr) || applyErr.Code != "state_controller_owned" {
		t.Fatalf("direct applied assignment was not rejected: %v", err)
	}
}

func TestAssignmentUpdateReleasesRemovedBinding(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, node := range []domain.Node{{ID: "gw", Name: "gateway", Enabled: true}, {ID: "agent", Name: "agent", Enabled: true}} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gw", PublicEndpoints: []string{"gw.example:4433"}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, service := range []domain.Service{
		{ID: "svc-a", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8001", PublicBind: "0.0.0.0"},
		{ID: "svc-b", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8002", PublicBind: "0.0.0.0"},
	} {
		if err := store.PutService(ctx, service, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	first := domain.Assignment{ID: "assignment", GatewayID: "gw", AgentID: "agent", ServiceIDs: []string{"svc-a", "svc-b"}, Bindings: []domain.Binding{{ServiceID: "svc-a", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}, {ServiceID: "svc-b", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18081}}, Generation: 1}
	if err := store.PutAssignment(ctx, first, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ServiceIDs = []string{"svc-b"}
	second.Bindings = []domain.Binding{{ServiceID: "svc-b", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18081}}
	if err := store.PutAssignment(ctx, second, WriteOptions{IfMatch: 1}); err != nil {
		t.Fatal(err)
	}
	third := domain.Assignment{ID: "other", GatewayID: "gw", AgentID: "agent", ServiceIDs: []string{"svc-a"}, Bindings: []domain.Binding{{ServiceID: "svc-a", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}}, Generation: 2}
	if err := store.PutAssignment(ctx, third, WriteOptions{}); err != nil {
		t.Fatalf("removed binding still occupied its port: %v", err)
	}
}

func TestSchedulePreservesHealthyAssignmentAndPort(t *testing.T) {
	request := ScheduleRequest{
		Agent:      domain.Node{ID: "agent", Name: "agent", Enabled: true},
		AgentSpec:  domain.AgentSpec{NodeID: "agent"},
		Services:   []domain.Service{{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", Enabled: true}},
		Existing:   &domain.Assignment{ID: "agent-gw", GatewayID: "gw", AgentID: "agent", ServiceIDs: []string{"svc"}, Bindings: []domain.Binding{{ServiceID: "svc", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}}, Generation: 4},
		Generation: 5,
	}
	assignment, err := Schedule(request, []GatewayCandidate{{Node: domain.Node{ID: "gw", Name: "gateway", Enabled: true, SpecKind: domain.NodeSpecGateway}, Spec: domain.GatewaySpec{NodeID: "gw", PublicEndpoints: []string{"gw.example:4433"}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 18080, Max: 18080}}}}, Healthy: true, Assignments: []domain.Assignment{*request.Existing}, UsedBindings: map[string]struct{}{"tcp|0.0.0.0|18080": {}}}})
	if err != nil {
		t.Fatal(err)
	}
	if assignment.ID != request.Existing.ID || len(assignment.Bindings) != 1 || assignment.Bindings[0].Port != 18080 {
		t.Fatalf("stable assignment changed: %#v", assignment)
	}
}

func TestScheduleUsesFixedGatewayWithoutFailover(t *testing.T) {
	request := ScheduleRequest{
		Agent:      domain.Node{ID: "agent", Name: "agent", Enabled: true},
		AgentSpec:  domain.AgentSpec{NodeID: "agent", GatewayID: "gw-b"},
		Services:   []domain.Service{{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", Enabled: true}},
		Generation: 1,
	}
	candidates := []GatewayCandidate{
		{Node: domain.Node{ID: "gw-a", Name: "gateway-a", Enabled: true, SpecKind: domain.NodeSpecGateway}, Spec: domain.GatewaySpec{NodeID: "gw-a", PublicEndpoints: []string{"gw-a.example:4433"}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 18080, Max: 18080}}}}, Healthy: true},
		{Node: domain.Node{ID: "gw-b", Name: "gateway-b", Enabled: true, SpecKind: domain.NodeSpecGateway}, Spec: domain.GatewaySpec{NodeID: "gw-b", PublicEndpoints: []string{"gw-b.example:4433"}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 18081, Max: 18081}}}}, Healthy: true},
	}
	assignment, err := Schedule(request, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.GatewayID != "gw-b" {
		t.Fatalf("fixed Gateway was not selected: %#v", assignment)
	}
	candidates[1].Healthy = false
	if _, err := Schedule(request, candidates); !errors.Is(err, ErrNoHealthyGateway) {
		t.Fatalf("unavailable fixed Gateway error = %v, want ErrNoHealthyGateway", err)
	}
}

func TestReconcileAssignmentsFailsOverStaleGateway(t *testing.T) {
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
	for _, id := range []string{"gw-a", "gw-b"} {
		if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: id, PublicEndpoints: []string{id + ":4433"}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 18080, Max: 18080}}}}, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	assignments, err := schedulerForTest(store).ScheduleAgent(ctx, "agent", WriteOptions{})
	if err != nil || len(assignments) != 1 || assignments[0].GatewayID != "gw-a" {
		t.Fatalf("initial assignment = %#v, err=%v", assignments, err)
	}
	observed := domain.ObservedState{SchemaVersion: domain.CurrentControlProtocolVersion, NodeID: "gw-a", AppliedGeneration: 1, Healthy: true, ObservedAt: time.Now().Add(-time.Hour)}
	document, _ := json.Marshal(observed)
	if err := store.SaveObserved(ctx, ObservedRecord{NodeID: "gw-a", Generation: 1, Document: document, UpdatedAt: observed.ObservedAt}); err != nil {
		t.Fatal(err)
	}
	failedOver, err := schedulerForTest(store).ReconcileAssignments(ctx, time.Minute)
	if err != nil || len(failedOver) != 1 || failedOver[0].GatewayID != "gw-b" {
		t.Fatalf("failover result = %#v, err=%v", failedOver, err)
	}
	assignments, err = store.ListAssignments(ctx, "", "agent")
	if err != nil || len(assignments) != 1 || assignments[0].GatewayID != "gw-b" || assignments[0].State != domain.AssignmentPending {
		t.Fatalf("failover did not replace the degraded claim atomically: assignments=%#v err=%v", assignments, err)
	}
	if snapshot, snapshotErr := store.GetSnapshot(ctx, "agent"); snapshotErr != nil || len(snapshot.Assignments) != 1 || snapshot.Assignments[0].GatewayID != "gw-b" {
		t.Fatalf("agent snapshot retained the old assignment: snapshot=%#v err=%v", snapshot, snapshotErr)
	}
}

func TestScheduleSkipsGatewayAtConnectionCapacity(t *testing.T) {
	request := ScheduleRequest{
		Agent:      domain.Node{ID: "agent", Name: "agent", Enabled: true},
		AgentSpec:  domain.AgentSpec{NodeID: "agent"},
		Services:   []domain.Service{{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", Enabled: true}},
		Generation: 1,
	}
	candidates := []GatewayCandidate{
		{Node: domain.Node{ID: "gw-full", Name: "full", Enabled: true, SpecKind: domain.NodeSpecGateway}, Spec: domain.GatewaySpec{NodeID: "gw-full", PublicEndpoints: []string{"full.example:4433"}, Capacity: domain.Capacity{MaxAgents: 1}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 18080, Max: 18080}}}}, Healthy: true, Assignments: []domain.Assignment{{ID: "existing", AgentID: "other-agent", ServiceIDs: []string{"other-service"}, State: domain.AssignmentPending}}},
		{Node: domain.Node{ID: "gw-free", Name: "free", Enabled: true, SpecKind: domain.NodeSpecGateway}, Spec: domain.GatewaySpec{NodeID: "gw-free", PublicEndpoints: []string{"free.example:4433"}, Capacity: domain.Capacity{MaxAgents: 1}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 18081, Max: 18081}}}}, Healthy: true},
	}
	assignment, err := Schedule(request, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.GatewayID != "gw-free" {
		t.Fatalf("scheduler selected saturated gateway: %#v", assignment)
	}
}
