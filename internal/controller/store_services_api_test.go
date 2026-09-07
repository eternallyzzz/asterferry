package controller

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"asterferry/internal/domain"
)

func seedAssignedServicesForDelete(t *testing.T, serviceCount int, state string) (*ResourceRepository, domain.Assignment) {
	t.Helper()
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "gateway", Name: "gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateNode(ctx, domain.Node{ID: "agent", Name: "agent", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{
		NodeID:          "gateway",
		PublicEndpoints: []string{"gateway.example:4433"},
		PortPool:        domain.PortPool{TCP: []domain.PortRange{{Min: 18080, Max: 18081}}},
	}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent", GatewayID: "gateway"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	serviceIDs := make([]string, 0, serviceCount)
	bindings := make([]domain.Binding, 0, serviceCount)
	for index := 0; index < serviceCount; index++ {
		serviceID := "svc-" + string(rune('a'+index))
		serviceIDs = append(serviceIDs, serviceID)
		if err := store.PutService(ctx, domain.Service{ID: serviceID, AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", Enabled: true}, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, domain.Binding{ServiceID: serviceID, Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: uint16(18080 + index)})
	}
	if state == domain.AssignmentDegraded {
		bindings = nil
	}
	assignment := domain.Assignment{ID: "agent-gateway", GatewayID: "gateway", AgentID: "agent", ServiceIDs: serviceIDs, Bindings: bindings, Generation: 1, State: state}
	if err := store.PutAssignment(ctx, assignment, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	return store, assignment
}

func TestDeleteAssignedServiceRemovesEmptyAssignment(t *testing.T) {
	store, _ := seedAssignedServicesForDelete(t, 1, domain.AssignmentDegraded)
	ctx := context.Background()
	if err := store.DeleteService(ctx, "svc-a", WriteOptions{IfMatch: 1, Actor: "test"}); err != nil {
		t.Fatalf("delete assigned service: %v", err)
	}
	if _, err := store.GetService(ctx, "svc-a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted service lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.GetAssignment(ctx, "agent-gateway"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty assignment lookup error = %v, want sql.ErrNoRows", err)
	}
	for _, nodeID := range []string{"agent", "gateway"} {
		node, err := store.GetNode(ctx, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		decommissioned, err := store.DecommissionNode(ctx, nodeID, WriteOptions{IfMatch: node.Revision})
		if err != nil {
			t.Fatalf("decommission %s after deleting service: %v", nodeID, err)
		}
		if err := store.PurgeNode(ctx, nodeID, WriteOptions{IfMatch: decommissioned.Revision}); err != nil {
			t.Fatalf("purge %s after deleting service: %v", nodeID, err)
		}
	}
}

func TestDeleteAssignedServiceDetachesFromSharedAssignment(t *testing.T) {
	store, _ := seedAssignedServicesForDelete(t, 2, domain.AssignmentPending)
	ctx := context.Background()
	if err := store.DeleteService(ctx, "svc-a", WriteOptions{IfMatch: 1, Actor: "test"}); err != nil {
		t.Fatalf("delete service from shared assignment: %v", err)
	}
	assignment, err := store.GetAssignment(ctx, "agent-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if assignment.Revision != 2 || assignment.Generation != 2 || assignment.State != domain.AssignmentPending || len(assignment.ServiceIDs) != 1 || assignment.ServiceIDs[0] != "svc-b" || len(assignment.Bindings) != 1 || assignment.Bindings[0].ServiceID != "svc-b" {
		t.Fatalf("shared assignment was not pruned correctly: %#v", assignment)
	}
	if _, err := store.GetService(ctx, "svc-b"); err != nil {
		t.Fatalf("remaining service was removed with deleted service: %v", err)
	}
	if err := store.DeleteService(ctx, "svc-b", WriteOptions{IfMatch: 1, Actor: "test"}); err != nil {
		t.Fatalf("delete remaining service: %v", err)
	}
	if _, err := store.GetAssignment(ctx, "agent-gateway"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("final empty assignment lookup error = %v, want sql.ErrNoRows", err)
	}
}
