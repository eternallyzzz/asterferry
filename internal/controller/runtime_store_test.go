package controller

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"asterferry/internal/domain"
)

func TestRuntimeStorePersistsLifecycleAndDefaultSafety(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "node-a", Name: "Node A", Enabled: true}, WriteOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	enabled, err := store.AdvancedOperationsEnabled(ctx)
	if err != nil || enabled {
		t.Fatalf("advanced operations default = %v, %v; want disabled", enabled, err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	connection := domain.RuntimeConnection{ID: "rt-test", Type: domain.RuntimeConnectionTCP, NodeID: "node-a", PeerNodeID: "node-b", GatewayID: "node-a", AgentID: "node-b", AssignmentID: "assignment-a", ServiceID: "service-a", Protocol: domain.ProtocolTCP, SourceIP: "192.0.2.10", SourcePort: 4242, Target: "127.0.0.1:8080", StartedAt: now, LastActivityAt: now, State: domain.RuntimeStateActive, BytesIn: 123, BytesOut: 456}
	event := domain.RuntimeEvent{ID: "runtime-event-open", Type: domain.RuntimeEventOpened, NodeID: "node-a", ConnectionID: connection.ID, Connection: &connection, CreatedAt: now}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRuntimeEvent(ctx, connection.NodeID, event.ID, "runtime_connection", payload, now); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRuntimeConnection(ctx, connection.NodeID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BytesIn != 123 || got.BytesOut != 456 || got.SourceIP != connection.SourceIP {
		t.Fatalf("stored runtime connection = %+v", got)
	}

	ended := now.Add(time.Second)
	connection.State = domain.RuntimeStateClosed
	connection.EndedAt = &ended
	connection.CloseReason = domain.RuntimeCloseOperator
	connection.BytesIn = 789
	event = domain.RuntimeEvent{ID: "runtime-event-close", Type: domain.RuntimeEventClosed, NodeID: "node-a", ConnectionID: connection.ID, Connection: &connection, CreatedAt: ended}
	payload, err = json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRuntimeEvent(ctx, connection.NodeID, event.ID, "runtime_connection", payload, ended); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetRuntimeConnection(ctx, connection.NodeID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.RuntimeStateClosed || got.EndedAt == nil || got.BytesIn != 789 {
		t.Fatalf("closed runtime connection = %+v", got)
	}
	rejected := domain.RuntimeEvent{ID: "runtime-event-reject", Type: domain.RuntimeEventRejected, NodeID: "node-a", Message: "session unavailable", CreatedAt: ended}
	payload, err = json.Marshal(rejected)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRuntimeEvent(ctx, connection.NodeID, rejected.ID, "runtime_connection", payload, ended); err != nil {
		t.Fatal(err)
	}

	events, err := store.ListRuntimeEvents(ctx, "node-a", 10)
	if err != nil || len(events) != 3 {
		t.Fatalf("runtime events = %d, %v", len(events), err)
	}
	rollups, err := store.ListRuntimeTraffic(ctx, "node-a", 10)
	if err != nil || len(rollups) == 0 {
		t.Fatalf("runtime rollups = %+v, %v", rollups, err)
	}
	var foundClosed, foundRejected bool
	for _, rollup := range rollups {
		if rollup.Closed == 1 {
			foundClosed = true
		}
		if rollup.Rejected == 1 {
			foundRejected = true
		}
	}
	if !foundClosed {
		t.Fatalf("runtime close was not rolled up: %+v", rollups)
	}
	if !foundRejected {
		t.Fatalf("runtime rejection was not rolled up: %+v", rollups)
	}
}

func TestRuntimeStoreSettingsAndSubscription(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "runtime-settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	changes, unsubscribe := store.SubscribeRuntimeChanges()
	defer unsubscribe()
	if err := store.SetAdvancedOperationsEnabled(context.Background(), true, WriteOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("runtime setting change was not published")
	}
	enabled, err := store.AdvancedOperationsEnabled(context.Background())
	if err != nil || !enabled {
		t.Fatalf("advanced operations = %v, %v", enabled, err)
	}
}
