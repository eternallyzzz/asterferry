package controller

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"asterferry/internal/domain"
)

func TestBuildDesiredSnapshotRejectsMaxInt64Generation(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.CreateNode(ctx, domain.Node{ID: "max-generation-gateway", Name: "Gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{
		NodeID:          "max-generation-gateway",
		PublicEndpoints: []string{"gateway.example:4433"},
	}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	base, err := store.BuildDesiredSnapshot(ctx, "max-generation-gateway")
	if err != nil {
		t.Fatal(err)
	}
	base.Generation = uint64(math.MaxInt64)
	base, err = base.WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, SnapshotRecord{
		NodeID:     base.NodeID,
		Generation: base.Generation,
		Checksum:   base.Checksum,
		Document:   document,
	}); err != nil {
		t.Fatal(err)
	}

	spec, err := store.GetGatewaySpec(ctx, "max-generation-gateway")
	if err != nil {
		t.Fatal(err)
	}
	spec.Labels = map[string]string{"changed": "true"}
	if err := store.PutGatewaySpec(ctx, spec, WriteOptions{IfMatch: spec.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BuildDesiredSnapshot(ctx, "max-generation-gateway"); err == nil || !strings.Contains(err.Error(), "desired snapshot generation is exhausted") {
		t.Fatalf("max generation rebuild error = %v, want generation exhausted", err)
	}
}

func TestUpdateAssignmentEndpointsRejectsEmptyPublicEndpoints(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	for _, node := range []domain.Node{
		{ID: "empty-endpoint-gateway", Name: "Gateway", Enabled: true},
		{ID: "empty-endpoint-agent", Name: "Agent", Enabled: true},
	} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{
		NodeID:          "empty-endpoint-gateway",
		PublicEndpoints: []string{"gateway.example:4433"},
	}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "empty-endpoint-agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{
		ID:          "empty-endpoint-service",
		AgentID:     "empty-endpoint-agent",
		Protocol:    domain.ProtocolTCP,
		LocalTarget: "127.0.0.1:8080",
		PublicBind:  "0.0.0.0",
		PublicPort:  18080,
		Enabled:     true,
	}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAssignment(ctx, domain.Assignment{
		ID:             "empty-endpoint-assignment",
		GatewayID:      "empty-endpoint-gateway",
		AgentID:        "empty-endpoint-agent",
		ServiceIDs:     []string{"empty-endpoint-service"},
		Bindings:       []domain.Binding{{ServiceID: "empty-endpoint-service", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}},
		Generation:     1,
		State:          domain.AssignmentPending,
		PublicEndpoint: "gateway.example:4433",
	}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = updateAssignmentEndpointsTx(ctx, tx, domain.GatewaySpec{NodeID: "empty-endpoint-gateway"})
	if err == nil || err.Error() != "gateway spec has no public endpoints" {
		t.Fatalf("empty gateway endpoint error = %v, want explicit validation error", err)
	}
}
