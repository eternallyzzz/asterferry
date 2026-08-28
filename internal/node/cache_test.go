package node

import (
	"context"
	"path/filepath"
	"testing"

	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
)

func TestEncryptedSnapshotCacheAndReconciler(t *testing.T) {
	cache, err := NewSnapshotCache(filepath.Join(t.TempDir(), "cache", "snapshot"), []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	reconciler, err := NewReconciler(cache, func(_ context.Context, _ domain.DesiredSnapshot, _ *domain.DesiredSnapshot) error {
		called++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := domain.DesiredSnapshot{SchemaVersion: domain.SchemaVersion, NodeID: "agent-1", Generation: 1, Agent: &domain.AgentSpec{NodeID: "agent-1"}}
	checksum, err := snapshot.WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	snapshot = checksum
	result := reconciler.Apply(context.Background(), snapshot)
	if result.GetStatus().String() != "APPLY_STATUS_APPLIED" || called != 1 {
		t.Fatalf("unexpected apply result: %v called=%d", result, called)
	}
	loaded, err := cache.Read()
	if err != nil || loaded.Checksum != snapshot.Checksum {
		t.Fatalf("cache read failed: %v %#v", err, loaded)
	}
	stale := reconciler.Apply(context.Background(), snapshot)
	if stale.GetStatus().String() != "APPLY_STATUS_REJECTED" {
		t.Fatalf("stale snapshot accepted: %v", stale)
	}
}

func TestReconcilerAcceptsAuthoritativeSameGenerationRepair(t *testing.T) {
	cache, err := NewSnapshotCache(filepath.Join(t.TempDir(), "snapshot.cache"), []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var applied []domain.DesiredSnapshot
	reconciler, err := NewReconciler(cache, func(_ context.Context, snapshot domain.DesiredSnapshot, _ *domain.DesiredSnapshot) error {
		applied = append(applied, snapshot)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := (domain.DesiredSnapshot{SchemaVersion: domain.SchemaVersion, NodeID: "agent-1", Generation: 1, Agent: &domain.AgentSpec{NodeID: "agent-1", Logging: domain.LoggingPolicy{Level: "info"}}}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if result := reconciler.Apply(context.Background(), first); result.GetStatus() != v1.ApplyStatus_APPLY_STATUS_APPLIED {
		t.Fatalf("initial apply status = %s", result.GetStatus())
	}
	repaired := first
	repaired.Agent.Logging.Level = "debug"
	repaired, err = repaired.WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if result := reconciler.Apply(context.Background(), repaired); result.GetStatus() != v1.ApplyStatus_APPLY_STATUS_APPLIED {
		t.Fatalf("same-generation repair status = %s, error=%v", result.GetStatus(), result.GetError())
	}
	if len(applied) != 2 || reconciler.AppliedChecksum() != repaired.Checksum {
		t.Fatalf("repair was not applied: %#v checksum=%q", applied, reconciler.AppliedChecksum())
	}
}

func TestReconcilerResetsFirstGenerationWhenCachePublishFails(t *testing.T) {
	// Corrupt the in-memory cache key after construction. Encryption fails
	// during publication, after ApplyFunc has already activated the speculative
	// generation, without relying on platform-specific filesystem permissions.
	cache, err := NewSnapshotCache(filepath.Join(t.TempDir(), "snapshot.cache"), []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	cache.key = []byte("invalid")
	var applied, reset bool
	reconciler, err := NewReconcilerWithReset(cache, func(_ context.Context, _ domain.DesiredSnapshot, _ *domain.DesiredSnapshot) error {
		applied = true
		return nil
	}, func(_ context.Context, generation uint64) error {
		if generation != 1 {
			t.Fatalf("reset generation = %d, want 1", generation)
		}
		reset = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (domain.DesiredSnapshot{SchemaVersion: domain.SchemaVersion, NodeID: "agent-1", Generation: 1, Agent: &domain.AgentSpec{NodeID: "agent-1"}}).WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	result := reconciler.Apply(context.Background(), snapshot)
	if result.GetStatus() != v1.ApplyStatus_APPLY_STATUS_REJECTED || !applied || !reset {
		t.Fatalf("cache publication failure was not rolled back: status=%s applied=%v reset=%v error=%v", result.GetStatus(), applied, reset, result.GetError())
	}
	if reconciler.AppliedGeneration() != 0 {
		t.Fatalf("failed first generation became durable: %d", reconciler.AppliedGeneration())
	}
}
