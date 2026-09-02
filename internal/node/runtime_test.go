package node

import (
	"context"
	"testing"

	"asterferry/internal/dataplane"
	"asterferry/internal/domain"
)

func TestApplySnapshotFailureRestoresEngineDrainState(t *testing.T) {
	for _, initiallyDraining := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-draining", true: "already-draining"}[initiallyDraining], func(t *testing.T) {
			engine, previous := newRuntimeDrainTestFixture(t, initiallyDraining)
			runtime := &Runtime{engine: engine, runtimeKind: domain.RoleAgent}
			failed := previous.Clone()
			failed.Generation++
			failed.Checksum = ""

			if err := runtime.applySnapshot(context.Background(), failed, &previous); err == nil {
				t.Fatal("invalid same-kind snapshot was accepted")
			}
			if engine.Generation() != previous.Generation {
				t.Fatalf("failed apply changed generation to %d, want %d", engine.Generation(), previous.Generation)
			}
			if got := engine.IsDraining(); got != initiallyDraining {
				t.Fatalf("drain state after failed apply = %v, want %v", got, initiallyDraining)
			}
		})
	}
}

func newRuntimeDrainTestFixture(t *testing.T, initiallyDraining bool) (*dataplane.Engine, domain.DesiredSnapshot) {
	t.Helper()
	engine, err := dataplane.New(dataplane.Options{Role: domain.RoleAgent, NodeID: "agent-runtime-test"})
	if err != nil {
		t.Fatal(err)
	}
	previous, err := (domain.DesiredSnapshot{
		SchemaVersion: domain.SchemaVersion,
		NodeID:        "agent-runtime-test",
		Generation:    1,
		Agent:         &domain.AgentSpec{NodeID: "agent-runtime-test"},
	}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplySnapshot(context.Background(), previous, nil); err != nil {
		t.Fatal(err)
	}
	if initiallyDraining {
		engine.BeginDrain()
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine, previous
}
