package domain

import (
	"testing"
	"time"
)

func TestSnapshotValidationRejectsCrossNodeState(t *testing.T) {
	snapshot := DesiredSnapshot{SchemaVersion: SchemaVersion, NodeID: "agent-1", Generation: 1, Agent: &AgentSpec{NodeID: "agent-1"}, Services: []Service{{ID: "svc-1", AgentID: "agent-1", Protocol: ProtocolTCP, LocalTarget: "127.0.0.1:80", PublicBind: "0.0.0.0"}}, Assignments: []Assignment{{ID: "as-1", GatewayID: "gw-1", AgentID: "agent-1", ServiceIDs: []string{"svc-1"}, Generation: 2}}}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("assignment generation mismatch was accepted")
	}
	snapshot.Assignments[0].Generation = 1
	snapshot.Services[0].AgentID = "agent-2"
	if err := snapshot.Validate(); err == nil {
		t.Fatal("agent snapshot containing another agent's service was accepted")
	}
}

func TestChecksumIgnoresResourceOrdering(t *testing.T) {
	base := DesiredSnapshot{SchemaVersion: SchemaVersion, NodeID: "gateway-1", Generation: 1, Gateway: &GatewaySpec{NodeID: "gateway-1", PublicEndpoints: []string{"gw.example:4433"}}, Services: []Service{{ID: "b", AgentID: "agent-1", Protocol: ProtocolTCP, LocalTarget: "127.0.0.1:81", PublicBind: "0.0.0.0"}, {ID: "a", AgentID: "agent-1", Protocol: ProtocolTCP, LocalTarget: "127.0.0.1:80", PublicBind: "0.0.0.0"}}}
	other := base.Clone()
	other.Services[0], other.Services[1] = other.Services[1], other.Services[0]
	left, err := base.ComputeChecksum()
	if err != nil {
		t.Fatal(err)
	}
	right, err := other.ComputeChecksum()
	if err != nil || left != right {
		t.Fatalf("resource ordering changed checksum: %s != %s (err=%v)", left, right, err)
	}
}

func TestChecksumExcludesRepositoryMetadata(t *testing.T) {
	base := DesiredSnapshot{
		SchemaVersion: SchemaVersion,
		NodeID:        "gateway-1",
		Generation:    4,
		Gateway:       &GatewaySpec{NodeID: "gateway-1", PublicEndpoints: []string{"gw.example:4433"}, Revision: 2},
		Services:      []Service{{ID: "svc", AgentID: "agent-1", Protocol: ProtocolTCP, LocalTarget: "127.0.0.1:80", PublicBind: "0.0.0.0", Revision: 3, UpdatedAt: time.Unix(10, 0)}},
		Assignments:   []Assignment{{ID: "as", GatewayID: "gateway-1", AgentID: "agent-1", ServiceIDs: []string{"svc"}, Generation: 4, Revision: 5, UpdatedAt: time.Unix(10, 0)}},
	}
	other := base.Clone()
	other.Gateway.Revision = 99
	other.Services[0].Revision = 100
	other.Services[0].UpdatedAt = time.Unix(99, 0)
	other.Assignments[0].Revision = 101
	other.Assignments[0].UpdatedAt = time.Unix(99, 0)
	left, err := base.ComputeChecksum()
	if err != nil {
		t.Fatal(err)
	}
	right, err := other.ComputeChecksum()
	if err != nil || left != right {
		t.Fatalf("repository metadata changed checksum: %s != %s (err=%v)", left, right, err)
	}
}

func TestSnapshotValidationRejectsListenerBindingCollision(t *testing.T) {
	snapshot := DesiredSnapshot{
		SchemaVersion: SchemaVersion,
		NodeID:        "gateway-1",
		Generation:    1,
		Gateway: &GatewaySpec{
			NodeID:          "gateway-1",
			PublicEndpoints: []string{"gw.example:4433"},
			Listeners:       []Listener{{Protocol: ProtocolTCP, Bind: "0.0.0.0", Port: 18080}},
		},
		Services:    []Service{{ID: "svc", AgentID: "agent-1", Protocol: ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0"}},
		Assignments: []Assignment{{ID: "as", GatewayID: "gateway-1", AgentID: "agent-1", ServiceIDs: []string{"svc"}, Bindings: []Binding{{ServiceID: "svc", Protocol: ProtocolTCP, Bind: "0.0.0.0", Port: 18080}}, Generation: 1}},
	}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("listener and assignment binding collision was accepted")
	}
}
