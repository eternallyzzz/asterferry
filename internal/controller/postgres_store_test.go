package controller

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"asterferry/internal/domain"
)

func TestPostgresStoreSchemaAndRuntimeEvent(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("ASTERFERRY_TEST_POSTGRES_URL"))
	if baseURL == "" {
		t.Skip("ASTERFERRY_TEST_POSTGRES_URL is not set")
	}

	_, databaseURL := createPostgresTestSchema(t, baseURL)
	config := Config{DatabaseDriver: DatabaseDriverPostgres, DatabaseURL: databaseURL, DatabaseMaxOpenConns: 4}
	key := make([]byte, masterKeyBytes)
	repositories, err := OpenControllerRepositoriesWithConfig(config, key)
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	resources, runtime := repositories.Resources, repositories.Runtime
	if repositories.DatabaseDriver() != DatabaseDriverPostgres {
		t.Fatalf("database driver = %q", repositories.DatabaseDriver())
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := resources.CreateNode(context.Background(), domain.Node{ID: "pg-node", Name: "PostgreSQL node", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	connection := domain.RuntimeConnection{ID: "connection-1", Type: domain.RuntimeConnectionTCP, NodeID: "pg-node", Protocol: domain.ProtocolTCP, SourceIP: "192.0.2.1", SourcePort: 443, StartedAt: now, LastActivityAt: now, State: domain.RuntimeStateActive}
	event := domain.RuntimeEvent{ID: "event-1", Type: domain.RuntimeEventOpened, NodeID: "pg-node", ConnectionID: connection.ID, Connection: &connection, CreatedAt: now}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordRuntimeEvent(context.Background(), "pg-node", event.ID, "runtime_connection", payload, now); err != nil {
		t.Fatal(err)
	}
	connections, err := runtime.ListRuntimeConnections(context.Background(), RuntimeConnectionFilter{NodeID: "pg-node", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) != 1 || connections[0].ID != connection.ID || connections[0].BytesIn != 0 {
		t.Fatalf("unexpected PostgreSQL runtime connections: %#v", connections)
	}
	events, err := runtime.ListRuntimeEvents(context.Background(), "pg-node", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != event.ID {
		t.Fatalf("unexpected PostgreSQL runtime events: %#v", events)
	}
	if err := resources.SetAdvancedOperationsEnabled(context.Background(), true, WriteOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	enabled, err := resources.AdvancedOperationsEnabled(context.Background())
	if err != nil || !enabled {
		t.Fatalf("advanced operations enabled = %v, err=%v", enabled, err)
	}
}

func TestPostgresConcurrentRevisionWriteReturnsConflict(t *testing.T) {
	storeA, storeB := openTwoPostgresTestStores(t)
	ctx := context.Background()
	if err := storeA.CreateNode(ctx, domain.Node{ID: "concurrent-node", Name: "initial", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	current, err := storeA.GetNode(ctx, "concurrent-node")
	if err != nil {
		t.Fatal(err)
	}

	// Hold each UPDATE long enough for the other transaction to complete its
	// preflight SELECT against the same committed revision. Without checking
	// RowsAffected, the second transaction would then commit as a false
	// success after its conditional UPDATE affected zero rows.
	if _, err := storeA.db.ExecContext(ctx, `CREATE OR REPLACE FUNCTION test_delay_node_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.2); RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.db.ExecContext(ctx, `CREATE TRIGGER test_delay_node_update_trigger BEFORE UPDATE ON nodes FOR EACH ROW EXECUTE FUNCTION test_delay_node_update()`); err != nil {
		t.Fatal(err)
	}

	left := current
	left.Name = "left"
	right := current
	right.Name = "right"
	start := make(chan struct{})
	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	go func() {
		defer waitGroup.Done()
		<-start
		results <- storeA.UpdateNode(ctx, left, WriteOptions{IfMatch: current.Revision, Actor: "left"})
	}()
	go func() {
		defer waitGroup.Done()
		<-start
		results <- storeB.UpdateNode(ctx, right, WriteOptions{IfMatch: current.Revision, Actor: "right"})
	}()
	close(start)
	waitGroup.Wait()

	successes := 0
	conflicts := 0
	for index := 0; index < 2; index++ {
		writeErr := <-results
		switch {
		case writeErr == nil:
			successes++
		case IsRevisionConflict(writeErr):
			conflicts++
		default:
			t.Fatalf("concurrent revision write error = %v", writeErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent revision writes: successes=%d conflicts=%d", successes, conflicts)
	}
	updated, err := storeA.GetNode(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != current.Revision+1 || (updated.Name != left.Name && updated.Name != right.Name) {
		t.Fatalf("updated node = %#v, want exactly one committed revision", updated)
	}
	var updateAuditCount int
	if err := storeA.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action=? AND resource=? AND resource_id=?`, "update", "node", current.ID).Scan(&updateAuditCount); err != nil {
		t.Fatal(err)
	}
	if updateAuditCount != 1 {
		t.Fatalf("node update audit count = %d, want one", updateAuditCount)
	}
}

func TestPostgresAssignmentAcknowledgementsSerializeByAssignment(t *testing.T) {
	storeA, storeB := openTwoPostgresTestStores(t)
	ctx := context.Background()
	for _, node := range []domain.Node{
		{ID: "ack-gateway", Name: "Gateway", Enabled: true},
		{ID: "ack-agent", Name: "Agent", Enabled: true},
	} {
		if err := storeA.CreateNode(ctx, node, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := storeA.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "ack-gateway", PublicEndpoints: []string{"gateway.example:4433"}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := storeA.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "ack-agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := storeA.PutService(ctx, domain.Service{ID: "ack-service", AgentID: "ack-agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", PublicPort: 18080, Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	assignment := domain.Assignment{
		ID:         "ack-assignment",
		GatewayID:  "ack-gateway",
		AgentID:    "ack-agent",
		ServiceIDs: []string{"ack-service"},
		Bindings:   []domain.Binding{{ServiceID: "ack-service", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}},
		Generation: 1,
		State:      domain.AssignmentPending,
	}
	if err := storeA.PutAssignment(ctx, assignment, WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	// This transaction represents the Gateway's ACK while it is still
	// uncommitted. The Agent result must lock the assignment row and wait; if
	// it only reads the row, it can miss this ACK and leave the barrier pending.
	lockTx, err := storeA.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lockedID string
	if err := lockTx.QueryRowContext(ctx, `SELECT id FROM assignments WHERE id=? FOR UPDATE`, assignment.ID).Scan(&lockedID); err != nil {
		_ = lockTx.Rollback()
		t.Fatal(err)
	}
	ackTime := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := lockTx.ExecContext(ctx, `INSERT INTO assignment_acks(assignment_id,node_id,generation,status,error_code,updated_at) VALUES(?,?,?,?,?,?)`, assignment.ID, assignment.GatewayID, assignment.Generation, "applied", "", ackTime); err != nil {
		_ = lockTx.Rollback()
		t.Fatal(err)
	}

	type result struct {
		changed []domain.Assignment
		err     error
	}
	done := make(chan result, 1)
	go func() {
		changed, applyErr := storeB.applyNodeResult(ctx, assignment.AgentID, assignment.Generation, true, "agent")
		done <- result{changed: changed, err: applyErr}
	}()
	select {
	case early := <-done:
		_ = lockTx.Rollback()
		t.Fatalf("agent acknowledgement completed before the gateway transaction committed: changed=%#v err=%v", early.changed, early.err)
	case <-time.After(250 * time.Millisecond):
	}
	if err := lockTx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case applied := <-done:
		if applied.err != nil {
			t.Fatal(applied.err)
		}
		if len(applied.changed) != 1 || applied.changed[0].State != domain.AssignmentApplied {
			t.Fatalf("agent acknowledgement result = %#v, want applied assignment", applied)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent acknowledgement remained blocked after gateway commit")
	}

	current, err := storeA.GetAssignment(ctx, assignment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != domain.AssignmentApplied {
		t.Fatalf("assignment state = %q, want applied", current.State)
	}
}

func openTwoPostgresTestStores(t *testing.T) (*ResourceRepository, *ResourceRepository) {
	t.Helper()
	baseURL := strings.TrimSpace(os.Getenv("ASTERFERRY_TEST_POSTGRES_URL"))
	if baseURL == "" {
		t.Skip("ASTERFERRY_TEST_POSTGRES_URL is not set")
	}
	_, databaseURL := createPostgresTestSchema(t, baseURL)
	config := Config{DatabaseDriver: DatabaseDriverPostgres, DatabaseURL: databaseURL, DatabaseMaxOpenConns: 4}
	firstRepositories, err := OpenControllerRepositoriesWithConfig(config, testMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	secondRepositories, err := OpenControllerRepositoriesWithConfig(config, testMasterKey)
	if err != nil {
		_ = firstRepositories.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = secondRepositories.Close()
		_ = firstRepositories.Close()
	})
	return firstRepositories.Resources, secondRepositories.Resources
}

func TestInitPostgresUsesExternalDatabaseWithoutLocalDatabaseFile(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("ASTERFERRY_TEST_POSTGRES_URL"))
	if baseURL == "" {
		t.Skip("ASTERFERRY_TEST_POSTGRES_URL is not set")
	}
	_, targetURL := createPostgresTestSchema(t, baseURL)
	result, err := Init(context.Background(), InitOptions{
		Dir: filepath.Join(t.TempDir(), "controller"), GRPCAdvertise: "127.0.0.1:9443",
		DatabaseDriver: DatabaseDriverPostgres, DatabaseURL: targetURL,
		Password: "a-very-long-admin-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.DatabasePath != "" || result.Config.DatabaseURL != targetURL {
		t.Fatalf("PostgreSQL config retained local database path: %#v", result.Config)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(result.ConfigPath), "controller.db")); !os.IsNotExist(err) {
		t.Fatalf("PostgreSQL initialization created a local database: %v", err)
	}
	loaded, err := LoadConfig(result.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := LoadOrCreateMasterKey(loaded.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := OpenControllerRepositoriesWithConfig(loaded, key)
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	if _, err := repositories.Resources.GetUser(context.Background(), result.Admin.ID); err != nil {
		t.Fatal(err)
	}
}

func TestInitPostgresRejectsNonEmptyTarget(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("ASTERFERRY_TEST_POSTGRES_URL"))
	if baseURL == "" {
		t.Skip("ASTERFERRY_TEST_POSTGRES_URL is not set")
	}
	_, targetURL := createPostgresTestSchema(t, baseURL)
	targetDB, backend, err := openConfiguredDatabase(context.Background(), Config{DatabaseDriver: DatabaseDriverPostgres, DatabaseURL: targetURL})
	if err != nil {
		t.Fatal(err)
	}
	if backend != databaseBackendPostgres {
		targetDB.Close()
		t.Fatal("test URL did not open PostgreSQL")
	}
	if _, err := targetDB.Exec(`CREATE TABLE existing_operator_table (id TEXT PRIMARY KEY)`); err != nil {
		targetDB.Close()
		t.Fatal(err)
	}
	if err := targetDB.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Init(context.Background(), InitOptions{
		Dir: filepath.Join(t.TempDir(), "controller"), GRPCAdvertise: "127.0.0.1:9443",
		DatabaseDriver: DatabaseDriverPostgres, DatabaseURL: targetURL,
		Password: "a-very-long-admin-password",
	})
	if err == nil || !strings.Contains(err.Error(), "empty schema") {
		t.Fatalf("non-empty PostgreSQL target error = %v", err)
	}
}

func createPostgresTestSchema(t *testing.T, baseURL string) (*sql.DB, string) {
	t.Helper()
	schemaBytes := make([]byte, 8)
	if _, err := rand.Read(schemaBytes); err != nil {
		t.Fatal(err)
	}
	schema := "asterferry_test_" + hex.EncodeToString(schemaBytes)
	adminDB, backend, err := openConfiguredDatabase(context.Background(), Config{DatabaseDriver: DatabaseDriverPostgres, DatabaseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}
	if backend != databaseBackendPostgres {
		adminDB.Close()
		t.Fatal("test URL did not open PostgreSQL")
	}
	quotedSchema := quotePostgresIdentifier(schema)
	if _, err := adminDB.Exec(`CREATE SCHEMA ` + quotedSchema); err != nil {
		adminDB.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.Exec(`DROP SCHEMA ` + quotedSchema + ` CASCADE`)
		_ = adminDB.Close()
	})
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	values := parsed.Query()
	values.Set("search_path", schema)
	parsed.RawQuery = values.Encode()
	return adminDB, parsed.String()
}

func quotePostgresIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func TestPostgresUtilityConnectionRedactsURLPassword(t *testing.T) {
	connection, environment := postgresUtilityConnection("postgresql://user:secret%20password@example.test/db?sslmode=disable")
	if strings.Contains(connection, "secret") || environment["PGPASSWORD"] != "secret password" {
		t.Fatalf("utility connection was not redacted: connection=%q env=%v", connection, environment)
	}
	if !strings.Contains(connection, "user@example.test") {
		t.Fatalf("utility connection lost user/host: %q", connection)
	}
	if _, err := url.Parse(connection); err != nil {
		t.Fatal(fmt.Errorf("redacted utility connection is invalid: %w", err))
	}
	connection, environment = postgresUtilityConnection("postgresql://user@example.test/db?password=secret&sslmode=disable")
	if strings.Contains(connection, "secret") || environment["PGPASSWORD"] != "secret" {
		t.Fatalf("query password was not redacted: connection=%q env=%v", connection, environment)
	}
}

func TestRedactPostgresURL(t *testing.T) {
	for _, value := range []string{
		"postgres://user:secret@example.test/db?sslmode=disable",
		"postgresql://user@example.test/db?password=secret&sslmode=disable",
	} {
		redacted := redactPostgresURL(value)
		if strings.Contains(redacted, "secret") {
			t.Fatalf("redacted PostgreSQL URL contains a password: %q", redacted)
		}
	}
	if got := redactPostgresURL("not-a-postgres-url"); got != "not-a-postgres-url" {
		t.Fatalf("non-PostgreSQL value changed: %q", got)
	}
}
