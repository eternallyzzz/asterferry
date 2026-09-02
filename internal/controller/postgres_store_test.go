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
	store, err := OpenStoreWithConfig(config, key)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.DatabaseDriver() != DatabaseDriverPostgres {
		t.Fatalf("database driver = %q", store.DatabaseDriver())
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.CreateNode(context.Background(), domain.Node{ID: "pg-node", Name: "PostgreSQL node", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	connection := domain.RuntimeConnection{ID: "connection-1", Type: domain.RuntimeConnectionTCP, NodeID: "pg-node", Protocol: domain.ProtocolTCP, SourceIP: "192.0.2.1", SourcePort: 443, StartedAt: now, LastActivityAt: now, State: domain.RuntimeStateActive}
	event := domain.RuntimeEvent{ID: "event-1", Type: domain.RuntimeEventOpened, NodeID: "pg-node", ConnectionID: connection.ID, Connection: &connection, CreatedAt: now}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRuntimeEvent(context.Background(), "pg-node", event.ID, "runtime_connection", payload, now); err != nil {
		t.Fatal(err)
	}
	connections, err := store.ListRuntimeConnections(context.Background(), RuntimeConnectionFilter{NodeID: "pg-node", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) != 1 || connections[0].ID != connection.ID || connections[0].BytesIn != 0 {
		t.Fatalf("unexpected PostgreSQL runtime connections: %#v", connections)
	}
	events, err := store.ListRuntimeEvents(context.Background(), "pg-node", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != event.ID {
		t.Fatalf("unexpected PostgreSQL runtime events: %#v", events)
	}
	if err := store.SetAdvancedOperationsEnabled(context.Background(), true, WriteOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	enabled, err := store.AdvancedOperationsEnabled(context.Background())
	if err != nil || !enabled {
		t.Fatalf("advanced operations enabled = %v, err=%v", enabled, err)
	}
}

func TestSQLiteToPostgresMigration(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("ASTERFERRY_TEST_POSTGRES_URL"))
	if baseURL == "" {
		t.Skip("ASTERFERRY_TEST_POSTGRES_URL is not set")
	}
	_, targetURL := createPostgresTestSchema(t, baseURL)
	sourceDir := t.TempDir()
	initialized, err := Init(context.Background(), InitOptions{Dir: sourceDir, GRPCAdvertise: "127.0.0.1:9443", Password: "a-very-long-admin-password"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := LoadOrCreateMasterKey(initialized.Config.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	sourceStore, err := OpenStore(initialized.Config.DatabasePath, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceStore.CreateNode(context.Background(), domain.Node{ID: "migration-node", Name: "Migration node", Enabled: true}, WriteOptions{Actor: "migration-test"}); err != nil {
		sourceStore.Close()
		t.Fatal(err)
	}
	if err := sourceStore.Close(); err != nil {
		t.Fatal(err)
	}

	outputConfigPath := filepath.Join(t.TempDir(), "controller.json")
	report, err := MigrateSQLiteToPostgres(context.Background(), SQLiteToPostgresMigrationOptions{SourceConfig: initialized.Config, TargetURL: targetURL, OutputConfigPath: outputConfigPath})
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalRows == 0 || report.RowsByTable["nodes"] != 1 {
		t.Fatalf("unexpected migration report: %#v", report)
	}
	migratedConfig, err := LoadConfig(outputConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if migratedConfig.DatabaseDriver != DatabaseDriverPostgres || migratedConfig.DatabasePath != "" || migratedConfig.DatabaseURL != targetURL {
		t.Fatalf("unexpected migrated config: %#v", migratedConfig)
	}
	migratedStore, err := OpenStoreWithConfig(migratedConfig, key)
	if err != nil {
		t.Fatal(err)
	}
	defer migratedStore.Close()
	if _, err := migratedStore.GetNode(context.Background(), "migration-node"); err != nil {
		t.Fatal(err)
	}
	if err := migratedStore.CreateNode(context.Background(), domain.Node{ID: "migration-node-2", Name: "Second migration node", Enabled: true}, WriteOptions{Actor: "migration-test"}); err != nil {
		t.Fatalf("post-migration audit sequence was not usable: %v", err)
	}
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
	store, err := OpenStoreWithConfig(loaded, key)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.GetUser(context.Background(), result.Admin.ID); err != nil {
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
	quotedSchema := quoteMigrationIdentifier(schema)
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
