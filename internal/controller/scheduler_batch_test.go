package controller

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"asterferry/internal/domain"
	moderncsqlite "modernc.org/sqlite"
)

func TestLoadGatewayCandidatesUsesBoundedBatchQueries(t *testing.T) {
	const gatewayCount = gatewayCandidateBatchSize*2 + 1
	path := filepath.Join(t.TempDir(), "candidate-batch.db")
	store, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	for index := 0; index < gatewayCount; index++ {
		id := fmt.Sprintf("gateway-%04d", index)
		if _, err := tx.Exec(`INSERT INTO nodes(id,name,enabled,certificate_state,certificate_serial,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, id, id, 1, domain.CertificatePending, "", 1, now, now); err != nil {
			_ = tx.Rollback()
			_ = store.Close()
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO node_specs(node_id,kind,revision,updated_at) VALUES(?,?,?,?)`, id, domain.NodeSpecGateway, 1, now); err != nil {
			_ = tx.Rollback()
			_ = store.Close()
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO gateway_specs(node_id) VALUES(?)`, id); err != nil {
			_ = tx.Rollback()
			_ = store.Close()
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO gateway_endpoints(node_id,position,endpoint) VALUES(?,?,?)`, id, 0, id+".example:4433"); err != nil {
			_ = tx.Rollback()
			_ = store.Close()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var selects atomic.Int64
	db := sql.OpenDB(countingSQLiteConnector{
		driver: &countingSQLiteDriver{inner: &moderncsqlite.Driver{}, selects: &selects},
		name:   sqliteDSN(path),
	})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	repository := &ResourceRepository{databaseHandle: &databaseHandle{db: db, dialect: sqliteDialect{}}, changes: newChangeBus()}
	candidates, err := repository.LoadGatewayCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != gatewayCount {
		t.Fatalf("candidate count = %d, want %d", len(candidates), gatewayCount)
	}
	if candidates[0].Node.ID != "gateway-0000" || candidates[len(candidates)-1].Node.ID != fmt.Sprintf("gateway-%04d", gatewayCount-1) {
		t.Fatalf("candidate ordering = %q ... %q", candidates[0].Node.ID, candidates[len(candidates)-1].Node.ID)
	}
	if got := selects.Load(); got >= 40 {
		t.Fatalf("candidate SELECT count = %d, want a bounded set of batch queries", got)
	}
}

func TestListServicesUsesBatchAggregateLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate-batch.db")
	store, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "agent-1", Name: "agent-1", Enabled: true}, WriteOptions{}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent-1"}, WriteOptions{}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		service := domain.Service{ID: fmt.Sprintf("service-%d", index), AgentID: "agent-1", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "127.0.0.1", PublicPort: uint16(20000 + index), Enabled: true}
		if err := store.PutService(ctx, service, WriteOptions{}); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var selects atomic.Int64
	db := sql.OpenDB(countingSQLiteConnector{
		driver: &countingSQLiteDriver{inner: &moderncsqlite.Driver{}, selects: &selects},
		name:   sqliteDSN(path),
	})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	repository := &ResourceRepository{databaseHandle: &databaseHandle{db: db, dialect: sqliteDialect{}}, changes: newChangeBus()}
	services, err := repository.ListServices(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 3 || services[0].ID != "service-0" || services[2].ID != "service-2" {
		t.Fatalf("services = %#v", services)
	}
	if got := selects.Load(); got > 4 {
		t.Fatalf("ListServices SELECT count = %d, want bounded batch loading", got)
	}
}
