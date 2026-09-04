package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"asterferry/internal/domain"
)

func TestGatewayCodecPreservesOrderedChildren(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "gateway-codec.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "gateway", Name: "gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	want := domain.GatewaySpec{
		NodeID:          "gateway",
		PublicEndpoints: []string{"first.example:4433", "second.example:4433"},
		Listeners: []domain.Listener{
			{Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080, Enabled: true},
			{Protocol: domain.ProtocolUDP, Bind: "0.0.0.0", Port: 18081, Enabled: true},
		},
		PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: 20000, Max: 20010}}, UDP: []domain.PortRange{{Min: 21000, Max: 21010}}},
		Egress:   domain.EgressPolicy{Enabled: true, TCPPorts: []string{"443", "8443"}, AllowCIDRs: []string{"10.0.0.0/8"}},
	}
	if err := store.PutGatewaySpec(ctx, want, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetGatewaySpec(ctx, want.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.PublicEndpoints, ",") != strings.Join(want.PublicEndpoints, ",") || len(got.Listeners) != 2 || got.Listeners[1].Protocol != domain.ProtocolUDP || len(got.PortPool.UDP) != 1 || len(got.Egress.TCPPorts) != 2 {
		t.Fatalf("gateway aggregate order/content changed: got=%#v want=%#v", got, want)
	}
}

func TestGatewayCodecRejectsNonDenseStoredPositions(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "gateway-codec-corrupt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "gateway", Name: "gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gateway", PublicEndpoints: []string{"first.example:4433", "second.example:4433"}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE gateway_endpoints SET position=2 WHERE node_id=? AND position=1`, "gateway"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetGatewaySpec(ctx, "gateway"); err == nil || !strings.Contains(err.Error(), "gateway endpoint position") {
		t.Fatalf("corrupt gateway position error = %v", err)
	}
}

func TestAgentCodecRejectsUnknownRouteValueKind(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "agent-codec-corrupt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "agent", Name: "agent", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent", Routes: []domain.RouteRule{{Name: "route", Destination: "direct", CIDRs: []string{"10.0.0.0/8"}}}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE agent_route_values SET kind='unknown' WHERE node_id=?`, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAgentSpec(ctx, "agent"); err == nil || !strings.Contains(err.Error(), "route value kind") {
		t.Fatalf("corrupt agent route kind error = %v", err)
	}
}
