package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	driverName          = "sqlite"
	currentDBSchema     = 5
	dbSchemaFingerprint = "asterferry-controller-sqlite-v5"
	maxSnapshotDocument = 16 << 20
)

// ErrIncompatibleDatabase is returned deliberately instead of attempting an
// in-place migration. Controller state is a new generation: an operator must
// create a fresh database with `controller init` (or restore a backup made by
// this generation) when the marker is absent or different.
var ErrIncompatibleDatabase = errors.New("controller database belongs to an incompatible generation")

func OpenStore(path string, masterKey []byte) (*Store, error) {
	if len(masterKey) != masterKeyBytes {
		return nil, errors.New("master key must contain exactly 32 bytes")
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("database path is required")
	}
	dsn := path
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve database path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
		dsn = abs
	}
	dsn = sqliteDSN(dsn)
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	copy(store.masterKey[:], masterKey)
	if err := store.configure(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func NewStore(path string, masterKey []byte) (*Store, error) { return OpenStore(path, masterKey) }

func (s *Store) Path() string { return s.path }

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

func (s *Store) migrate(ctx context.Context) error {
	compatible, empty, err := inspectDatabase(ctx, s.db)
	if err != nil {
		return err
	}
	if !empty && !compatible {
		return fmt.Errorf("%w: expected schema %d (%s)", ErrIncompatibleDatabase, currentDBSchema, dbSchemaFingerprint)
	}
	if compatible {
		if err := validateRequiredTables(ctx, s.db); err != nil {
			return fmt.Errorf("%w: %v", ErrIncompatibleDatabase, err)
		}
		return nil
	}
	statements := []string{
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
			role TEXT NOT NULL,
			name TEXT NOT NULL,
			labels_json BLOB NOT NULL,
			enabled INTEGER NOT NULL,
			certificate_state TEXT NOT NULL,
			certificate_serial TEXT,
			revision INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE gateway_specs (node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE, document_json BLOB NOT NULL, revision INTEGER NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE agent_specs (node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE, document_json BLOB NOT NULL, revision INTEGER NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE services (id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, document_json BLOB NOT NULL, revision INTEGER NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE service_bindings (service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE, gateway_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, protocol TEXT NOT NULL, bind TEXT NOT NULL, port INTEGER NOT NULL, PRIMARY KEY(service_id), UNIQUE(gateway_id, protocol, bind, port))`,
		`CREATE TABLE assignments (id TEXT PRIMARY KEY, gateway_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, agent_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, document_json BLOB NOT NULL, generation INTEGER NOT NULL, revision INTEGER NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE assignment_services (assignment_id TEXT NOT NULL REFERENCES assignments(id) ON DELETE CASCADE, service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE, PRIMARY KEY(assignment_id,service_id), UNIQUE(service_id))`,
		`CREATE TABLE assignment_acks (assignment_id TEXT NOT NULL REFERENCES assignments(id) ON DELETE CASCADE, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, generation INTEGER NOT NULL, status TEXT NOT NULL, error_code TEXT, updated_at TEXT NOT NULL, PRIMARY KEY(assignment_id, node_id))`,
		`CREATE TABLE desired_snapshots (node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE, generation INTEGER NOT NULL, checksum TEXT NOT NULL, document_json BLOB NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE observed_states (node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE, generation INTEGER NOT NULL, document_json BLOB NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE audit_events (id INTEGER PRIMARY KEY AUTOINCREMENT, actor TEXT NOT NULL, action TEXT NOT NULL, resource TEXT NOT NULL, resource_id TEXT NOT NULL, revision INTEGER NOT NULL, attributes_json BLOB, created_at TEXT NOT NULL)`,
		`CREATE TABLE idempotency_keys (key TEXT PRIMARY KEY, request_hash TEXT NOT NULL, response_json BLOB NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE enrollment_tokens (id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, role TEXT NOT NULL, expires_at TEXT NOT NULL, used_at TEXT, created_at TEXT NOT NULL)`,
		`CREATE INDEX idx_nodes_role_enabled ON nodes(role, enabled)`,
		`CREATE INDEX idx_services_agent ON services(agent_id)`,
		`CREATE INDEX idx_assignments_gateway ON assignments(gateway_id)`,
		`CREATE INDEX idx_assignments_agent ON assignments(agent_id)`,
		`CREATE INDEX idx_assignment_services_service ON assignment_services(service_id)`,
		`CREATE INDEX idx_assignment_acks_generation ON assignment_acks(assignment_id,generation)`,
		`CREATE INDEX idx_audit_created ON audit_events(created_at)`,
		`CREATE INDEX idx_idempotency_created ON idempotency_keys(created_at)`,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_meta(key, value) VALUES ('schema_version', ?), ('fingerprint', ?)`, strconv.Itoa(currentDBSchema), dbSchemaFingerprint); err != nil {
		_ = tx.Rollback()
		return err
	}
	return s.commitAndNotify(tx)
}

// inspectDatabase distinguishes a genuinely new SQLite file from an existing
// database. SQLite creates no user tables until the first migration, so this
// check is safe before any CREATE statement runs.
func inspectDatabase(ctx context.Context, db *sql.DB) (compatible, empty bool, err error) {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&count); err != nil {
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
	return version == strconv.Itoa(currentDBSchema) && fingerprint == dbSchemaFingerprint, false, nil
}

func validateRequiredTables(ctx context.Context, db *sql.DB) error {
	for _, table := range currentSchemaTables() {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
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
		"schema_meta":         {"key", "value"},
		"users":               {"id", "username", "password_hash", "password_changed_at", "role", "enabled", "revision", "created_at", "updated_at"},
		"api_tokens":          {"id", "user_id", "token_hash", "name", "expires_at", "revoked_at", "created_at"},
		"nodes":               {"id", "role", "name", "labels_json", "enabled", "certificate_state", "certificate_serial", "revision", "created_at", "updated_at"},
		"gateway_specs":       {"node_id", "document_json", "revision", "updated_at"},
		"agent_specs":         {"node_id", "document_json", "revision", "updated_at"},
		"services":            {"id", "agent_id", "document_json", "revision", "updated_at"},
		"service_bindings":    {"service_id", "gateway_id", "protocol", "bind", "port"},
		"assignments":         {"id", "gateway_id", "agent_id", "document_json", "generation", "revision", "updated_at"},
		"assignment_services": {"assignment_id", "service_id"},
		"assignment_acks":     {"assignment_id", "node_id", "generation", "status", "error_code", "updated_at"},
		"desired_snapshots":   {"node_id", "generation", "checksum", "document_json", "created_at"},
		"observed_states":     {"node_id", "generation", "document_json", "updated_at"},
		"audit_events":        {"id", "actor", "action", "resource", "resource_id", "revision", "attributes_json", "created_at"},
		"idempotency_keys":    {"key", "request_hash", "response_json", "created_at"},
		"enrollment_tokens":   {"id", "token_hash", "role", "expires_at", "used_at", "created_at"},
	}
	for table, columns := range requiredColumns {
		for _, column := range columns {
			var count int
			query := fmt.Sprintf("SELECT count(*) FROM pragma_table_info('%s') WHERE name=?", table)
			if err := db.QueryRowContext(ctx, query, column).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				return fmt.Errorf("required column %q.%q is missing", table, column)
			}
		}
	}
	for _, index := range []string{"idx_nodes_role_enabled", "idx_services_agent", "idx_assignments_gateway", "idx_assignments_agent", "idx_assignment_services_service", "idx_assignment_acks_generation", "idx_audit_created", "idx_idempotency_created"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("required index %q is missing", index)
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.close.Do(func() {
		// Wake action subscribers before closing SQLite so long-lived control
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
		s.err = s.db.Close()
	})
	return s.err
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
