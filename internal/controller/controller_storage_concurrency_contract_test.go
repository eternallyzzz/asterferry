package controller

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"asterferry/internal/domain"
)

func TestIdempotencyReservationReplaysCommittedResultContract(t *testing.T) {
	storeA, storeB := openTwoPostgresTestStores(t)
	ctx := context.Background()
	request := struct {
		Name string `json:"name"`
	}{Name: "same-request"}

	txA, err := storeA.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()
	txB, err := storeB.db.BeginTx(ctx, nil)
	if err != nil {
		txA.Rollback()
		t.Fatal(err)
	}
	defer txB.Rollback()

	hit, err := idempotencyHit(ctx, txA, "concurrent-idempotency", request)
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("first request unexpectedly hit an existing idempotency key")
	}

	type result struct {
		hit bool
		err error
	}
	second := make(chan result, 1)
	go func() {
		secondHit, secondErr := idempotencyHit(ctx, txB, "concurrent-idempotency", request)
		second <- result{hit: secondHit, err: secondErr}
	}()
	select {
	case result := <-second:
		t.Fatalf("second reservation completed before the first transaction: %#v", result)
	case <-time.After(250 * time.Millisecond):
	}

	if err := recordIdempotency(ctx, txA, "concurrent-idempotency", request, map[string]string{"id": "created-once"}); err != nil {
		t.Fatal(err)
	}
	if err := txA.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-second:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if !result.hit {
			t.Fatal("second request did not replay the committed idempotency result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second reservation remained blocked after the first transaction committed")
	}

	var response []byte
	if err := txB.QueryRowContext(ctx, `SELECT response_json FROM idempotency_keys WHERE key=?`, "concurrent-idempotency").Scan(&response); err != nil {
		t.Fatal(err)
	}
	if string(response) != `{"id":"created-once"}` {
		t.Fatalf("stored idempotency response = %s", response)
	}
}

func TestSnapshotConditionalConflictIsReportedContract(t *testing.T) {
	storeA, storeB := openTwoPostgresTestStores(t)
	ctx := context.Background()
	if err := storeA.CreateNode(ctx, domain.Node{ID: "snapshot-agent", Name: "Snapshot agent", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := storeA.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "snapshot-agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := storeA.BuildDesiredSnapshot(ctx, "snapshot-agent")
	if err != nil {
		t.Fatal(err)
	}
	baseDocument, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	winner := snapshot
	winner.Generation++
	winner, err = winner.WithChecksum()
	if err != nil {
		t.Fatal(err)
	}
	winnerDocument, err := json.Marshal(winner)
	if err != nil {
		t.Fatal(err)
	}

	// Hold a higher-generation insert uncommitted. SaveSnapshot on the other
	// connection passes its no-row preflight, then blocks on the unique key and
	// reaches the ON CONFLICT ... WHERE false path after this transaction wins.
	lockTx, err := storeA.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback()
	if _, err := lockTx.ExecContext(ctx, `INSERT INTO desired_snapshots(node_id,generation,checksum,payload_json,created_at) VALUES(?,?,?,?,?)`, winner.NodeID, winner.Generation, winner.Checksum, winnerDocument, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		result <- storeB.SaveSnapshot(ctx, SnapshotRecord{NodeID: snapshot.NodeID, Generation: snapshot.Generation, Checksum: snapshot.Checksum, Document: baseDocument})
	}()
	select {
	case err := <-result:
		t.Fatalf("snapshot write completed before the winning transaction committed: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	if err := lockTx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !IsRevisionConflict(err) {
			t.Fatalf("stale snapshot write error = %v, want revision conflict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale snapshot write remained blocked after the winning transaction committed")
	}

	stored, err := storeA.LoadSnapshot(ctx, snapshot.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Generation != winner.Generation || !strings.EqualFold(stored.Checksum, winner.Checksum) {
		t.Fatalf("stored snapshot = generation %d checksum %q, want winner generation %d checksum %q", stored.Generation, stored.Checksum, winner.Generation, winner.Checksum)
	}
}

func TestAssignmentValidationReportsMalformedGatewayContract(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, node := range []domain.Node{
		{ID: "malformed-gateway", Name: "Gateway", Enabled: true},
		{ID: "malformed-agent", Name: "Agent", Enabled: true},
	} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "malformed-gateway", PublicEndpoints: []string{"gateway.example:4433"}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "malformed-agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "malformed-service", AgentID: "malformed-agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", PublicPort: 18080, Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE gateway_specs SET capacity_max_agents=-1 WHERE node_id=?`, "malformed-gateway"); err != nil {
		t.Fatal(err)
	}

	err = store.PutAssignment(ctx, domain.Assignment{
		ID:             "malformed-assignment",
		GatewayID:      "malformed-gateway",
		AgentID:        "malformed-agent",
		ServiceIDs:     []string{"malformed-service"},
		Bindings:       []domain.Binding{{ServiceID: "malformed-service", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}},
		Generation:     1,
		State:          domain.AssignmentPending,
		PublicEndpoint: "gateway.example:4433",
	}, WriteOptions{})
	if err == nil || !strings.Contains(err.Error(), "stored gateway spec is invalid") {
		t.Fatalf("malformed gateway aggregate error = %v", err)
	}
	if strings.Contains(err.Error(), "%!w") {
		t.Fatalf("malformed gateway document error contains formatting artifact: %v", err)
	}
}

func TestConcurrentSchedulingRetriesDynamicPortConflictContract(t *testing.T) {
	storeA, storeB := openTwoPostgresTestStores(t)
	ctx := context.Background()
	if err := storeA.CreateNode(ctx, domain.Node{ID: "race-gateway", Name: "Race gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, agentID := range []string{"race-agent-a", "race-agent-b"} {
		if err := storeA.CreateNode(ctx, domain.Node{ID: agentID, Name: agentID, Enabled: true}, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := storeA.PutGatewaySpec(ctx, domain.GatewaySpec{
		NodeID:          "race-gateway",
		PublicEndpoints: []string{"gateway.example:4433"},
		PortPool:        domain.PortPool{TCP: []domain.PortRange{{Min: 19000, Max: 19001}}},
	}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, agentID := range []string{"race-agent-a", "race-agent-b"} {
		if err := storeA.PutAgentSpec(ctx, domain.AgentSpec{NodeID: agentID}, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for index, agentID := range []string{"race-agent-a", "race-agent-b"} {
		serviceID := []string{"race-service-a", "race-service-b"}[index]
		if err := storeA.PutService(ctx, domain.Service{ID: serviceID, AgentID: agentID, Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", Enabled: true}, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := storeA.db.ExecContext(ctx, `CREATE OR REPLACE FUNCTION test_delay_binding_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.3); RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.db.ExecContext(ctx, `CREATE TRIGGER test_delay_binding_insert_trigger BEFORE INSERT ON assignment_bindings FOR EACH ROW EXECUTE FUNCTION test_delay_binding_insert()`); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for index, agentID := range []string{"race-agent-a", "race-agent-b"} {
		index, agentID := index, agentID
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			var store *ResourceRepository
			if index == 0 {
				store = storeA
			} else {
				store = storeB
			}
			_, err := schedulerForTest(store).ScheduleAgent(ctx, agentID, WriteOptions{Actor: "test"})
			results <- err
		}()
	}
	close(start)
	waitGroup.Wait()
	for index := 0; index < 2; index++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent schedule %d failed: %v", index, err)
		}
	}

	assignments, err := storeA.ListAssignments(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 2 {
		t.Fatalf("assignments = %#v, want two successful placements", assignments)
	}
	ports := make(map[uint16]struct{}, 2)
	for _, assignment := range assignments {
		if len(assignment.Bindings) != 1 {
			t.Fatalf("assignment bindings = %#v", assignment.Bindings)
		}
		ports[assignment.Bindings[0].Port] = struct{}{}
	}
	if len(ports) != 2 {
		t.Fatalf("concurrent schedules selected duplicate ports: %#v", ports)
	}
	if _, ok := ports[19000]; !ok {
		t.Fatalf("port 19000 was not allocated: %#v", ports)
	}
	if _, ok := ports[19001]; !ok {
		t.Fatalf("port 19001 was not allocated after retry: %#v", ports)
	}
}
