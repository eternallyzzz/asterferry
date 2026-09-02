package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	driverName          = "sqlite"
	currentDBSchema     = 9
	dbSchemaFingerprint = "asterferry-controller-sqlite-v9"
	maxSnapshotDocument = 16 << 20
)

// ErrIncompatibleDatabase is returned for unknown database generations. The
// immediately previous v8 generation has a small additive migration path for
// the runtime operations tables; older or unmarked files remain fail-closed.
var ErrIncompatibleDatabase = errors.New("controller database belongs to an incompatible generation")

func OpenStore(path string, masterKey []byte) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("database path is required")
	}
	return OpenStoreWithConfig(Config{DatabaseDriver: DatabaseDriverSQLite, DatabasePath: path}, masterKey)
}

// OpenStoreWithConfig opens the configured Controller database. The legacy
// OpenStore(path, key) entry point above deliberately remains SQLite-only so
// package users and tests built around the original API keep working.
func OpenStoreWithConfig(config Config, masterKey []byte) (*Store, error) {
	if len(masterKey) != masterKeyBytes {
		return nil, errors.New("master key must contain exactly 32 bytes")
	}
	backend, err := validateDatabaseConfig(config)
	if err != nil {
		return nil, err
	}
	db, openedBackend, err := openConfiguredDatabase(context.Background(), config)
	if err != nil {
		return nil, err
	}
	if backend != openedBackend {
		_ = db.Close()
		return nil, errors.New("database backend changed while opening")
	}
	path := strings.TrimSpace(config.DatabasePath)
	if backend == databaseBackendPostgres {
		path = strings.TrimSpace(config.DatabaseURL)
	}
	store := &Store{db: db, path: path, backend: backend}
	copy(store.masterKey[:], masterKey)
	if backend == databaseBackendSQLite {
		if err := store.configure(); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := store.initializeSchema(context.Background(), backend); err != nil {
		_ = db.Close()
		return nil, err
	}
	// A process restart invalidates the Controller's view of live Node
	// connections. The next Node snapshot will restore active rows; until then
	// keep the read-only operations view fail-safe instead of showing stale
	// connections as live.
	if err := store.markRuntimeConnectionsUnknownOnStartup(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("reset runtime connection liveness: %w", err)
	}
	return store, nil
}

func NewStore(path string, masterKey []byte) (*Store, error) { return OpenStore(path, masterKey) }

func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	if s.backend == databaseBackendPostgres {
		return redactPostgresURL(s.path)
	}
	return s.path
}

// DatabaseDriver returns the configured backend name for metrics and
// diagnostics. It intentionally does not expose a raw *sql.DB handle.
func (s *Store) DatabaseDriver() string {
	if s == nil || s.backend == "" {
		return DatabaseDriverSQLite
	}
	return string(s.backend)
}

func (s *Store) configure() error {
	for _, statement := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("configure sqlite: %w", err)
		}
	}
	return nil
}

// sqliteDSN carries connection-scoped pragmas in the driver DSN. PRAGMA
// foreign_keys and busy_timeout otherwise disappear whenever database/sql
// opens a replacement connection from the pool.
func sqliteDSN(path string) string {
	if path == ":memory:" {
		path = "file::memory:"
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
}

func schemaFingerprint(backend databaseBackend) string {
	if backend == databaseBackendPostgres {
		return "asterferry-controller-postgres-v9"
	}
	return dbSchemaFingerprint
}

func (s *Store) initializeSchema(ctx context.Context, backends ...databaseBackend) error {
	backend := s.backend
	if backend == "" {
		backend = databaseBackendSQLite
	}
	if len(backends) > 0 {
		backend = backends[0]
	}
	compatible, empty, err := inspectDatabase(ctx, s.db, backend)
	if err != nil {
		return err
	}
	if !empty && !compatible && backend == databaseBackendSQLite {
		migrated, migrateErr := migrateV8ToV9(ctx, s.db)
		if migrateErr != nil {
			return migrateErr
		}
		if migrated {
			compatible, empty, err = inspectDatabase(ctx, s.db, backend)
			if err != nil {
				return err
			}
		}
		if !empty && !compatible {
			return fmt.Errorf("%w: expected schema %d (%s)", ErrIncompatibleDatabase, currentDBSchema, schemaFingerprint(backend))
		}
	}
	if !empty && !compatible {
		return fmt.Errorf("%w: expected schema %d (%s)", ErrIncompatibleDatabase, currentDBSchema, schemaFingerprint(backend))
	}
	if compatible {
		if err := validateRequiredTables(ctx, s.db, backend); err != nil {
			return fmt.Errorf("%w: %v", ErrIncompatibleDatabase, err)
		}
		return nil
	}
	statements := controllerSchemaStatements(backend)
	statements = append(statements, runtimeSchemaStatements(backend)...)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("initialize %s schema: %w", backend, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_meta(key, value) VALUES ('schema_version', ?), ('fingerprint', ?)`, strconv.Itoa(currentDBSchema), schemaFingerprint(backend)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if backend == databaseBackendSQLite {
		if _, err := tx.ExecContext(ctx, `PRAGMA user_version=9`); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return s.commitAndNotify(tx)
}

func controllerSchemaStatements(backend databaseBackend) []string {
	autoID := "INTEGER PRIMARY KEY AUTOINCREMENT"
	if backend == databaseBackendPostgres {
		autoID = "BIGSERIAL PRIMARY KEY"
	}
	return []string{
		`CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			password_changed_at TEXT NOT NULL,
			role TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			revision INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE api_tokens (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token_hash TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			expires_at TEXT,
			revoked_at TEXT,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			labels_json BYTEA NOT NULL,
			enabled BIGINT NOT NULL,
			certificate_state TEXT NOT NULL,
			certificate_serial TEXT,
			revision BIGINT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE node_specs (node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE, kind TEXT NOT NULL, document_json BYTEA NOT NULL, revision BIGINT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(node_id))`,
		`CREATE TABLE services (id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, document_json BYTEA NOT NULL, revision BIGINT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE service_bindings (service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE, gateway_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, protocol TEXT NOT NULL, bind TEXT NOT NULL, port INTEGER NOT NULL, PRIMARY KEY(service_id), UNIQUE(gateway_id, protocol, bind, port))`,
		`CREATE TABLE assignments (id TEXT PRIMARY KEY, gateway_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, agent_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, document_json BYTEA NOT NULL, generation BIGINT NOT NULL, revision BIGINT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE assignment_services (assignment_id TEXT NOT NULL REFERENCES assignments(id) ON DELETE CASCADE, service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE, PRIMARY KEY(assignment_id,service_id), UNIQUE(service_id))`,
		`CREATE TABLE assignment_acks (assignment_id TEXT NOT NULL REFERENCES assignments(id) ON DELETE CASCADE, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, generation BIGINT NOT NULL, status TEXT NOT NULL, error_code TEXT, updated_at TEXT NOT NULL, PRIMARY KEY(assignment_id, node_id))`,
		`CREATE TABLE desired_snapshots (node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE, generation BIGINT NOT NULL, checksum TEXT NOT NULL, document_json BYTEA NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE observed_states (node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE, generation BIGINT NOT NULL, document_json BYTEA NOT NULL, updated_at TEXT NOT NULL)`,
		fmt.Sprintf(`CREATE TABLE audit_events (id %s, actor TEXT NOT NULL, action TEXT NOT NULL, resource TEXT NOT NULL, resource_id TEXT NOT NULL, revision BIGINT NOT NULL, attributes_json BYTEA, created_at TEXT NOT NULL)`, autoID),
		`CREATE TABLE idempotency_keys (key TEXT PRIMARY KEY, request_hash TEXT NOT NULL, response_json BYTEA NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE enrollment_tokens (id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, expires_at TEXT NOT NULL, used_at TEXT, created_at TEXT NOT NULL)`,
		`CREATE TABLE node_bootstraps (node_id TEXT PRIMARY KEY, name TEXT NOT NULL, labels_json BYTEA NOT NULL, enabled BIGINT NOT NULL, platform TEXT NOT NULL, arch TEXT NOT NULL, spec_json BYTEA, token_hash TEXT NOT NULL UNIQUE, expires_at TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE INDEX idx_node_specs_kind ON node_specs(kind, node_id)`,
		`CREATE INDEX idx_services_agent ON services(agent_id)`,
		`CREATE INDEX idx_assignments_gateway ON assignments(gateway_id)`,
		`CREATE INDEX idx_assignments_agent ON assignments(agent_id)`,
		`CREATE INDEX idx_assignment_services_service ON assignment_services(service_id)`,
		`CREATE INDEX idx_assignment_acks_generation ON assignment_acks(assignment_id,generation)`,
		`CREATE INDEX idx_audit_created ON audit_events(created_at)`,
		`CREATE INDEX idx_idempotency_created ON idempotency_keys(created_at)`,
		`CREATE INDEX idx_node_bootstraps_expires ON node_bootstraps(expires_at)`,
	}
}

// inspectDatabase distinguishes a genuinely new database from an existing
// database. PostgreSQL may contain unrelated tables in the selected schema;
// those are intentionally treated as non-empty and incompatible.
func inspectDatabase(ctx context.Context, db *sql.DB, backends ...databaseBackend) (compatible, empty bool, err error) {
	backend := databaseBackendSQLite
	if len(backends) > 0 {
		backend = backends[0]
	}
	var count int
	query := `SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`
	if backend == databaseBackendPostgres {
		// information_schema.tables omits views, sequences and foreign tables.
		// Treat every user relation in the selected schema as occupied so an
		// initialization or migration can never collide with an unrelated
		// object after the emptiness check.
		query = `SELECT count(*) FROM pg_class AS c JOIN pg_namespace AS n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind IN ('r','p','v','m','S','f')`
	}
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return false, false, err
	}
	if count == 0 {
		return false, true, nil
	}
	var version, fingerprint string
	if err := db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&version); err != nil {
		return false, false, nil
	}
	if err := db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='fingerprint'`).Scan(&fingerprint); err != nil {
		return false, false, nil
	}
	return version == strconv.Itoa(currentDBSchema) && fingerprint == schemaFingerprint(backend), false, nil
}

func validateRequiredTables(ctx context.Context, db *sql.DB, backends ...databaseBackend) error {
	backend := databaseBackendSQLite
	if len(backends) > 0 {
		backend = backends[0]
	}
	for _, table := range currentSchemaTables() {
		var count int
		query := `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`
		if backend == databaseBackendPostgres {
			query = `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' AND table_name=?`
		}
		if err := db.QueryRowContext(ctx, query, table).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("required table %q is missing", table)
		}
	}
	// A matching marker alone is not enough to make a database compatible:
	// database/sql can be pointed at a partially copied or manually edited
	// file. Validate the columns that are read by the store before accepting
	// the marker, especially the v5 password invalidation marker.
	requiredColumns := map[string][]string{
		"schema_meta":             {"key", "value"},
		"users":                   {"id", "username", "password_hash", "password_changed_at", "role", "enabled", "revision", "created_at", "updated_at"},
		"api_tokens":              {"id", "user_id", "token_hash", "name", "expires_at", "revoked_at", "created_at"},
		"nodes":                   {"id", "name", "labels_json", "enabled", "certificate_state", "certificate_serial", "revision", "created_at", "updated_at"},
		"node_specs":              {"node_id", "kind", "document_json", "revision", "updated_at"},
		"services":                {"id", "agent_id", "document_json", "revision", "updated_at"},
		"service_bindings":        {"service_id", "gateway_id", "protocol", "bind", "port"},
		"assignments":             {"id", "gateway_id", "agent_id", "document_json", "generation", "revision", "updated_at"},
		"assignment_services":     {"assignment_id", "service_id"},
		"assignment_acks":         {"assignment_id", "node_id", "generation", "status", "error_code", "updated_at"},
		"desired_snapshots":       {"node_id", "generation", "checksum", "document_json", "created_at"},
		"observed_states":         {"node_id", "generation", "document_json", "updated_at"},
		"audit_events":            {"id", "actor", "action", "resource", "resource_id", "revision", "attributes_json", "created_at"},
		"idempotency_keys":        {"key", "request_hash", "response_json", "created_at"},
		"enrollment_tokens":       {"id", "token_hash", "expires_at", "used_at", "created_at"},
		"node_bootstraps":         {"node_id", "name", "labels_json", "enabled", "platform", "arch", "spec_json", "token_hash", "expires_at", "created_at"},
		"runtime_connections":     {"node_id", "id", "type", "state", "peer_node_id", "gateway_id", "agent_id", "assignment_id", "service_id", "protocol", "source_ip", "source_port", "target", "parent_session_id", "started_at", "last_activity_at", "ended_at", "close_reason", "bytes_in", "bytes_out", "rate_in", "rate_out", "limit_json", "updated_at"},
		"runtime_events":          {"id", "event_id", "node_id", "connection_id", "event_type", "payload_json", "created_at"},
		"runtime_traffic_rollups": {"bucket_start", "node_id", "gateway_id", "agent_id", "assignment_id", "service_id", "protocol", "bytes_in", "bytes_out", "opened", "closed", "rejected", "rate_limited", "active_max"},
		"runtime_settings":        {"key", "value", "updated_at"},
	}
	for table, columns := range requiredColumns {
		for _, column := range columns {
			var count int
			query := `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`
			args := []any{table, column}
			if backend == databaseBackendPostgres {
				query = `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=? AND column_name=?`
				args = []any{table, column}
			}
			if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				return fmt.Errorf("required column %q.%q is missing", table, column)
			}
		}
	}
	for _, index := range append([]string{"idx_node_specs_kind", "idx_services_agent", "idx_assignments_gateway", "idx_assignments_agent", "idx_assignment_services_service", "idx_assignment_acks_generation", "idx_audit_created", "idx_idempotency_created", "idx_node_bootstraps_expires"}, runtimeSchemaIndexes()...) {
		var count int
		query := `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`
		if backend == databaseBackendPostgres {
			query = `SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname=?`
		}
		if err := db.QueryRowContext(ctx, query, index).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("required index %q is missing", index)
		}
	}
	return nil
}

func currentSchemaTables() []string {
	return []string{
		"schema_meta", "users", "api_tokens", "nodes", "node_specs", "services",
		"service_bindings", "assignments", "assignment_services", "assignment_acks",
		"desired_snapshots", "observed_states", "audit_events", "idempotency_keys",
		"enrollment_tokens", "node_bootstraps",
		"runtime_connections", "runtime_events", "runtime_traffic_rollups", "runtime_settings",
	}
}

func (s *Store) Close() error {
	s.close.Do(func() {
		// Wake action subscribers before closing the database so long-lived control
		// streams do not retain goroutines after the Controller shuts down.
		s.actionMu.Lock()
		for nodeID, subscribers := range s.actionSubs {
			for id, subscription := range subscribers {
				close(subscription.ch)
				delete(subscribers, id)
			}
			delete(s.actionSubs, nodeID)
		}
		for nodeID, subscribers := range s.snapshotSubs {
			for id, subscription := range subscribers {
				close(subscription.ch)
				delete(subscribers, id)
			}
			delete(s.snapshotSubs, nodeID)
		}
		s.actionMu.Unlock()
		s.changeMu.Lock()
		for id, subscription := range s.changeSubs {
			close(subscription.ch)
			delete(s.changeSubs, id)
		}
		s.changeMu.Unlock()
		s.runtimeMu.Lock()
		for id, subscription := range s.runtimeSubs {
			close(subscription.ch)
			delete(s.runtimeSubs, id)
		}
		s.runtimeMu.Unlock()
		s.err = s.db.Close()
	})
	return s.err
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
