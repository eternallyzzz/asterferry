package domain

import (
	"testing"
	"time"
)

func TestSnapshotCloneDoesNotShareMutableState(t *testing.T) {
	snapshot := DesiredSnapshot{
		SchemaVersion: SchemaVersion,
		NodeID:        "gateway-1",
		Generation:    1,
		Gateway: &GatewaySpec{
			NodeID:          "gateway-1",
			PublicEndpoints: []string{"gw.example:4433"},
			Labels:          map[string]string{"region": "east"},
			Listeners:       []Listener{{Protocol: ProtocolTCP, Bind: "0.0.0.0", Port: 18080}},
			Obfuscation:     ObfuscationPolicy{Mode: "camouflage", Key: []byte("01234567890123456789012345678901"), PreviousKey: []byte("abcdefghijklmnopqrstuvwxyz123456")},
		},
		Agent: &AgentSpec{
			NodeID:          "agent-1",
			GatewaySelector: Selector{MatchLabels: map[string]string{"region": "east"}},
			Proxies:         []ProxySpec{{ID: "proxy", Protocol: "http", Bind: "127.0.0.1:8080"}},
			Routes:          []RouteRule{{Name: "route", Destination: "direct", CIDRs: []string{"10.0.0.0/8"}}},
		},
		Services:    []Service{{ID: "svc", AgentID: "agent-1", Protocol: ProtocolTCP, LocalTarget: "127.0.0.1:80", PublicBind: "0.0.0.0", GatewaySelector: Selector{MatchLabels: map[string]string{"region": "east"}}}},
		Assignments: []Assignment{{ID: "assignment", GatewayID: "gateway-1", AgentID: "agent-1", ServiceIDs: []string{"svc"}, Bindings: []Binding{{ServiceID: "svc", Protocol: ProtocolTCP, Bind: "0.0.0.0", Port: 18080}}, Generation: 1, Obfuscation: ObfuscationPolicy{Mode: "camouflage", Key: []byte("01234567890123456789012345678901")}}},
	}
	clone := snapshot.Clone()
	clone.Gateway.Labels["region"] = "west"
	clone.Gateway.PublicEndpoints[0] = "other.example:4433"
	clone.Gateway.Listeners[0].Port = 18081
	clone.Gateway.Obfuscation.Key[0] = 'x'
	clone.Agent.GatewaySelector.MatchLabels["region"] = "west"
	clone.Agent.Proxies[0].Bind = "127.0.0.1:8081"
	clone.Agent.Routes[0].CIDRs[0] = "192.168.0.0/16"
	clone.Services[0].GatewaySelector.MatchLabels["region"] = "west"
	clone.Assignments[0].ServiceIDs[0] = "other"
	clone.Assignments[0].Bindings[0].Port = 18081
	clone.Assignments[0].Obfuscation.Key[0] = 'x'

	if snapshot.Gateway.Labels["region"] != "east" || snapshot.Gateway.PublicEndpoints[0] != "gw.example:4433" || snapshot.Gateway.Listeners[0].Port != 18080 || snapshot.Gateway.Obfuscation.Key[0] != '0' {
		t.Fatal("gateway clone shares mutable state with the original")
	}
	if snapshot.Agent.GatewaySelector.MatchLabels["region"] != "east" || snapshot.Agent.Proxies[0].Bind != "127.0.0.1:8080" || snapshot.Agent.Routes[0].CIDRs[0] != "10.0.0.0/8" {
		t.Fatal("agent clone shares mutable state with the original")
	}
	if snapshot.Services[0].GatewaySelector.MatchLabels["region"] != "east" || snapshot.Assignments[0].ServiceIDs[0] != "svc" || snapshot.Assignments[0].Bindings[0].Port != 18080 || snapshot.Assignments[0].Obfuscation.Key[0] != '0' {
		t.Fatal("resource clone shares mutable state with the original")
	}
}

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
