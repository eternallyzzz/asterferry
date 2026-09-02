package controller

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenStoreWithConfigRejectsDatabaseWithoutGenerationMarker(t *testing.T) {
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

func TestOpenStoreWithConfigRejectsWrongGenerationFingerprint(t *testing.T) {
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

func TestOpenStoreWithConfigDoesNotMigrateLegacyGeneration(t *testing.T) {
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

func TestOpenStoreWithConfigCreatesCurrentGenerationMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	store, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int64
	var markerBackend, fingerprint, initializedAt string
	if err := store.db.QueryRow(`SELECT schema_version, backend, fingerprint, initialized_at FROM schema_meta WHERE singleton=1`).Scan(&version, &markerBackend, &fingerprint, &initializedAt); err != nil {
		t.Fatal(err)
	}
	if version != currentDBSchema || markerBackend != DatabaseDriverSQLite || fingerprint != dbSchemaFingerprint || initializedAt == "" {
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
