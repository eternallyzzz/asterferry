package node

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"asterferry/internal/domain"
)

func testRuntimeConnection(id string) domain.RuntimeConnection {
	now := time.Now().UTC()
	return domain.RuntimeConnection{ID: id, Type: domain.RuntimeConnectionTCP, NodeID: "node-a", PeerNodeID: "node-b", AssignmentID: "assignment-a", ServiceID: "service-a", Protocol: domain.ProtocolTCP, SourceIP: "192.0.2.10", SourcePort: 4000, StartedAt: now, LastActivityAt: now, State: domain.RuntimeStateActive}
}

func TestCumulativeByteRate(t *testing.T) {
	started := time.Unix(100, 0).UTC()
	if got := cumulativeByteRate(100, started, started.Add(2*time.Second)); got != 50 {
		t.Fatalf("cumulative byte rate = %v, want 50", got)
	}
	if got := cumulativeByteRate(0, started, started.Add(2*time.Second)); got != 0 {
		t.Fatalf("zero-byte cumulative rate = %v, want 0", got)
	}
	if got := cumulativeByteRate(100, started, started); got != 100000 {
		t.Fatalf("sub-millisecond cumulative rate = %v, want 100000", got)
	}
}

func TestRuntimeTelemetryLifecycleAndSelector(t *testing.T) {
	telemetry := newRuntimeTelemetry()
	var closed atomic.Int32
	entry := telemetry.open(context.Background(), nil, testRuntimeConnection("rt-one"), func() { closed.Add(1) })
	if entry == nil {
		t.Fatal("runtime entry is nil")
	}
	entry.touch(100, 200)
	var snapshot domain.RuntimeSnapshot
	snapshot = telemetry.snapshot("node-a")
	if len(snapshot.Connections) != 1 || snapshot.Connections[0].BytesIn != 100 || snapshot.Connections[0].BytesOut != 200 {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	payload, err := json.Marshal(runtimeActionRequest{Action: "rate_limit", Selector: runtimeSelector{ConnectionID: "rt-one"}, Direction: "both", BytesPerSecond: 1024, BurstBytes: 2048, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	affected, err := telemetry.applyAction(context.Background(), "runtime_connection", payload)
	if err != nil || affected != 1 {
		t.Fatalf("rate limit affected=%d err=%v", affected, err)
	}
	snapshot = telemetry.snapshot("node-a")
	if snapshot.Connections[0].Limit == nil || snapshot.Connections[0].Limit.BytesPerSecond != 1024 {
		t.Fatalf("limit was not applied: %+v", snapshot.Connections[0])
	}

	payload, err = json.Marshal(runtimeActionRequest{Action: "disconnect", Selector: runtimeSelector{SourceIP: "192.0.2.10"}})
	if err != nil {
		t.Fatal(err)
	}
	affected, err = telemetry.applyAction(context.Background(), "runtime_connection", payload)
	if err != nil || affected != 1 {
		t.Fatalf("disconnect affected=%d err=%v", affected, err)
	}
	if closed.Load() != 1 || len(telemetry.snapshot("node-a").Connections) != 0 {
		t.Fatalf("disconnect did not close entry: callback=%d", closed.Load())
	}
	events := telemetry.drainEvents(10)
	if len(events) < 3 {
		t.Fatalf("lifecycle events = %d", len(events))
	}
}

func TestRuntimeLimiterStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	limiter := newRuntimeLimiter(1, 1, ctx)
	if err := limiter.wait(1); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := limiter.wait(1); err == nil {
		t.Fatal("canceled limiter accepted another write")
	}
}
