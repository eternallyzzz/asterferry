package controller

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenStoreRejectsDatabaseWithoutGenerationMarker(t *testing.T) {
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

func TestOpenStoreRejectsWrongGenerationFingerprint(t *testing.T) {
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

func TestOpenStoreCreatesCurrentGenerationMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	store, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version, fingerprint string
	if err := store.DB().QueryRow(`SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT value FROM schema_meta WHERE key='fingerprint'`).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	if version != strconv.Itoa(currentDBSchema) || fingerprint != dbSchemaFingerprint {
		t.Fatalf("unexpected schema marker version=%q fingerprint=%q", version, fingerprint)
	}
}
