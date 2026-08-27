package dataplane

import (
	"context"
	"testing"

	"asterferry/internal/afdp"
	"asterferry/internal/domain"
)

func TestEngineEgressPolicyResolvesAndLimitsTargets(t *testing.T) {
	engine, err := New(Options{Role: domain.RoleAgent, NodeID: "agent-egress"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (domain.DesiredSnapshot{
		SchemaVersion: domain.SchemaVersion,
		NodeID:        "agent-egress",
		Generation:    1,
		Agent: &domain.AgentSpec{
			NodeID: "agent-egress",
			Egress: domain.EgressPolicy{
				Enabled:           true,
				TCPPorts:          []string{"80"},
				AllowCIDRs:        []string{"127.0.0.0/8"},
				AllowSpecialCIDRs: []string{"127.0.0.0/8"},
				MaxConnections:    1,
			},
		},
	}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), snapshot, nil); err != nil {
		t.Fatal(err)
	}
	if got := engine.MaxBufferBytes(); got != 0 {
		t.Fatalf("unexpected default buffer limit: %d", got)
	}
	approved, release, err := engine.AcquireEgress(context.Background(), domain.ProtocolTCP, "127.0.0.1:80")
	if err != nil {
		t.Fatal(err)
	}
	if approved != "127.0.0.1:80" || engine.ActiveEgress() != 1 {
		t.Fatalf("unexpected approved egress target=%q active=%d", approved, engine.ActiveEgress())
	}
	if _, _, err := engine.AcquireEgress(context.Background(), domain.ProtocolTCP, "127.0.0.1:80"); err == nil {
		t.Fatal("egress connection limit was not enforced")
	}
	release()
	release()
	if engine.ActiveEgress() != 0 {
		t.Fatalf("egress release was not idempotent: active=%d", engine.ActiveEgress())
	}
	if _, _, err := engine.AcquireEgress(context.Background(), domain.ProtocolTCP, "127.0.0.1:443"); err == nil {
		t.Fatal("disallowed egress port was accepted")
	}
}

func TestEngineEgressPolicyRequiresSpecialUseException(t *testing.T) {
	engine, err := New(Options{Role: domain.RoleAgent, NodeID: "agent-special"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (domain.DesiredSnapshot{
		SchemaVersion: domain.SchemaVersion,
		NodeID:        "agent-special",
		Generation:    1,
		Agent: &domain.AgentSpec{
			NodeID: "agent-special",
			Egress: domain.EgressPolicy{Enabled: true, TCPPorts: []string{"80"}, AllowCIDRs: []string{"127.0.0.0/8"}},
		},
	}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), snapshot, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.AcquireEgress(context.Background(), domain.ProtocolTCP, "127.0.0.1:80"); err == nil {
		t.Fatal("special-use destination bypassed its explicit exception")
	}
}

func TestEngineEgressLimitAppliesWhenFilteringIsDisabled(t *testing.T) {
	engine, err := New(Options{Role: domain.RoleAgent, NodeID: "agent-unrestricted"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (domain.DesiredSnapshot{
		SchemaVersion: domain.SchemaVersion,
		NodeID:        "agent-unrestricted",
		Generation:    1,
		Agent: &domain.AgentSpec{
			NodeID: "agent-unrestricted",
			Egress: domain.EgressPolicy{MaxConnections: 1},
		},
	}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), snapshot, nil); err != nil {
		t.Fatal(err)
	}
	_, release, err := engine.AcquireEgress(context.Background(), domain.ProtocolTCP, "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, _, err := engine.AcquireEgress(context.Background(), domain.ProtocolTCP, "example.org:443"); err == nil {
		t.Fatal("disabled egress filtering bypassed the connection limit")
	}
}

func TestGatewayEngineAuthorizesEgressWithoutService(t *testing.T) {
	engine, err := New(Options{Role: domain.RoleGateway, NodeID: "gateway-egress"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (domain.DesiredSnapshot{
		SchemaVersion: domain.SchemaVersion,
		NodeID:        "gateway-egress",
		Generation:    1,
		Gateway: &domain.GatewaySpec{
			NodeID:          "gateway-egress",
			PublicEndpoints: []string{"gw.example:4433"},
			Egress: domain.EgressPolicy{
				Enabled:           true,
				TCPPorts:          []string{"443"},
				AllowCIDRs:        []string{"192.0.2.0/24"},
				AllowSpecialCIDRs: []string{"192.0.2.0/24"},
			},
		},
		Assignments: []domain.Assignment{{ID: "assignment-egress", GatewayID: "gateway-egress", AgentID: "agent-egress", Generation: 1}},
	}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), snapshot, nil); err != nil {
		t.Fatal(err)
	}
	metadata := afdp.OpenMetadata{Protocol: domain.ProtocolTCP, Target: "192.0.2.10:443", Egress: true}
	if err := engine.AuthorizeOpen("assignment-egress", metadata); err != nil {
		t.Fatalf("gateway egress was not authorized: %v", err)
	}
	defer engine.ReleaseOpen("assignment-egress")
	approved, release, err := engine.AcquireEgress(context.Background(), domain.ProtocolTCP, metadata.Target)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if approved != metadata.Target {
		t.Fatalf("gateway egress target changed unexpectedly: %q", approved)
	}
}

func TestEngineAppliesAtomicallyAndBoundsAdmissions(t *testing.T) {
	engine, err := New(Options{Role: domain.RoleAgent, NodeID: "agent-1", MaxStreams: 1, MaxSessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	service := domain.Service{ID: "svc-1", AgentID: "agent-1", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "127.0.0.1", Enabled: true}
	assignment := domain.Assignment{ID: "as-1", GatewayID: "gw-1", AgentID: "agent-1", ServiceIDs: []string{"svc-1"}, Generation: 1}
	snapshot, err := (domain.DesiredSnapshot{SchemaVersion: domain.SchemaVersion, NodeID: "agent-1", Generation: 1, Agent: &domain.AgentSpec{NodeID: "agent-1"}, Services: []domain.Service{service}, Assignments: []domain.Assignment{assignment}}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), snapshot, nil); err != nil {
		t.Fatal(err)
	}
	if err := engine.AuthorizeSession(afdp.SessionHello{AssignmentID: "as-1", AgentID: "agent-1", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if err := engine.AuthorizeSession(afdp.SessionHello{AssignmentID: "as-1", AgentID: "agent-1", Generation: 1}); err == nil {
		t.Fatal("session limit was not enforced")
	}
	if err := engine.AuthorizeOpen("as-1", afdp.OpenMetadata{Protocol: domain.ProtocolTCP, ServiceID: "svc-1", Target: service.LocalTarget}); err != nil {
		t.Fatal(err)
	}
	if err := engine.AuthorizeOpen("as-1", afdp.OpenMetadata{Protocol: domain.ProtocolTCP, ServiceID: "svc-1", Target: service.LocalTarget}); err == nil {
		t.Fatal("stream limit was not enforced")
	}
	engine.ReleaseOpen("as-1")
	engine.ReleaseSession()
	if err := engine.ApplySnapshot(context.Background(), snapshot, nil); err == nil {
		t.Fatal("stale generation was accepted")
	}
}

func TestEngineAppliesAgentConnectionLimit(t *testing.T) {
	engine, err := New(Options{Role: domain.RoleAgent, NodeID: "agent-connections", MaxStreams: 8})
	if err != nil {
		t.Fatal(err)
	}
	service := domain.Service{ID: "svc", AgentID: "agent-connections", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "127.0.0.1", Enabled: true}
	snapshot, err := (domain.DesiredSnapshot{
		SchemaVersion: domain.SchemaVersion,
		NodeID:        "agent-connections",
		Generation:    1,
		Agent:         &domain.AgentSpec{NodeID: "agent-connections", Limits: domain.AgentLimits{MaxConnections: 1, MaxBufferBytes: 4096}},
		Services:      []domain.Service{service},
		Assignments:   []domain.Assignment{{ID: "assignment", GatewayID: "gateway", AgentID: "agent-connections", ServiceIDs: []string{"svc"}, Generation: 1}},
	}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), snapshot, nil); err != nil {
		t.Fatal(err)
	}
	if got := engine.MaxBufferBytes(); got != 4096 {
		t.Fatalf("agent buffer limit was not applied: %d", got)
	}
	open := afdp.OpenMetadata{Protocol: domain.ProtocolTCP, ServiceID: "svc", Target: service.LocalTarget}
	if err := engine.AuthorizeOpen("assignment", open); err != nil {
		t.Fatal(err)
	}
	if err := engine.AuthorizeOpen("assignment", open); err == nil {
		t.Fatal("agent connection limit was not enforced")
	}
	engine.ReleaseOpen("assignment")
}

func TestEngineLocalProxyUsesConnectionLimit(t *testing.T) {
	engine, err := New(Options{Role: domain.RoleAgent, NodeID: "agent-local", MaxStreams: 4})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (domain.DesiredSnapshot{
		SchemaVersion: domain.SchemaVersion,
		NodeID:        "agent-local",
		Generation:    1,
		Agent: &domain.AgentSpec{
			NodeID: "agent-local",
			Limits: domain.AgentLimits{MaxConnections: 1},
		},
	}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), snapshot, nil); err != nil {
		t.Fatal(err)
	}
	if err := engine.AuthorizeLocalOpen(); err != nil {
		t.Fatal(err)
	}
	if err := engine.AuthorizeLocalOpen(); err == nil {
		t.Fatal("local proxy connection limit was not enforced")
	}
	engine.ReleaseLocalOpen()
	engine.ReleaseLocalOpen()
	if got := engine.ActiveStreams(); got != 0 {
		t.Fatalf("local proxy reservation was not released: %d", got)
	}
}

func TestEngineResetSnapshotClearsSpeculativeGeneration(t *testing.T) {
	engine, err := New(Options{Role: domain.RoleAgent, NodeID: "agent-reset"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (domain.DesiredSnapshot{
		SchemaVersion: domain.SchemaVersion,
		NodeID:        "agent-reset",
		Generation:    7,
		Agent:         &domain.AgentSpec{NodeID: "agent-reset"},
	}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), snapshot, nil); err != nil {
		t.Fatal(err)
	}
	if err := engine.ResetSnapshot(snapshot.Generation); err != nil {
		t.Fatal(err)
	}
	if engine.Generation() != 0 {
		t.Fatalf("speculative snapshot was not cleared: generation=%d", engine.Generation())
	}
	if _, ok := engine.AgentSpec(); ok {
		t.Fatal("speculative agent spec was not cleared")
	}
	if !engine.IsDraining() {
		t.Fatal("engine must remain drained after clearing a failed generation")
	}
	if err := engine.ResetSnapshot(snapshot.Generation); err == nil {
		t.Fatal("reset accepted a stale expected generation")
	}
}

func TestEngineOpenLeaseIsGenerationScoped(t *testing.T) {
	engine, err := New(Options{Role: domain.RoleAgent, NodeID: "agent-lease", MaxStreams: 8})
	if err != nil {
		t.Fatal(err)
	}
	service := domain.Service{ID: "svc", AgentID: "agent-lease", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "127.0.0.1", Enabled: true}
	first, err := (domain.DesiredSnapshot{
		SchemaVersion: domain.SchemaVersion,
		NodeID:        "agent-lease",
		Generation:    1,
		Agent:         &domain.AgentSpec{NodeID: "agent-lease"},
		Services:      []domain.Service{service},
		Assignments:   []domain.Assignment{{ID: "assignment", GatewayID: "gateway", AgentID: "agent-lease", ServiceIDs: []string{"svc"}, Generation: 1}},
	}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), first, nil); err != nil {
		t.Fatal(err)
	}
	oldLease, err := engine.ReserveOpen("assignment", afdp.OpenMetadata{Protocol: domain.ProtocolTCP, ServiceID: "svc", Target: service.LocalTarget})
	if err != nil {
		t.Fatal(err)
	}
	second := first.Clone()
	second.Generation = 2
	second.Assignments[0].Generation = 2
	second, err = second.WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), second, &first); err != nil {
		t.Fatal(err)
	}
	newLease, err := engine.ReserveOpen("assignment", afdp.OpenMetadata{Protocol: domain.ProtocolTCP, ServiceID: "svc", Target: service.LocalTarget})
	if err != nil {
		t.Fatal(err)
	}
	oldLease.Release()
	if got := engine.ActiveStreamsForAssignment("assignment"); got != 1 {
		t.Fatalf("old-generation cleanup changed current active count: %d", got)
	}
	newLease.Release()
	if got := engine.ActiveStreamsForAssignment("assignment"); got != 0 {
		t.Fatalf("new-generation lease was not released: %d", got)
	}
	if got := engine.ActiveStreams(); got != 0 {
		t.Fatalf("aggregate stream reservation leaked: %d", got)
	}
}

func TestEngineSnapshotStreamLimitCanBeRelaxed(t *testing.T) {
	engine, err := New(Options{Role: domain.RoleAgent, NodeID: "agent-limit-reset", MaxStreams: 4})
	if err != nil {
		t.Fatal(err)
	}
	service := domain.Service{ID: "svc", AgentID: "agent-limit-reset", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "127.0.0.1", Enabled: true}
	first, err := (domain.DesiredSnapshot{
		SchemaVersion: domain.SchemaVersion,
		NodeID:        "agent-limit-reset",
		Generation:    1,
		Agent:         &domain.AgentSpec{NodeID: "agent-limit-reset", Limits: domain.AgentLimits{MaxStreams: 1}},
		Services:      []domain.Service{service},
		Assignments:   []domain.Assignment{{ID: "assignment", GatewayID: "gateway", AgentID: "agent-limit-reset", ServiceIDs: []string{"svc"}, Generation: 1}},
	}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), first, nil); err != nil {
		t.Fatal(err)
	}
	open := afdp.OpenMetadata{Protocol: domain.ProtocolTCP, ServiceID: "svc", Target: service.LocalTarget}
	lease, err := engine.ReserveOpen("assignment", open)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReserveOpen("assignment", open); err == nil {
		t.Fatal("tight snapshot stream limit was not enforced")
	}
	lease.Release()

	second := first.Clone()
	second.Generation = 2
	second.Assignments[0].Generation = 2
	second.Agent.Limits.MaxStreams = 0
	second, err = second.WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), second, &first); err != nil {
		t.Fatal(err)
	}
	firstLease, err := engine.ReserveOpen("assignment", open)
	if err != nil {
		t.Fatal(err)
	}
	secondLease, err := engine.ReserveOpen("assignment", open)
	if err != nil {
		t.Fatal(err)
	}
	firstLease.Release()
	secondLease.Release()
}
