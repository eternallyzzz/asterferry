package controller

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"asterferry/internal/domain"
)

func TestMigrateDatabaseV3DryRunAndPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	prepareV3Database(t, path)

	dryRun, err := MigrateDatabase(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.FromVersion != legacyDBSchema || dryRun.ToVersion != currentDBSchema || dryRun.Assignments != 1 || dryRun.AssignmentServices != 1 || dryRun.LegacyIdempotencyKeys != 1 || !dryRun.DryRun {
		t.Fatalf("unexpected dry-run report: %#v", dryRun)
	}
	assertSchemaMarker(t, path, legacyDBSchema, legacyDBFingerprint)
	if tableExists(t, path, "assignment_services") {
		t.Fatal("dry-run created the assignment service relation")
	}

	report, err := MigrateDatabase(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.BackupPath == "" {
		t.Fatal("migration did not retain a rollback backup")
	}
	if _, err := os.Stat(report.BackupPath); err != nil {
		t.Fatalf("migration rollback backup is missing: %v", err)
	}
	assertSchemaMarker(t, path, currentDBSchema, dbSchemaFingerprint)

	store, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var relations, idempotency int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM assignment_services`).Scan(&relations); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM idempotency_keys`).Scan(&idempotency); err != nil {
		t.Fatal(err)
	}
	if relations != 1 || idempotency != 0 {
		t.Fatalf("migrated relation/idempotency counts = %d/%d", relations, idempotency)
	}
	assignment, err := store.GetAssignment(context.Background(), "assignment")
	if err != nil || len(assignment.ServiceIDs) != 1 || assignment.ServiceIDs[0] != "svc" {
		t.Fatalf("migrated assignment = %#v, err=%v", assignment, err)
	}
	assertSchemaMarker(t, report.BackupPath, legacyDBSchema, legacyDBFingerprint)
}

func prepareV3Database(t *testing.T, path string) {
	t.Helper()
	store, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, node := range []domain.Node{
		{ID: "gw", Role: domain.RoleGateway, Name: "gateway", Enabled: true},
		{ID: "agent", Role: domain.RoleAgent, Name: "agent", Enabled: true},
	} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	if err := store.PutService(ctx, domain.Service{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0"}, WriteOptions{}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.PutAssignment(ctx, domain.Assignment{ID: "assignment", GatewayID: "gw", AgentID: "agent", ServiceIDs: []string{"svc"}, Bindings: []domain.Binding{{ServiceID: "svc", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}}, Generation: 1}, WriteOptions{}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open(driverName, sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, statement := range []string{
		`DROP INDEX idx_assignment_services_service`,
		`DROP TABLE assignment_services`,
		`DROP INDEX idx_assignments_agent`,
		`UPDATE schema_meta SET value='3' WHERE key='schema_version'`,
		`UPDATE schema_meta SET value='asterferry-controller-sqlite-v3' WHERE key='fingerprint'`,
		`PRAGMA user_version=3`,
		`INSERT INTO idempotency_keys(key,request_hash,response_json,created_at) VALUES('legacy-key','legacy-hash','{}','2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func assertSchemaMarker(t *testing.T, path string, wantVersion int, wantFingerprint string) {
	t.Helper()
	db, err := sql.Open(driverName, sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	version, fingerprint, err := schemaIdentity(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if version != wantVersion || fingerprint != wantFingerprint {
		t.Fatalf("schema marker = %d/%q, want %d/%q", version, fingerprint, wantVersion, wantFingerprint)
	}
}

func tableExists(t *testing.T, path, name string) bool {
	t.Helper()
	db, err := sql.Open(driverName, sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return count == 1
}
