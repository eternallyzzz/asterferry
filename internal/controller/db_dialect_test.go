package controller

import (
	"testing"
	"time"
)

func TestPostgresConnectionPoolLifecyclePolicy(t *testing.T) {
	if postgresConnMaxLifetime != 30*time.Minute {
		t.Fatalf("postgres connection max lifetime = %s, want 30m", postgresConnMaxLifetime)
	}
	if postgresConnMaxIdleTime != 5*time.Minute {
		t.Fatalf("postgres connection max idle time = %s, want 5m", postgresConnMaxIdleTime)
	}
}

func TestBindPostgresPlaceholdersSkipsQuotedTextAndComments(t *testing.T) {
	query := `SELECT ?, '?', 'it''s ?' AS literal, "?" AS identifier, ` + "`?`" + ` AS legacy_identifier, -- ?
  ? /* ? */ ?`
	want := `SELECT $1, '?', 'it''s ?' AS literal, "?" AS identifier, ` + "`?`" + ` AS legacy_identifier, -- ?
  $2 /* ? */ $3`
	if got := bindPostgresPlaceholders(query); got != want {
		t.Fatalf("bound query = %q, want %q", got, want)
	}
}

func TestBindPostgresPlaceholdersNumbersAllArguments(t *testing.T) {
	query := "SELECT "
	for index := 0; index < 12; index++ {
		if index > 0 {
			query += ","
		}
		query += "?"
	}
	want := "SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12"
	if got := bindPostgresPlaceholders(query); got != want {
		t.Fatalf("bound query = %q, want %q", got, want)
	}
}

func TestBindPostgresPlaceholdersSkipsDollarQuotesAndJSONOperators(t *testing.T) {
	query := `SELECT $$ ? $$, $body$ value ? $body$, E'escaped \? ', payload ? 'name', payload ?| ARRAY['name'], payload ?& ARRAY['name'], payload ? ?, value = ?, value = ?`
	want := `SELECT $$ ? $$, $body$ value ? $body$, E'escaped \? ', payload ? 'name', payload ?| ARRAY['name'], payload ?& ARRAY['name'], payload ? $1, value = $2, value = $3`
	if got := bindPostgresPlaceholders(query); got != want {
		t.Fatalf("bound query = %q, want %q", got, want)
	}
}

func TestDatabaseDialectsExposeBackendSpecificSchemaContract(t *testing.T) {
	sqlite := newDatabaseDialect(databaseBackendSQLite)
	postgres := newDatabaseDialect(databaseBackendPostgres)
	if sqlite.forUpdateSuffix() != "" || postgres.forUpdateSuffix() != " FOR UPDATE" {
		t.Fatalf("unexpected row-lock suffixes: sqlite=%q postgres=%q", sqlite.forUpdateSuffix(), postgres.forUpdateSuffix())
	}
	if got := sqlite.schemaTypes(); got.blob != "BLOB" || got.bigInteger != "INTEGER" || got.real != "REAL" || got.autoID != "INTEGER PRIMARY KEY AUTOINCREMENT" {
		t.Fatalf("unexpected SQLite schema types: %#v", got)
	}
	if got := postgres.schemaTypes(); got.blob != "BYTEA" || got.bigInteger != "BIGINT" || got.real != "DOUBLE PRECISION" || got.autoID != "BIGSERIAL PRIMARY KEY" {
		t.Fatalf("unexpected PostgreSQL schema types: %#v", got)
	}
}
