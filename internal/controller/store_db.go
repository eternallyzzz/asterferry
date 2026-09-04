package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	driverName          = "sqlite"
	maxSnapshotDocument = 16 << 20
)

// ErrIncompatibleDatabase is returned for databases that do not contain the
// current schema. Development releases intentionally have no in-place schema
// migration path: initialize a new database and restore only from a backup
// produced by the same database generation.
var ErrIncompatibleDatabase = errors.New("controller database belongs to an incompatible generation")

// OpenStoreWithConfig opens the configured Controller database. Database
// configuration is explicit so SQLite and PostgreSQL have the same lifecycle
// and there is no hidden SQLite-only compatibility entry point.
func OpenStoreWithConfig(config Config, masterKey []byte) (*Repository, error) {
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
	store := &Repository{db: db, path: path, dialect: newDatabaseDialect(backend), changes: newChangeBus()}
	copy(store.masterKey[:], masterKey)
	if backend == databaseBackendSQLite {
		if err := store.configure(); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := store.initializeSchema(context.Background()); err != nil {
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

func (s *Repository) Path() string {
	if s == nil {
		return ""
	}
	if s.DatabaseDriver() == DatabaseDriverPostgres {
		return redactPostgresURL(s.path)
	}
	return s.path
}

// DatabaseDriver returns the configured backend name for metrics and
// diagnostics. It intentionally does not expose a raw *sql.DB handle.
func (s *Repository) DatabaseDriver() string {
	if s == nil || s.dialect == nil {
		return DatabaseDriverSQLite
	}
	return string(s.dialect.backend())
}

func (s *Repository) configure() error {
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

func (s *Repository) initializeSchema(ctx context.Context) error {
	if s == nil || s.dialect == nil {
		return errors.New("database dialect is not configured")
	}
	dialect := s.dialect
	compatible, empty, err := inspectDatabase(ctx, s.db, dialect)
	if err != nil {
		return err
	}
	if !empty && !compatible {
		return fmt.Errorf("%w: expected schema %s", ErrIncompatibleDatabase, databaseSchemaMarker())
	}
	if compatible {
		if err := validateRequiredTables(ctx, s.db, dialect); err != nil {
			return fmt.Errorf("%w: %v", ErrIncompatibleDatabase, err)
		}
		return nil
	}
	statements := controllerSchemaStatements(dialect.schemaTypes())
	statements = append(statements, runtimeSchemaStatements(dialect.schemaTypes())...)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("initialize %s schema: %w", dialect.backend(), err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_meta(singleton, database_schema_version, backend, database_schema_fingerprint, initialized_at) VALUES (1, ?, ?, ?, ?)`, CurrentDatabaseSchemaVersion, dialect.backend(), DatabaseSchemaFingerprint(), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return s.commitAndNotify(tx)
}

func controllerSchemaStatements(types schemaTypes) []string {
	return []string{
		fmt.Sprintf(`CREATE TABLE schema_meta (
			singleton %s PRIMARY KEY CHECK (singleton=1),
			database_schema_version %s NOT NULL,
			backend TEXT NOT NULL,
			database_schema_fingerprint TEXT NOT NULL,
			initialized_at TEXT NOT NULL
		)`, types.integer, types.integer),
		fmt.Sprintf(`CREATE TABLE users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			password_changed_at TEXT NOT NULL,
			role TEXT NOT NULL,
			enabled %s NOT NULL DEFAULT 1,
			revision %s NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`, types.integer, types.bigInteger),
		`CREATE TABLE api_tokens (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token_hash TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			expires_at TEXT,
			revoked_at TEXT,
			created_at TEXT NOT NULL
		)`,
		fmt.Sprintf(`CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			enabled %s NOT NULL,
			certificate_state TEXT NOT NULL,
			certificate_serial TEXT,
			revision %s NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`, types.bigInteger, types.bigInteger),
		`CREATE TABLE node_labels (node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(node_id,key))`,
		fmt.Sprintf(`CREATE TABLE node_specs (node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE, kind TEXT NOT NULL, revision %s NOT NULL, updated_at TEXT NOT NULL, UNIQUE(node_id))`, types.bigInteger),
		fmt.Sprintf(`CREATE TABLE gateway_specs (
			node_id TEXT PRIMARY KEY REFERENCES node_specs(node_id) ON DELETE CASCADE,
			capacity_max_agents %s NOT NULL DEFAULT 0,
			capacity_max_connections %s NOT NULL DEFAULT 0,
			capacity_max_services %s NOT NULL DEFAULT 0,
			transport_alpn TEXT NOT NULL DEFAULT '',
			transport_max_streams %s NOT NULL DEFAULT 0,
			transport_max_frame_bytes %s NOT NULL DEFAULT 0,
			transport_max_datagram_bytes %s NOT NULL DEFAULT 0,
			transport_handshake_timeout_seconds %s NOT NULL DEFAULT 0,
			transport_idle_timeout_seconds %s NOT NULL DEFAULT 0,
			obfuscation_mode TEXT NOT NULL DEFAULT '',
			obfuscation_key_ciphertext %s,
			obfuscation_previous_key_ciphertext %s,
			obfuscation_key_id TEXT NOT NULL DEFAULT '',
			obfuscation_previous_key_id TEXT NOT NULL DEFAULT '',
			obfuscation_max_padding_bytes %s NOT NULL DEFAULT 0,
			obfuscation_handshake_shaping %s NOT NULL DEFAULT 0,
			egress_enabled %s NOT NULL DEFAULT 0,
			egress_max_connections %s NOT NULL DEFAULT 0
		)`, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.blob, types.blob, types.bigInteger, types.integer, types.integer, types.bigInteger),
		fmt.Sprintf(`CREATE TABLE gateway_endpoints (node_id TEXT NOT NULL REFERENCES gateway_specs(node_id) ON DELETE CASCADE, position %s NOT NULL, endpoint TEXT NOT NULL, PRIMARY KEY(node_id,position), UNIQUE(node_id,endpoint))`, types.bigInteger),
		`CREATE TABLE gateway_labels (node_id TEXT NOT NULL REFERENCES gateway_specs(node_id) ON DELETE CASCADE, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(node_id,key))`,
		fmt.Sprintf(`CREATE TABLE gateway_listeners (node_id TEXT NOT NULL REFERENCES gateway_specs(node_id) ON DELETE CASCADE, position %s NOT NULL, protocol TEXT NOT NULL, bind TEXT NOT NULL, port %s NOT NULL, enabled %s NOT NULL, PRIMARY KEY(node_id,position))`, types.bigInteger, types.integer, types.integer),
		fmt.Sprintf(`CREATE TABLE gateway_port_ranges (node_id TEXT NOT NULL REFERENCES gateway_specs(node_id) ON DELETE CASCADE, protocol TEXT NOT NULL, position %s NOT NULL, min_port %s NOT NULL, max_port %s NOT NULL, PRIMARY KEY(node_id,protocol,position))`, types.bigInteger, types.integer, types.integer),
		fmt.Sprintf(`CREATE TABLE gateway_egress_values (node_id TEXT NOT NULL REFERENCES gateway_specs(node_id) ON DELETE CASCADE, kind TEXT NOT NULL, position %s NOT NULL, value TEXT NOT NULL, PRIMARY KEY(node_id,kind,position))`, types.bigInteger),
		fmt.Sprintf(`CREATE TABLE agent_specs (
			node_id TEXT PRIMARY KEY REFERENCES node_specs(node_id) ON DELETE CASCADE,
			limits_max_connections %s NOT NULL DEFAULT 0,
			limits_max_streams %s NOT NULL DEFAULT 0,
			limits_max_buffer_bytes %s NOT NULL DEFAULT 0,
			logging_level TEXT NOT NULL DEFAULT '',
			logging_format TEXT NOT NULL DEFAULT '',
			egress_enabled %s NOT NULL DEFAULT 0,
			egress_max_connections %s NOT NULL DEFAULT 0
		)`, types.bigInteger, types.bigInteger, types.bigInteger, types.integer, types.bigInteger),
		`CREATE TABLE agent_selector_labels (node_id TEXT NOT NULL REFERENCES agent_specs(node_id) ON DELETE CASCADE, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(node_id,key))`,
		fmt.Sprintf(`CREATE TABLE agent_proxies (node_id TEXT NOT NULL REFERENCES agent_specs(node_id) ON DELETE CASCADE, position %s NOT NULL, id TEXT NOT NULL, protocol TEXT NOT NULL, bind TEXT NOT NULL, route TEXT NOT NULL, enabled %s NOT NULL, PRIMARY KEY(node_id,position), UNIQUE(node_id,id))`, types.bigInteger, types.integer),
		fmt.Sprintf(`CREATE TABLE agent_routes (node_id TEXT NOT NULL REFERENCES agent_specs(node_id) ON DELETE CASCADE, position %s NOT NULL, name TEXT NOT NULL, destination TEXT NOT NULL, enabled %s NOT NULL, PRIMARY KEY(node_id,position), UNIQUE(node_id,name))`, types.bigInteger, types.integer),
		fmt.Sprintf(`CREATE TABLE agent_route_values (node_id TEXT NOT NULL REFERENCES agent_specs(node_id) ON DELETE CASCADE, route_position %s NOT NULL, kind TEXT NOT NULL, position %s NOT NULL, value TEXT NOT NULL, PRIMARY KEY(node_id,route_position,kind,position))`, types.bigInteger, types.bigInteger),
		fmt.Sprintf(`CREATE TABLE agent_egress_values (node_id TEXT NOT NULL REFERENCES agent_specs(node_id) ON DELETE CASCADE, kind TEXT NOT NULL, position %s NOT NULL, value TEXT NOT NULL, PRIMARY KEY(node_id,kind,position))`, types.bigInteger),
		fmt.Sprintf(`CREATE TABLE services (
			id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			protocol TEXT NOT NULL,
			local_target TEXT NOT NULL,
			public_bind TEXT NOT NULL,
			public_port %s NOT NULL DEFAULT 0,
			enabled %s NOT NULL,
			revision %s NOT NULL,
			updated_at TEXT NOT NULL
		)`, types.integer, types.integer, types.bigInteger),
		`CREATE TABLE service_selector_labels (service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(service_id,key))`,
		fmt.Sprintf(`CREATE TABLE assignments (
			id TEXT PRIMARY KEY,
			gateway_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			agent_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			generation %s NOT NULL,
			state TEXT NOT NULL,
			public_endpoint TEXT NOT NULL DEFAULT '',
			obfuscation_mode TEXT NOT NULL DEFAULT '',
			obfuscation_key_ciphertext %s,
			obfuscation_previous_key_ciphertext %s,
			obfuscation_key_id TEXT NOT NULL DEFAULT '',
			obfuscation_previous_key_id TEXT NOT NULL DEFAULT '',
			obfuscation_max_padding_bytes %s NOT NULL DEFAULT 0,
			obfuscation_handshake_shaping %s NOT NULL DEFAULT 0,
			revision %s NOT NULL,
			updated_at TEXT NOT NULL
		)`, types.bigInteger, types.blob, types.blob, types.bigInteger, types.integer, types.bigInteger),
		fmt.Sprintf(`CREATE TABLE assignment_services (assignment_id TEXT NOT NULL REFERENCES assignments(id) ON DELETE CASCADE, position %s NOT NULL, service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE, PRIMARY KEY(assignment_id,position), UNIQUE(service_id))`, types.bigInteger),
		fmt.Sprintf(`CREATE TABLE assignment_bindings (assignment_id TEXT NOT NULL REFERENCES assignments(id) ON DELETE CASCADE, position %s NOT NULL, service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE, gateway_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, protocol TEXT NOT NULL, bind TEXT NOT NULL, port %s NOT NULL, PRIMARY KEY(assignment_id,position), UNIQUE(assignment_id,service_id), UNIQUE(gateway_id,protocol,bind,port))`, types.bigInteger, types.integer),
		fmt.Sprintf(`CREATE TABLE assignment_acks (assignment_id TEXT NOT NULL REFERENCES assignments(id) ON DELETE CASCADE, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, generation %s NOT NULL, status TEXT NOT NULL, error_code TEXT, updated_at TEXT NOT NULL, PRIMARY KEY(assignment_id, node_id))`, types.bigInteger),
		fmt.Sprintf(`CREATE TABLE desired_snapshots (node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE, generation %s NOT NULL, checksum TEXT NOT NULL, payload_json %s NOT NULL, created_at TEXT NOT NULL)`, types.bigInteger, types.blob),
		fmt.Sprintf(`CREATE TABLE observed_states (
			node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
			generation %s NOT NULL,
			protocol_version %s NOT NULL,
			healthy %s NOT NULL,
			degraded %s NOT NULL,
			last_error_code TEXT NOT NULL DEFAULT '',
			last_error_path TEXT NOT NULL DEFAULT '',
			last_error_message TEXT NOT NULL DEFAULT '',
			observed_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			active_streams %s NOT NULL DEFAULT 0,
			active_sessions %s NOT NULL DEFAULT 0,
			active_egress %s NOT NULL DEFAULT 0,
			udp_oversize_drops %s NOT NULL DEFAULT 0,
			geoip_up %s NOT NULL DEFAULT 0,
			active_connections %s NOT NULL DEFAULT 0,
			active_flows %s NOT NULL DEFAULT 0,
			runtime_bytes_in_total %s NOT NULL DEFAULT 0,
			runtime_bytes_out_total %s NOT NULL DEFAULT 0,
			runtime_opened_total %s NOT NULL DEFAULT 0,
			runtime_closed_total %s NOT NULL DEFAULT 0,
			runtime_rejected_total %s NOT NULL DEFAULT 0,
			runtime_rate_limited_total %s NOT NULL DEFAULT 0,
			runtime_telemetry_dropped_total %s NOT NULL DEFAULT 0
		)`, types.bigInteger, types.integer, types.integer, types.integer, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.integer, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger, types.bigInteger),
		fmt.Sprintf(`CREATE TABLE observed_sessions (node_id TEXT NOT NULL REFERENCES observed_states(node_id) ON DELETE CASCADE, position %s NOT NULL, id TEXT NOT NULL, peer_id TEXT NOT NULL, started_at TEXT NOT NULL, streams %s NOT NULL, PRIMARY KEY(node_id,position))`, types.bigInteger, types.integer),
		fmt.Sprintf(`CREATE TABLE observed_listeners (node_id TEXT NOT NULL REFERENCES observed_states(node_id) ON DELETE CASCADE, position %s NOT NULL, protocol TEXT NOT NULL, bind TEXT NOT NULL, port %s NOT NULL, ready %s NOT NULL, PRIMARY KEY(node_id,position))`, types.bigInteger, types.integer, types.integer),
		fmt.Sprintf(`CREATE TABLE audit_events (id %s, actor TEXT NOT NULL, action TEXT NOT NULL, resource TEXT NOT NULL, resource_id TEXT NOT NULL, revision %s NOT NULL, attributes_json %s, created_at TEXT NOT NULL)`, types.autoID, types.bigInteger, types.blob),
		fmt.Sprintf(`CREATE TABLE idempotency_keys (key TEXT PRIMARY KEY, request_hash TEXT NOT NULL, response_json %s NOT NULL, created_at TEXT NOT NULL)`, types.blob),
		`CREATE TABLE enrollment_tokens (id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, expires_at TEXT NOT NULL, used_at TEXT, created_at TEXT NOT NULL)`,
		fmt.Sprintf(`CREATE TABLE node_bootstraps (node_id TEXT PRIMARY KEY, name TEXT NOT NULL, labels_json %s NOT NULL, enabled %s NOT NULL, platform TEXT NOT NULL, arch TEXT NOT NULL, spec_json %s, token_hash TEXT NOT NULL UNIQUE, expires_at TEXT NOT NULL, created_at TEXT NOT NULL)`, types.blob, types.bigInteger, types.blob),
		`CREATE INDEX idx_node_specs_kind ON node_specs(kind, node_id)`,
		`CREATE INDEX idx_services_agent ON services(agent_id)`,
		`CREATE INDEX idx_assignments_gateway ON assignments(gateway_id)`,
		`CREATE INDEX idx_assignments_agent ON assignments(agent_id)`,
		`CREATE INDEX idx_assignment_services_service ON assignment_services(service_id)`,
		`CREATE INDEX idx_assignment_bindings_gateway ON assignment_bindings(gateway_id,protocol,bind,port)`,
		`CREATE INDEX idx_node_labels_value ON node_labels(key,value,node_id)`,
		`CREATE INDEX idx_assignment_acks_generation ON assignment_acks(assignment_id,generation)`,
		`CREATE INDEX idx_audit_created ON audit_events(created_at)`,
		`CREATE INDEX idx_idempotency_created ON idempotency_keys(created_at)`,
		`CREATE INDEX idx_node_bootstraps_expires ON node_bootstraps(expires_at)`,
	}
}

// inspectDatabase distinguishes a genuinely new database from an existing
// database. PostgreSQL may contain unrelated tables in the selected schema;
// those are intentionally treated as non-empty and incompatible. A marker
// from any older generation is therefore a non-empty incompatible database,
// never a migration candidate.
func inspectDatabase(ctx context.Context, db *sql.DB, dialect databaseDialect) (compatible, empty bool, err error) {
	if dialect == nil {
		return false, false, errors.New("database dialect is not configured")
	}
	var count int
	if err := db.QueryRowContext(ctx, dialect.relationCountQuery()).Scan(&count); err != nil {
		return false, false, err
	}
	if count == 0 {
		return false, true, nil
	}
	var version int64
	var markerBackend, fingerprint string
	if err := db.QueryRowContext(ctx, `SELECT database_schema_version, backend, database_schema_fingerprint FROM schema_meta WHERE singleton=1`).Scan(&version, &markerBackend, &fingerprint); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// An existing schema without a singleton marker is a legacy or
			// otherwise incompatible database, but it is not a probe failure.
			return false, false, nil
		}
		// Preserve the incompatibility classification for callers while
		// retaining the database driver's concrete failure (for example a
		// missing column or malformed schema_meta table).
		return false, false, fmt.Errorf("%w: inspect schema_meta: %w", ErrIncompatibleDatabase, err)
	}
	return version == int64(CurrentDatabaseSchemaVersion) && databaseBackend(markerBackend) == dialect.backend() && fingerprint == DatabaseSchemaFingerprint(), false, nil
}

func validateRequiredTables(ctx context.Context, db *sql.DB, dialect databaseDialect) error {
	if dialect == nil {
		return errors.New("database dialect is not configured")
	}
	var markerCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_meta`).Scan(&markerCount); err != nil {
		return err
	}
	if markerCount != 1 {
		return fmt.Errorf("schema_meta must contain exactly one marker row, got %d", markerCount)
	}
	for _, table := range currentSchemaTables() {
		var count int
		if err := db.QueryRowContext(ctx, dialect.tableExistsQuery(), table).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("required table %q is missing", table)
		}
	}
	// A matching marker alone is not enough to make a database compatible:
	// database/sql can be pointed at a partially copied or manually edited
	// database. Validate the columns that are read by the store before
	// accepting the marker.
	requiredColumns := map[string][]string{
		"schema_meta":             {"singleton", "database_schema_version", "backend", "database_schema_fingerprint", "initialized_at"},
		"users":                   {"id", "username", "password_hash", "password_changed_at", "role", "enabled", "revision", "created_at", "updated_at"},
		"api_tokens":              {"id", "user_id", "token_hash", "name", "expires_at", "revoked_at", "created_at"},
		"nodes":                   {"id", "name", "enabled", "certificate_state", "certificate_serial", "revision", "created_at", "updated_at"},
		"node_labels":             {"node_id", "key", "value"},
		"node_specs":              {"node_id", "kind", "revision", "updated_at"},
		"gateway_specs":           {"node_id", "capacity_max_agents", "capacity_max_connections", "capacity_max_services", "transport_alpn", "transport_max_streams", "transport_max_frame_bytes", "transport_max_datagram_bytes", "transport_handshake_timeout_seconds", "transport_idle_timeout_seconds", "obfuscation_mode", "obfuscation_key_ciphertext", "obfuscation_previous_key_ciphertext", "obfuscation_key_id", "obfuscation_previous_key_id", "obfuscation_max_padding_bytes", "obfuscation_handshake_shaping", "egress_enabled", "egress_max_connections"},
		"gateway_endpoints":       {"node_id", "position", "endpoint"},
		"gateway_labels":          {"node_id", "key", "value"},
		"gateway_listeners":       {"node_id", "position", "protocol", "bind", "port", "enabled"},
		"gateway_port_ranges":     {"node_id", "protocol", "position", "min_port", "max_port"},
		"gateway_egress_values":   {"node_id", "kind", "position", "value"},
		"agent_specs":             {"node_id", "limits_max_connections", "limits_max_streams", "limits_max_buffer_bytes", "logging_level", "logging_format", "egress_enabled", "egress_max_connections"},
		"agent_selector_labels":   {"node_id", "key", "value"},
		"agent_proxies":           {"node_id", "position", "id", "protocol", "bind", "route", "enabled"},
		"agent_routes":            {"node_id", "position", "name", "destination", "enabled"},
		"agent_route_values":      {"node_id", "route_position", "kind", "position", "value"},
		"agent_egress_values":     {"node_id", "kind", "position", "value"},
		"services":                {"id", "agent_id", "protocol", "local_target", "public_bind", "public_port", "enabled", "revision", "updated_at"},
		"service_selector_labels": {"service_id", "key", "value"},
		"assignments":             {"id", "gateway_id", "agent_id", "generation", "state", "public_endpoint", "obfuscation_mode", "obfuscation_key_ciphertext", "obfuscation_previous_key_ciphertext", "obfuscation_key_id", "obfuscation_previous_key_id", "obfuscation_max_padding_bytes", "obfuscation_handshake_shaping", "revision", "updated_at"},
		"assignment_services":     {"assignment_id", "position", "service_id"},
		"assignment_bindings":     {"assignment_id", "position", "service_id", "gateway_id", "protocol", "bind", "port"},
		"assignment_acks":         {"assignment_id", "node_id", "generation", "status", "error_code", "updated_at"},
		"desired_snapshots":       {"node_id", "generation", "checksum", "payload_json", "created_at"},
		"observed_states":         {"node_id", "generation", "protocol_version", "healthy", "degraded", "last_error_code", "last_error_path", "last_error_message", "observed_at", "updated_at", "active_streams", "active_sessions", "active_egress", "udp_oversize_drops", "geoip_up", "active_connections", "active_flows", "runtime_bytes_in_total", "runtime_bytes_out_total", "runtime_opened_total", "runtime_closed_total", "runtime_rejected_total", "runtime_rate_limited_total", "runtime_telemetry_dropped_total"},
		"observed_sessions":       {"node_id", "position", "id", "peer_id", "started_at", "streams"},
		"observed_listeners":      {"node_id", "position", "protocol", "bind", "port", "ready"},
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
			if err := db.QueryRowContext(ctx, dialect.columnExistsQuery(), table, column).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				return fmt.Errorf("required column %q.%q is missing", table, column)
			}
		}
	}
	for _, index := range append([]string{"idx_node_specs_kind", "idx_services_agent", "idx_assignments_gateway", "idx_assignments_agent", "idx_assignment_services_service", "idx_assignment_bindings_gateway", "idx_node_labels_value", "idx_assignment_acks_generation", "idx_audit_created", "idx_idempotency_created", "idx_node_bootstraps_expires"}, runtimeSchemaIndexes()...) {
		var count int
		if err := db.QueryRowContext(ctx, dialect.indexExistsQuery(), index).Scan(&count); err != nil {
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
		"schema_meta", "users", "api_tokens", "nodes", "node_labels", "node_specs",
		"gateway_specs", "gateway_endpoints", "gateway_labels", "gateway_listeners", "gateway_port_ranges", "gateway_egress_values",
		"agent_specs", "agent_selector_labels", "agent_proxies", "agent_routes", "agent_route_values", "agent_egress_values",
		"services", "service_selector_labels", "assignments", "assignment_services", "assignment_bindings", "assignment_acks",
		"desired_snapshots", "observed_states", "observed_sessions", "observed_listeners", "audit_events", "idempotency_keys",
		"enrollment_tokens", "node_bootstraps",
		"runtime_connections", "runtime_events", "runtime_traffic_rollups", "runtime_settings",
	}
}

func (s *Repository) Close() error {
	s.close.Do(func() {
		// Wake all process-local subscribers before closing the database so
		// long-lived control streams do not retain goroutines after shutdown.
		s.ChangeBus().Close()
		s.err = s.db.Close()
	})
	return s.err
}

func (s *Repository) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
