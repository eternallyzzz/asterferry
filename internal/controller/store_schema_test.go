package controller

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenControllerRepositoriesWithConfigRejectsDatabaseWithoutGenerationMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE legacy_state (id TEXT PRIMARY KEY)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openTestStore(path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrIncompatibleDatabase) {
		t.Fatalf("expected incompatible database error, got %v", err)
	}
}

func TestOpenControllerRepositoriesWithConfigPreservesSchemaProbeCause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "damaged.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE legacy_state (id TEXT PRIMARY KEY)`,
		`CREATE TABLE schema_meta (singleton INTEGER PRIMARY KEY, backend TEXT NOT NULL, fingerprint TEXT NOT NULL, initialized_at TEXT NOT NULL)`,
		`INSERT INTO schema_meta(singleton,backend,fingerprint,initialized_at) VALUES (1,'sqlite','wrong','now')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openTestStore(path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrIncompatibleDatabase) {
		t.Fatalf("expected incompatible database error, got %v", err)
	}
	if !strings.Contains(err.Error(), "database_schema_version") {
		t.Fatalf("schema probe cause was lost: %v", err)
	}
}

func TestOpenControllerRepositoriesWithConfigTreatsMissingMarkerRowAsIncompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unmarked.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE legacy_state (id TEXT PRIMARY KEY)`,
		`CREATE TABLE schema_meta (singleton INTEGER PRIMARY KEY CHECK(singleton=1), database_schema_version INTEGER NOT NULL, backend TEXT NOT NULL, database_schema_fingerprint TEXT NOT NULL, initialized_at TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openTestStore(path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrIncompatibleDatabase) {
		t.Fatalf("expected incompatible database error, got %v", err)
	}
	if strings.Contains(err.Error(), "inspect schema_meta") {
		t.Fatalf("missing marker row was reported as probe failure: %v", err)
	}
}

func TestOpenControllerRepositoriesWithConfigRejectsWrongGenerationFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wrong-generation.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO schema_meta(key,value) VALUES ('schema_version','2'),('fingerprint','asterferry-controller-sqlite-v2')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openTestStore(path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrIncompatibleDatabase) {
		t.Fatalf("expected wrong fingerprint to be rejected, got %v", err)
	}
}

func TestOpenControllerRepositoriesWithConfigDoesNotMigrateLegacyGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO schema_meta(key,value) VALUES ('schema_version','9'),('fingerprint','asterferry-controller-sqlite-v9')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openTestStore(path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrIncompatibleDatabase) {
		t.Fatalf("expected legacy database to be rejected, got %v", err)
	}
	check, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version string
	if err := check.QueryRow(`SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "9" {
		t.Fatalf("legacy schema was modified during rejection: version=%q", version)
	}
}

func TestOpenControllerRepositoriesWithConfigCreatesCurrentGenerationMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	store, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int64
	var markerBackend, fingerprint, initializedAt string
	if err := store.db.QueryRow(`SELECT database_schema_version, backend, database_schema_fingerprint, initialized_at FROM schema_meta WHERE singleton=1`).Scan(&version, &markerBackend, &fingerprint, &initializedAt); err != nil {
		t.Fatal(err)
	}
	if version != int64(CurrentDatabaseSchemaVersion) || markerBackend != DatabaseDriverSQLite || fingerprint != DatabaseSchemaFingerprint() || initializedAt == "" {
		t.Fatalf("unexpected schema marker version=%d backend=%q fingerprint=%q initialized_at=%q", version, markerBackend, fingerprint, initializedAt)
	}
	var userVersion int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if userVersion != 0 {
		t.Fatalf("unexpected legacy SQLite user_version = %d", userVersion)
	}
	var pendingTable, pendingIndex int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='node_bootstraps'`).Scan(&pendingTable); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_node_bootstraps_expires'`).Scan(&pendingIndex); err != nil {
		t.Fatal(err)
	}
	if pendingTable != 1 || pendingIndex != 1 {
		t.Fatalf("pending bootstrap schema objects = table:%d index:%d", pendingTable, pendingIndex)
	}
}

func TestOpenControllerRepositoriesWithConfigRejectsCurrentMarkerWithMissingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial-current.db")
	store, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE node_labels`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = openTestStore(path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrIncompatibleDatabase) {
		t.Fatalf("expected partially copied current database to be rejected, got %v", err)
	}
	if !strings.Contains(err.Error(), `required table "node_labels" is missing`) {
		t.Fatalf("missing required table was not reported, got %v", err)
	}
}

func TestOpenControllerRepositoriesWithConfigRejectsExplicitV10MarkerWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v10-marker.db")
	db, err := sql.Open(driverName, sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_meta (singleton INTEGER PRIMARY KEY CHECK(singleton=1), database_schema_version INTEGER NOT NULL, backend TEXT NOT NULL, database_schema_fingerprint TEXT NOT NULL, initialized_at TEXT NOT NULL)`,
		`INSERT INTO schema_meta(singleton,database_schema_version,backend,database_schema_fingerprint,initialized_at) VALUES (1,10,'sqlite','asterferry-controller-db-v10-json','now')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openTestStore(path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrIncompatibleDatabase) {
		t.Fatalf("expected v10 database to be rejected, got %v", err)
	}
	check, err := sql.Open(driverName, sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version int64
	var fingerprint string
	if err := check.QueryRow(`SELECT database_schema_version,database_schema_fingerprint FROM schema_meta WHERE singleton=1`).Scan(&version, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if version != 10 || fingerprint != "asterferry-controller-db-v10-json" {
		t.Fatalf("legacy marker was modified during rejection: version=%d fingerprint=%q", version, fingerprint)
	}
}

func TestCurrentSchemaStoresBusinessAggregatesRelationally(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "relational.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, table := range []string{"nodes", "node_specs", "services", "assignments", "observed_states"} {
		var oldJSONColumns int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name='document_json'`, table).Scan(&oldJSONColumns); err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		if oldJSONColumns != 0 {
			t.Fatalf("normalized table %s still exposes document_json", table)
		}
	}
	for _, table := range []string{
		"node_labels", "gateway_specs", "gateway_endpoints", "gateway_listeners", "gateway_port_ranges",
		"agent_specs", "agent_proxies", "agent_routes", "services", "service_selector_labels",
		"assignment_services", "assignment_bindings", "assignment_acks", "observed_sessions", "observed_listeners",
	} {
		var present int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&present); err != nil {
			t.Fatalf("inspect table %s: %v", table, err)
		}
		if present != 1 {
			t.Fatalf("normalized relation table %s is missing", table)
		}
	}
}

func TestSQLiteConnectionPragmasSurvivePoolConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pragmas.db")
	db, err := sql.Open(driverName, sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	if _, err := db.Exec(`CREATE TABLE parent (id TEXT PRIMARY KEY); CREATE TABLE child (parent_id TEXT REFERENCES parent(id));`); err != nil {
		t.Fatal(err)
	}
	first, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for name, conn := range map[string]*sql.Conn{"first": first, "second": second} {
		var foreignKeys, busyTimeout int
		if err := conn.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatalf("%s foreign_keys: %v", name, err)
		}
		if err := conn.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatalf("%s busy_timeout: %v", name, err)
		}
		if foreignKeys != 1 || busyTimeout != 5000 {
			t.Fatalf("%s connection pragmas = foreign_keys:%d busy_timeout:%d", name, foreignKeys, busyTimeout)
		}
	}
	if _, err := second.ExecContext(context.Background(), `INSERT INTO child(parent_id) VALUES('missing')`); err == nil {
		t.Fatal("foreign key constraint was not enforced on the second pooled connection")
	}
}
