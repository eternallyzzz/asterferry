package controller

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"asterferry/internal/domain"
	_ "modernc.org/sqlite"
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

type Store struct {
	db           *sql.DB
	path         string
	masterKey    [masterKeyBytes]byte
	metrics      *ControllerMetrics
	close        sync.Once
	err          error
	actionMu     sync.Mutex
	actionSubs   map[string]map[uint64]*actionSubscription
	snapshotSubs map[string]map[uint64]*snapshotSubscription
	changeMu     sync.Mutex
	changeSubs   map[uint64]*resourceChangeSubscription
	// Snapshot materialization reads a previous generation and then writes a
	// replacement. Serialize that check-and-write pair so concurrent control
	// streams cannot publish the same generation with different content.
	snapshotMu sync.Mutex
}

type RevisionConflictError struct {
	Resource string
	Expected int64
	Actual   int64
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("%s revision conflict: expected %d, current %d", e.Resource, e.Expected, e.Actual)
}

func IsRevisionConflict(err error) bool {
	var conflict *RevisionConflictError
	return errors.As(err, &conflict)
}

type WriteOptions struct {
	IfMatch        int64
	IdempotencyKey string
	Actor          string
}

type AuditRecord struct {
	ID         int64             `json:"id"`
	Actor      string            `json:"actor"`
	Action     string            `json:"action"`
	Resource   string            `json:"resource"`
	ResourceID string            `json:"resource_id"`
	Revision   int64             `json:"revision"`
	Attributes map[string]string `json:"attributes,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

type SnapshotRecord struct {
	NodeID     string
	Generation uint64
	Checksum   string
	Document   []byte
	CreatedAt  time.Time
}

type ObservedRecord struct {
	NodeID     string
	Generation uint64
	Document   []byte
	UpdatedAt  time.Time
}

// RecordEvent persists a node-originated event in the same audit stream used
// by API writes. Event payloads are intentionally bounded and stored as
// attributes rather than allowing arbitrary SQL-visible columns.
func (s *Store) RecordEvent(ctx context.Context, actor, eventID, eventType, message, resourceID string, attributes map[string]string) error {
	eventID = strings.TrimSpace(eventID)
	eventType = strings.TrimSpace(eventType)
	if len(eventID) > 128 || len(eventType) == 0 || len(eventType) > 128 || strings.ContainsAny(eventID+eventType, "\x00\r\n") {
		return errors.New("event id or type is invalid")
	}
	if len(message) > 4096 || len(attributes) > 64 {
		return errors.New("event payload is too large")
	}
	for key, value := range attributes {
		if len(key) > 128 || len(value) > 2048 || strings.ContainsAny(key, "\x00\r\n") || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("event attribute is invalid")
		}
	}
	if len(attributes)+2 > 64 {
		return errors.New("event attributes are too numerous")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertAudit(ctx, tx, actor, "event:"+strings.TrimSpace(eventType), "event", strings.TrimSpace(resourceID), 0, attributesWithMessage(attributes, message, eventID)); err != nil {
		return err
	}
	return tx.Commit()
}

func attributesWithMessage(attributes map[string]string, message, eventID string) map[string]string {
	result := make(map[string]string, len(attributes)+2)
	for key, value := range attributes {
		result[key] = value
	}
	if eventID != "" {
		result["event_id"] = eventID
	}
	if message != "" {
		result["message"] = message
	}
	return result
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,username,role,enabled,revision,created_at,updated_at,password_changed_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []User{}
	for rows.Next() {
		user, scanErr := scanUser(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, user)
	}
	return result, rows.Err()
}

func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT id,username,role,enabled,revision,created_at,updated_at,password_changed_at FROM users WHERE id=?`, id))
}

func getUserTx(ctx context.Context, tx *sql.Tx, field, value string) (User, error) {
	if field != "id" && field != "username" {
		return User{}, errors.New("invalid user lookup field")
	}
	query := `SELECT id,username,role,enabled,revision,created_at,updated_at,password_changed_at FROM users WHERE ` + field + `=?`
	return scanUser(tx.QueryRowContext(ctx, query, value))
}

/*
The public User model deliberately does not expose PasswordChangedAt, but
the controller keeps it on every authenticated copy so in-memory sessions
can be invalidated without retaining or comparing password material.
*/
func scanUser(row scanner) (User, error) {
	var user User
	var enabled int
	var created, updated, passwordChanged string
	if err := row.Scan(&user.ID, &user.Username, &user.Role, &enabled, &user.Revision, &created, &updated, &passwordChanged); err != nil {
		return User{}, err
	}
	user.Enabled = enabled != 0
	var err error
	user.CreatedAt, err = parseStoredTime("user.created_at", created)
	if err != nil {
		return User{}, err
	}
	user.UpdatedAt, err = parseStoredTime("user.updated_at", updated)
	if err != nil {
		return User{}, err
	}
	user.PasswordChangedAt, err = parseStoredTime("user.password_changed_at", passwordChanged)
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// ApplySnapshot commits a complete desired state and its derived resources in
// one transaction. A failed resource or audit insert rolls the whole write
// back, so nodes never receive a partially updated generation.
func (s *Store) ApplySnapshot(ctx context.Context, snapshot domain.DesiredSnapshot, options WriteOptions) error {
	for index := range snapshot.Assignments {
		if snapshot.Assignments[index].State == "" {
			snapshot.Assignments[index].State = domain.AssignmentPending
		}
		// AssignmentApplied is an observed/controller-owned state.  A complete
		// snapshot write is allowed to publish pending, degraded, or draining
		// placement data, but it must never be used as a shortcut to open a
		// listener without the two-sided Gateway/Agent acknowledgement barrier.
		if snapshot.Assignments[index].State == domain.AssignmentApplied {
			return &domain.ApplyError{Code: "state_controller_owned", Path: fmt.Sprintf("assignments[%d].state", index), Message: "assignment state applied is controller-owned"}
		}
	}
	if snapshot.Gateway != nil {
		if err := s.protectObfuscationPolicy(&snapshot.Gateway.Obfuscation); err != nil {
			return err
		}
	}
	for index := range snapshot.Assignments {
		if err := s.protectObfuscationPolicy(&snapshot.Assignments[index].Obfuscation); err != nil {
			return fmt.Errorf("assignment %q obfuscation: %w", snapshot.Assignments[index].ID, err)
		}
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if snapshot.Generation > math.MaxInt64 {
		return &domain.ApplyError{Code: "invalid_generation", Path: "generation", Message: "generation exceeds repository limit"}
	}
	withChecksum, err := snapshot.WithChecksum()
	if err != nil {
		return err
	}
	if snapshot.Checksum != "" && !strings.EqualFold(snapshot.Checksum, withChecksum.Checksum) {
		return &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "snapshot checksum does not match its content"}
	}
	snapshot = withChecksum
	document, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if len(document) > maxSnapshotDocument {
		return &domain.ApplyError{Code: "message_too_large", Message: "desired snapshot exceeds repository limit"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	requestSnapshot := snapshotForIdempotency(snapshot)
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, requestSnapshot)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	var currentGeneration uint64
	var current []byte
	err = tx.QueryRowContext(ctx, `SELECT generation,document_json FROM desired_snapshots WHERE node_id=?`, snapshot.NodeID).Scan(&currentGeneration, &current)
	if err == nil && snapshot.Generation <= currentGeneration {
		expected := currentGeneration
		if expected < math.MaxUint64 {
			expected++
		}
		return &RevisionConflictError{Resource: "desired_snapshot", Expected: uint64ToRevision(expected), Actual: uint64ToRevision(snapshot.Generation)}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if snapshot.Gateway != nil {
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM nodes WHERE id=?`, snapshot.Gateway.NodeID).Scan(&role); err != nil {
			return err
		}
		if role != domain.RoleGateway {
			return errors.New("snapshot gateway node has the wrong role")
		}
	}
	if snapshot.Agent != nil {
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM nodes WHERE id=?`, snapshot.Agent.NodeID).Scan(&role); err != nil {
			return err
		}
		if role != domain.RoleAgent {
			return errors.New("snapshot agent node has the wrong role")
		}
	}
	// A gateway snapshot is complete for that gateway, so replacing its
	// binding rows first releases ports removed from the desired generation.
	// An agent snapshot is scoped to the agent and only releases bindings for
	// the services it owns.
	if snapshot.Gateway != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE gateway_id=?`, snapshot.NodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE gateway_id=?`, snapshot.NodeID); err != nil {
			return err
		}
	} else {
		// Agent snapshots are complete for the agent. Remove all of its old
		// bindings, assignments and service documents before inserting the new
		// generation; the surrounding transaction restores them if validation
		// or any later insert fails.
		if _, err := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id IN (SELECT id FROM services WHERE agent_id=?)`, snapshot.NodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE agent_id=?`, snapshot.NodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE agent_id=?`, snapshot.NodeID); err != nil {
			return err
		}
	}
	// A node-scoped snapshot can be applied independently while another node
	// is offline. Re-check the global service/assignment invariants after the
	// scoped cleanup so a partial apply cannot silently steal a service or a
	// public binding from a different assignment.
	for _, service := range snapshot.Services {
		var existingAgent string
		if err := tx.QueryRowContext(ctx, `SELECT agent_id FROM services WHERE id=?`, service.ID).Scan(&existingAgent); err == nil {
			if existingAgent != service.AgentID {
				return &domain.ApplyError{Code: "resource_conflict", Path: "services", Message: fmt.Sprintf("service %q belongs to another agent", service.ID)}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	for _, assignment := range snapshot.Assignments {
		// Assignment IDs are stable identities. A node-scoped snapshot may
		// replace rows owned by that node, but it must never overwrite an
		// assignment that belongs to a different Gateway/Agent pair. The
		// regular PutAssignment path intentionally permits an atomic failover;
		// snapshots, however, are complete documents and an owner mismatch is
		// always a stale or corrupted Controller view.
		var existingGateway, existingAgent string
		if err := tx.QueryRowContext(ctx, `SELECT gateway_id,agent_id FROM assignments WHERE id=?`, assignment.ID).Scan(&existingGateway, &existingAgent); err == nil {
			if existingGateway != assignment.GatewayID || existingAgent != assignment.AgentID {
				return &domain.ApplyError{Code: "resource_conflict", Path: "assignments", Message: fmt.Sprintf("assignment %q belongs to another gateway or agent", assignment.ID)}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, serviceID := range assignment.ServiceIDs {
			assigned, err := serviceAssignedElsewhere(ctx, tx, serviceID, assignment.ID)
			if err != nil {
				return err
			}
			if assigned {
				return &domain.ApplyError{Code: "resource_conflict", Path: "assignments", Message: fmt.Sprintf("service %q is already assigned", serviceID)}
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO desired_snapshots(node_id,generation,checksum,document_json,created_at) VALUES(?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET generation=excluded.generation,checksum=excluded.checksum,document_json=excluded.document_json,created_at=excluded.created_at`, snapshot.NodeID, snapshot.Generation, snapshot.Checksum, document, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if snapshot.Gateway != nil {
		value, err := json.Marshal(snapshot.Gateway)
		if err != nil {
			return fmt.Errorf("encode gateway snapshot: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_specs(node_id,document_json,revision,updated_at) VALUES(?,?,1,?) ON CONFLICT(node_id) DO UPDATE SET document_json=excluded.document_json,revision=gateway_specs.revision+1,updated_at=excluded.updated_at`, snapshot.Gateway.NodeID, value, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if snapshot.Agent != nil {
		value, err := json.Marshal(snapshot.Agent)
		if err != nil {
			return fmt.Errorf("encode agent snapshot: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_specs(node_id,document_json,revision,updated_at) VALUES(?,?,1,?) ON CONFLICT(node_id) DO UPDATE SET document_json=excluded.document_json,revision=agent_specs.revision+1,updated_at=excluded.updated_at`, snapshot.Agent.NodeID, value, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if snapshot.Agent != nil {
		// Agent snapshots are authoritative for the services owned by that
		// Agent, so replace their documents in the same transaction.
		for _, service := range snapshot.Services {
			value, err := json.Marshal(service)
			if err != nil {
				return fmt.Errorf("encode service %q: %w", service.ID, err)
			}
			var serviceRole string
			if err := tx.QueryRowContext(ctx, `SELECT role FROM nodes WHERE id=?`, service.AgentID).Scan(&serviceRole); err != nil {
				return fmt.Errorf("service %q agent: %w", service.ID, err)
			}
			if serviceRole != domain.RoleAgent {
				return fmt.Errorf("service %q agent has the wrong role", service.ID)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO services(id,agent_id,document_json,revision,updated_at) VALUES(?,?,?,1,?) ON CONFLICT(id) DO UPDATE SET agent_id=excluded.agent_id,document_json=excluded.document_json,revision=services.revision+1,updated_at=excluded.updated_at`, service.ID, service.AgentID, value, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
	} else {
		// A Gateway snapshot only references service documents owned by Agents.
		// Never let a stale Gateway stream overwrite those authoritative rows;
		// require the referenced content to match what is already stored.
		for _, service := range snapshot.Services {
			var existingDocument []byte
			if err := tx.QueryRowContext(ctx, `SELECT document_json FROM services WHERE id=?`, service.ID).Scan(&existingDocument); err != nil {
				return fmt.Errorf("gateway snapshot service %q: %w", service.ID, err)
			}
			var existing domain.Service
			if err := json.Unmarshal(existingDocument, &existing); err != nil {
				return fmt.Errorf("decode gateway snapshot service %q: %w", service.ID, err)
			}
			if !sameServiceContent(existing, service) {
				return &domain.ApplyError{Code: "resource_conflict", Path: "services", Message: fmt.Sprintf("gateway snapshot service %q is not current", service.ID)}
			}
		}
	}
	for _, assignment := range snapshot.Assignments {
		if err := assignment.Validate(); err != nil {
			return err
		}
		var gatewayRole, agentRole string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM nodes WHERE id=?`, assignment.GatewayID).Scan(&gatewayRole); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT role FROM nodes WHERE id=?`, assignment.AgentID).Scan(&agentRole); err != nil {
			return err
		}
		if gatewayRole != domain.RoleGateway || agentRole != domain.RoleAgent {
			return errors.New("assignment node roles are invalid")
		}
		var gatewayLabelsJSON []byte
		if err := tx.QueryRowContext(ctx, `SELECT labels_json FROM nodes WHERE id=?`, assignment.GatewayID).Scan(&gatewayLabelsJSON); err != nil {
			return err
		}
		var gatewayLabels map[string]string
		if len(gatewayLabelsJSON) > 0 {
			if err := json.Unmarshal(gatewayLabelsJSON, &gatewayLabels); err != nil {
				return fmt.Errorf("decode gateway labels: %w", err)
			}
		}
		var gatewayPortPool domain.PortPool
		var gatewaySpecDocument []byte
		var gatewaySpec domain.GatewaySpec
		haveGatewaySpec := false
		if snapshot.Gateway != nil && snapshot.Gateway.NodeID == assignment.GatewayID {
			gatewayPortPool = snapshot.Gateway.PortPool
			gatewaySpec = *snapshot.Gateway
			haveGatewaySpec = true
		} else if err := tx.QueryRowContext(ctx, `SELECT document_json FROM gateway_specs WHERE node_id=?`, assignment.GatewayID).Scan(&gatewaySpecDocument); err == nil {
			if err := json.Unmarshal(gatewaySpecDocument, &gatewaySpec); err != nil {
				return fmt.Errorf("decode gateway spec: %w", err)
			}
			gatewayPortPool = gatewaySpec.PortPool
			haveGatewaySpec = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		serviceSet := make(map[string]struct{}, len(assignment.ServiceIDs))
		for _, serviceID := range assignment.ServiceIDs {
			var serviceAgent string
			var serviceDocument []byte
			if err := tx.QueryRowContext(ctx, `SELECT agent_id,document_json FROM services WHERE id=?`, serviceID).Scan(&serviceAgent, &serviceDocument); err != nil {
				return fmt.Errorf("assignment service %q: %w", serviceID, err)
			}
			if serviceAgent != assignment.AgentID {
				return fmt.Errorf("assignment service %q belongs to another agent", serviceID)
			}
			var service domain.Service
			if err := json.Unmarshal(serviceDocument, &service); err != nil {
				return fmt.Errorf("assignment service %q: %w", serviceID, err)
			}
			if !service.GatewaySelector.Matches(gatewayLabels) {
				return &domain.ApplyError{Code: "selector_mismatch", Path: "assignments", Message: fmt.Sprintf("gateway %q does not match service %q selector", assignment.GatewayID, serviceID)}
			}
			serviceSet[serviceID] = struct{}{}
		}
		value, err := json.Marshal(assignment)
		if err != nil {
			return fmt.Errorf("encode assignment %q: %w", assignment.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignments(id,gateway_id,agent_id,document_json,generation,revision,updated_at) VALUES(?,?,?,?,?,1,?) ON CONFLICT(id) DO UPDATE SET gateway_id=excluded.gateway_id,agent_id=excluded.agent_id,document_json=excluded.document_json,generation=excluded.generation,revision=assignments.revision+1,updated_at=excluded.updated_at`, assignment.ID, assignment.GatewayID, assignment.AgentID, value, assignment.Generation, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if err := replaceAssignmentServicesTx(ctx, tx, assignment); err != nil {
			return err
		}
		// Degraded and draining placements have relinquished their public
		// listeners. Keep binding metadata in the assignment document for
		// diagnostics/failover, but do not reinsert it into the occupancy index.
		if assignment.State != domain.AssignmentDegraded && assignment.State != domain.AssignmentDraining {
			for _, binding := range assignment.Bindings {
				if _, ok := serviceSet[binding.ServiceID]; !ok {
					return fmt.Errorf("assignment binding references unknown service %q", binding.ServiceID)
				}
				if (snapshot.Gateway != nil || len(gatewaySpecDocument) > 0) && !portInPool(gatewayPortPool, binding.Protocol, binding.Port) {
					return &domain.ApplyError{Code: "port_outside_pool", Path: "assignments", Message: fmt.Sprintf("binding port %d is outside gateway %q %s port pool", binding.Port, assignment.GatewayID, binding.Protocol)}
				}
				if haveGatewaySpec {
					for _, listener := range gatewaySpec.Listeners {
						if bindingKey(listener.Protocol, listener.Bind, listener.Port) == bindingKey(binding.Protocol, binding.Bind, binding.Port) {
							return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
						}
					}
				}
				if _, bindingErr := tx.ExecContext(ctx, `INSERT INTO service_bindings(service_id,gateway_id,protocol,bind,port) VALUES(?,?,?,?,?) ON CONFLICT(service_id) DO UPDATE SET gateway_id=excluded.gateway_id,protocol=excluded.protocol,bind=excluded.bind,port=excluded.port`, binding.ServiceID, assignment.GatewayID, binding.Protocol, normalizeBind(binding.Bind), binding.Port); bindingErr != nil {
					if isSQLiteUniqueConstraint(bindingErr) {
						return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
					}
					return bindingErr
				}
			}
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "apply", "desired_snapshot", snapshot.NodeID, int64(snapshot.Generation), map[string]string{"checksum": snapshot.Checksum}); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, requestSnapshot, map[string]any{"node_id": snapshot.NodeID, "generation": snapshot.Generation, "checksum": snapshot.Checksum}); err != nil {
		return err
	}
	affectedNodes := []string{snapshot.NodeID}
	for _, assignment := range snapshot.Assignments {
		affectedNodes = append(affectedNodes, assignment.GatewayID, assignment.AgentID)
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

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

func (s *Store) CreateNode(ctx context.Context, node domain.Node, options WriteOptions) error {
	if err := validateNode(node); err != nil {
		return err
	}
	requestNode := node
	requestNode.Revision = 0
	requestNode.CreatedAt = time.Time{}
	requestNode.UpdatedAt = time.Time{}
	idempotentRequest := struct {
		Node domain.Node `json:"node"`
	}{Node: requestNode}
	// Revisions are assigned by the repository.  Accepting a caller-supplied
	// revision would allow a newly created resource to jump over future
	// If-Match values and would make idempotent retries ambiguous.
	node.Revision = 1
	now := time.Now().UTC()
	node.CreatedAt = now
	node.UpdatedAt = now
	labels, err := json.Marshal(node.Labels)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, idempotentRequest)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO nodes(id, role, name, labels_json, enabled, certificate_state, certificate_serial, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, node.ID, node.Role, node.Name, labels, boolInt(node.Enabled), defaultCertificateState(node.CertificateState), node.CertificateSerial, node.Revision, node.CreatedAt.Format(time.RFC3339Nano), node.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	if err := insertAudit(ctx, tx, options.Actor, "create", "node", node.ID, node.Revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"id": node.ID, "revision": node.Revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, node.ID)
}

func (s *Store) GetNode(ctx context.Context, id string) (domain.Node, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, role, name, labels_json, enabled, certificate_state, certificate_serial, revision, created_at, updated_at FROM nodes WHERE id = ?`, id)
	return scanNode(row)
}

func (s *Store) ListNodes(ctx context.Context, role string) ([]domain.Node, error) {
	query := `SELECT id, role, name, labels_json, enabled, certificate_state, certificate_serial, revision, created_at, updated_at FROM nodes`
	args := []any{}
	if strings.TrimSpace(role) != "" {
		query += ` WHERE role = ?`
		args = append(args, role)
	}
	query += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Node{}
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

// GatewayView and AgentView are one-query list projections used by the REST
// list endpoints. Keeping the optional spec in the projection avoids an N+1
// Get*Spec query for every node while preserving the existing JSON response.
type GatewayView struct {
	Node domain.Node
	Spec *domain.GatewaySpec
}

type AgentView struct {
	Node domain.Node
	Spec *domain.AgentSpec
}

func (s *Store) ListGatewayViews(ctx context.Context) ([]GatewayView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id,n.role,n.name,n.labels_json,n.enabled,n.certificate_state,n.certificate_serial,n.revision,n.created_at,n.updated_at,g.document_json,g.revision FROM nodes n LEFT JOIN gateway_specs g ON g.node_id=n.id WHERE n.role=? ORDER BY n.id`, domain.RoleGateway)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]GatewayView, 0)
	for rows.Next() {
		var node domain.Node
		var labels, document []byte
		var enabled int
		var created, updated string
		var specRevision sql.NullInt64
		if err := rows.Scan(&node.ID, &node.Role, &node.Name, &labels, &enabled, &node.CertificateState, &node.CertificateSerial, &node.Revision, &created, &updated, &document, &specRevision); err != nil {
			return nil, err
		}
		if len(labels) > 0 {
			if err := json.Unmarshal(labels, &node.Labels); err != nil {
				return nil, err
			}
		}
		node.Enabled = enabled != 0
		var parseErr error
		node.CreatedAt, parseErr = parseStoredTime("node.created_at", created)
		if parseErr != nil {
			return nil, parseErr
		}
		node.UpdatedAt, parseErr = parseStoredTime("node.updated_at", updated)
		if parseErr != nil {
			return nil, parseErr
		}
		if err := node.Validate(); err != nil {
			return nil, fmt.Errorf("stored gateway node is invalid: %w", err)
		}
		view := GatewayView{Node: node}
		if len(document) > 0 && specRevision.Valid {
			var spec domain.GatewaySpec
			if err := json.Unmarshal(document, &spec); err != nil {
				return nil, err
			}
			if spec.NodeID != node.ID {
				return nil, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "gateway.node_id", Message: "stored gateway spec node id does not match its row"}
			}
			spec.Revision = specRevision.Int64
			if err := spec.Validate(); err != nil {
				return nil, fmt.Errorf("stored gateway spec is invalid: %w", err)
			}
			view.Spec = &spec
		}
		result = append(result, view)
	}
	return result, rows.Err()
}

func (s *Store) ListAgentViews(ctx context.Context) ([]AgentView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id,n.role,n.name,n.labels_json,n.enabled,n.certificate_state,n.certificate_serial,n.revision,n.created_at,n.updated_at,a.document_json,a.revision FROM nodes n LEFT JOIN agent_specs a ON a.node_id=n.id WHERE n.role=? ORDER BY n.id`, domain.RoleAgent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AgentView, 0)
	for rows.Next() {
		var node domain.Node
		var labels, document []byte
		var enabled int
		var created, updated string
		var specRevision sql.NullInt64
		if err := rows.Scan(&node.ID, &node.Role, &node.Name, &labels, &enabled, &node.CertificateState, &node.CertificateSerial, &node.Revision, &created, &updated, &document, &specRevision); err != nil {
			return nil, err
		}
		if len(labels) > 0 {
			if err := json.Unmarshal(labels, &node.Labels); err != nil {
				return nil, err
			}
		}
		node.Enabled = enabled != 0
		var parseErr error
		node.CreatedAt, parseErr = parseStoredTime("node.created_at", created)
		if parseErr != nil {
			return nil, parseErr
		}
		node.UpdatedAt, parseErr = parseStoredTime("node.updated_at", updated)
		if parseErr != nil {
			return nil, parseErr
		}
		if err := node.Validate(); err != nil {
			return nil, fmt.Errorf("stored agent node is invalid: %w", err)
		}
		view := AgentView{Node: node}
		if len(document) > 0 && specRevision.Valid {
			var spec domain.AgentSpec
			if err := json.Unmarshal(document, &spec); err != nil {
				return nil, err
			}
			if spec.NodeID != node.ID {
				return nil, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "agent.node_id", Message: "stored agent spec node id does not match its row"}
			}
			spec.Revision = specRevision.Int64
			if err := spec.Validate(); err != nil {
				return nil, fmt.Errorf("stored agent spec is invalid: %w", err)
			}
			view.Spec = &spec
		}
		result = append(result, view)
	}
	return result, rows.Err()
}

func (s *Store) UpdateNode(ctx context.Context, node domain.Node, options WriteOptions) error {
	if err := validateNode(node); err != nil {
		return err
	}
	if options.IfMatch <= 0 {
		return &RevisionConflictError{Resource: "node", Expected: options.IfMatch, Actual: node.Revision}
	}
	requestNode := node
	idempotentRequest := struct {
		Node    domain.Node `json:"node"`
		IfMatch int64       `json:"if_match"`
	}{Node: requestNode, IfMatch: options.IfMatch}
	labels, err := json.Marshal(node.Labels)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, idempotentRequest)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	var current int64
	var currentRole, currentCertificateState string
	var currentEnabled int
	if err := tx.QueryRowContext(ctx, `SELECT revision,role,enabled,certificate_state FROM nodes WHERE id = ?`, node.ID).Scan(&current, &currentRole, &currentEnabled, &currentCertificateState); err != nil {
		return err
	}
	if current != options.IfMatch {
		return &RevisionConflictError{Resource: "node", Expected: options.IfMatch, Actual: current}
	}
	if currentRole != node.Role {
		return &domain.ApplyError{Code: "immutable_field", Path: "role", Message: "node role cannot be changed"}
	}
	affectedNodes, err := assignmentParticipantIDsTx(ctx, tx, node.ID)
	if err != nil {
		return err
	}
	affectedNodes = append(affectedNodes, node.ID)
	node.Revision = current + 1
	_, err = tx.ExecContext(ctx, `UPDATE nodes SET role=?, name=?, labels_json=?, enabled=?, certificate_state=?, certificate_serial=?, revision=?, updated_at=? WHERE id=? AND revision=?`, node.Role, node.Name, labels, boolInt(node.Enabled), defaultCertificateState(node.CertificateState), node.CertificateSerial, node.Revision, now.Format(time.RFC3339Nano), node.ID, current)
	if err != nil {
		return err
	}
	// A disabled or non-active identity must not retain an applied placement.
	// Quarantine the assignment rows in this same transaction as the identity
	// update, so a concurrent snapshot builder can never publish a new node
	// status while leaving the old public listeners authorized. The direct
	// control-stream action in the API closes online sessions immediately; this
	// durable state also covers offline nodes and reconnects.
	effectiveCertificateState := defaultCertificateState(node.CertificateState)
	quarantine := !node.Enabled || effectiveCertificateState != domain.CertificateActive
	if currentEnabled == 0 && node.Enabled && effectiveCertificateState == domain.CertificateActive {
		// Re-enabling a node does not restore a previous placement. It remains
		// degraded until the scheduler creates a fresh, acknowledged generation.
		quarantine = false
	}
	if currentCertificateState == domain.CertificateRevoked && effectiveCertificateState == domain.CertificateActive {
		// A certificate-state repair is not a placement acknowledgement. Keep
		// previously quarantined assignments closed until ScheduleAgent creates
		// and both nodes apply a fresh generation.
		quarantine = true
	}
	if quarantine {
		if err := quarantineAssignmentsForNodeTx(ctx, tx, node.ID); err != nil {
			return err
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "update", "node", node.ID, node.Revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"id": node.ID, "revision": node.Revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

// quarantineAssignmentsForNodeTx moves every placement that references an
// unavailable identity to the fail-closed degraded state. It intentionally
// preserves the assignment identity and shared generation: a later scheduler
// pass can replace the Gateway while both peers still have to acknowledge the
// new placement before it becomes applied again.
func quarantineAssignmentsForNodeTx(ctx context.Context, tx *sql.Tx, nodeID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,gateway_id,agent_id,document_json,revision,generation FROM assignments WHERE gateway_id=? OR agent_id=? ORDER BY id`, nodeID, nodeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	now := time.Now().UTC()
	for rows.Next() {
		var id, gatewayID, agentID string
		var document []byte
		var revision int64
		var indexedGeneration uint64
		if err := rows.Scan(&id, &gatewayID, &agentID, &document, &revision, &indexedGeneration); err != nil {
			return err
		}
		var assignment domain.Assignment
		if err := json.Unmarshal(document, &assignment); err != nil {
			return fmt.Errorf("decode assignment %q: %w", id, err)
		}
		if assignment.ID != id || assignment.GatewayID != gatewayID || assignment.AgentID != agentID || assignment.Generation != indexedGeneration {
			return &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment", Message: "stored assignment metadata does not match its row"}
		}
		if assignment.State == "" {
			assignment.State = domain.AssignmentPending
		}
		if assignment.State == domain.AssignmentDegraded {
			// Clear stale acknowledgements even when the state is already degraded;
			// they must never satisfy the barrier after a later repair.
			if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, id); err != nil {
				return err
			}
			if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
				return err
			}
			continue
		}
		if revision == math.MaxInt64 {
			return &domain.ApplyError{Code: "invalid_revision", Path: "assignment.revision", Message: "assignment revision is exhausted"}
		}
		assignment.State = domain.AssignmentDegraded
		assignment.Revision = revision + 1
		assignment.UpdatedAt = now
		updated, err := json.Marshal(assignment)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Revision, now.Format(time.RFC3339Nano), id, revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, id); err != nil {
			return err
		}
		if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, "system", "node_quarantine", "assignment", id, assignment.Revision, map[string]string{"node_id": nodeID, "state": domain.AssignmentDegraded}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) DeleteNode(ctx context.Context, id string, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := struct {
		ID      string `json:"id"`
		IfMatch int64  `json:"if_match"`
	}{ID: id, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM nodes WHERE id=?`, id).Scan(&revision); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: "node", Expected: options.IfMatch, Actual: revision}
	}
	// Node deletion is intentionally not a cascading business-data operation.
	// A forgotten node must be disabled or its assignments/services removed
	// explicitly first; otherwise a single identity CRUD request could silently
	// erase the last-known desired state for an Agent or release a Gateway's
	// public bindings without an auditable placement change.
	var dependents int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM assignments WHERE gateway_id=? OR agent_id=?) +
		(SELECT COUNT(*) FROM services WHERE agent_id=?) +
		(SELECT COUNT(*) FROM gateway_specs WHERE node_id=?) +
		(SELECT COUNT(*) FROM agent_specs WHERE node_id=?)`, id, id, id, id, id).Scan(&dependents); err != nil {
		return err
	}
	if dependents > 0 {
		return &domain.ApplyError{Code: "resource_conflict", Path: "node", Message: "node has dependent specs, services, or assignments"}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=? AND revision=?`, id, revision); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", "node", id, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": revision}); err != nil {
		return err
	}
	if err := s.commitAndNotifyResources(tx, id); err != nil {
		return err
	}
	if s.metrics != nil {
		s.metrics.removeNode(id)
	}
	return nil
}

func (s *Store) PutGatewaySpec(ctx context.Context, spec domain.GatewaySpec, options WriteOptions) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	node, err := s.GetNode(ctx, spec.NodeID)
	if err != nil {
		return err
	}
	if node.Role != domain.RoleGateway {
		return errors.New("gateway spec node has the wrong role")
	}
	if err := s.protectObfuscationPolicy(&spec.Obfuscation); err != nil {
		return err
	}
	return s.putDocument(ctx, "gateway_specs", spec.NodeID, spec, options)
}

func (s *Store) DeleteGatewaySpec(ctx context.Context, nodeID string, options WriteOptions) error {
	return s.deleteDocument(ctx, "gateway_specs", nodeID, options)
}

func (s *Store) GetGatewaySpec(ctx context.Context, nodeID string) (domain.GatewaySpec, error) {
	var data []byte
	var revision int64
	if err := s.db.QueryRowContext(ctx, `SELECT document_json,revision FROM gateway_specs WHERE node_id=?`, nodeID).Scan(&data, &revision); err != nil {
		return domain.GatewaySpec{}, err
	}
	var spec domain.GatewaySpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return domain.GatewaySpec{}, err
	}
	if spec.NodeID != nodeID {
		return domain.GatewaySpec{}, &domain.ApplyError{Code: "node_mismatch", Path: "gateway.node_id", Message: "stored gateway spec node id does not match its row"}
	}
	spec.Revision = revision
	if err := spec.Validate(); err != nil {
		return domain.GatewaySpec{}, fmt.Errorf("stored gateway spec is invalid: %w", err)
	}
	return spec, nil
}

func (s *Store) PutAgentSpec(ctx context.Context, spec domain.AgentSpec, options WriteOptions) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	node, err := s.GetNode(ctx, spec.NodeID)
	if err != nil {
		return err
	}
	if node.Role != domain.RoleAgent {
		return errors.New("agent spec node has the wrong role")
	}
	return s.putDocument(ctx, "agent_specs", spec.NodeID, spec, options)
}

func (s *Store) DeleteAgentSpec(ctx context.Context, nodeID string, options WriteOptions) error {
	return s.deleteDocument(ctx, "agent_specs", nodeID, options)
}

func (s *Store) GetAgentSpec(ctx context.Context, nodeID string) (domain.AgentSpec, error) {
	var data []byte
	var revision int64
	if err := s.db.QueryRowContext(ctx, `SELECT document_json,revision FROM agent_specs WHERE node_id=?`, nodeID).Scan(&data, &revision); err != nil {
		return domain.AgentSpec{}, err
	}
	var spec domain.AgentSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return domain.AgentSpec{}, err
	}
	if spec.NodeID != nodeID {
		return domain.AgentSpec{}, &domain.ApplyError{Code: "node_mismatch", Path: "agent.node_id", Message: "stored agent spec node id does not match its row"}
	}
	spec.Revision = revision
	if err := spec.Validate(); err != nil {
		return domain.AgentSpec{}, fmt.Errorf("stored agent spec is invalid: %w", err)
	}
	return spec, nil
}

func (s *Store) PutService(ctx context.Context, service domain.Service, options WriteOptions) error {
	if err := service.Validate(); err != nil {
		return err
	}
	node, err := s.GetNode(ctx, service.AgentID)
	if err != nil {
		return err
	}
	if node.Role != domain.RoleAgent {
		return errors.New("service agent has the wrong role")
	}
	return s.putServiceDocument(ctx, service, options)
}

func (s *Store) GetService(ctx context.Context, id string) (domain.Service, error) {
	var data []byte
	var revision int64
	var indexedAgent string
	if err := s.db.QueryRowContext(ctx, `SELECT agent_id,document_json,revision FROM services WHERE id=?`, id).Scan(&indexedAgent, &data, &revision); err != nil {
		return domain.Service{}, err
	}
	var service domain.Service
	if err := json.Unmarshal(data, &service); err != nil {
		return domain.Service{}, err
	}
	if service.ID != id || service.AgentID != indexedAgent {
		return domain.Service{}, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "service", Message: "stored service metadata does not match its row"}
	}
	service.Revision = revision
	if err := service.Validate(); err != nil {
		return domain.Service{}, fmt.Errorf("stored service is invalid: %w", err)
	}
	return service, nil
}

func (s *Store) ListServices(ctx context.Context, agentID string) ([]domain.Service, error) {
	query := `SELECT id,agent_id,document_json,revision FROM services`
	args := []any{}
	if strings.TrimSpace(agentID) != "" {
		query += ` WHERE agent_id=?`
		args = append(args, agentID)
	}
	query += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Service{}
	for rows.Next() {
		var id, indexedAgent string
		var data []byte
		var revision int64
		if err := rows.Scan(&id, &indexedAgent, &data, &revision); err != nil {
			return nil, err
		}
		var service domain.Service
		if err := json.Unmarshal(data, &service); err != nil {
			return nil, err
		}
		if service.ID != id || service.AgentID != indexedAgent {
			return nil, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "service", Message: "stored service metadata does not match its row"}
		}
		service.Revision = revision
		if err := service.Validate(); err != nil {
			return nil, fmt.Errorf("stored service is invalid: %w", err)
		}
		result = append(result, service)
	}
	return result, rows.Err()
}

func (s *Store) DeleteService(ctx context.Context, id string, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := struct {
		ID      string `json:"id"`
		IfMatch int64  `json:"if_match"`
	}{ID: id, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	var revision int64
	var agentID string
	if err := tx.QueryRowContext(ctx, `SELECT revision,agent_id FROM services WHERE id=?`, id).Scan(&revision, &agentID); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: "service", Expected: options.IfMatch, Actual: revision}
	}
	if assigned, assignmentErr := serviceHasAssignment(ctx, tx, id); assignmentErr != nil {
		return assignmentErr
	} else if assigned {
		return &domain.ApplyError{Code: "resource_conflict", Path: "service", Message: "assigned service cannot be deleted"}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id=? AND revision=?`, id, revision); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", "service", id, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, agentID)
}

func (s *Store) PutAssignment(ctx context.Context, assignment domain.Assignment, options WriteOptions) error {
	// API callers may omit the lifecycle field on create. Persist the safe
	// default explicitly so a newly scheduled placement cannot be admitted by
	// a node before both peers acknowledge its generation.
	if assignment.State == "" {
		assignment.State = domain.AssignmentPending
	}
	// `applied` is an observed/controller-owned state.  Allowing a REST caller
	// (or a retrying placement writer) to set it directly would bypass the
	// two-sided acknowledgement barrier and could expose a listener before the
	// Gateway and Agent have both installed the same generation.  The control
	// stream is the only path that may promote an assignment after recording
	// participant acknowledgements.
	if assignment.State == domain.AssignmentApplied {
		return &domain.ApplyError{Code: "state_controller_owned", Path: "state", Message: "assignment state applied is controller-owned"}
	}
	if err := assignment.Validate(); err != nil {
		return err
	}
	if err := s.protectObfuscationPolicy(&assignment.Obfuscation); err != nil {
		return err
	}
	if assignment.Generation > math.MaxInt64 {
		return &domain.ApplyError{Code: "invalid_generation", Path: "generation", Message: "assignment generation exceeds repository limit"}
	}
	requestAssignment := assignment
	requestAssignment.Revision = 0
	requestAssignment.UpdatedAt = time.Time{}
	requestAssignment.Obfuscation = obfuscationRequestPolicy(requestAssignment.Obfuscation)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	idempotentRequest := struct {
		Assignment domain.Assignment `json:"assignment"`
		IfMatch    int64             `json:"if_match"`
	}{Assignment: requestAssignment, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, idempotentRequest)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	// Node identity and lifecycle checks are authoritative only inside the
	// assignment transaction. A preflight GetNode can race a role/enable
	// change and allow a placement to commit against a different identity.
	var gatewayRole, agentRole string
	var gatewayEnabled, agentEnabled int
	if err := tx.QueryRowContext(ctx, `SELECT role,enabled FROM nodes WHERE id=?`, assignment.GatewayID).Scan(&gatewayRole, &gatewayEnabled); err != nil {
		return err
	}
	if gatewayRole != domain.RoleGateway {
		return errors.New("assignment gateway has the wrong role")
	}
	if gatewayEnabled == 0 {
		return &domain.ApplyError{Code: "node_disabled", Path: "gateway_id", Message: "assignment gateway is disabled"}
	}
	if err := tx.QueryRowContext(ctx, `SELECT role,enabled FROM nodes WHERE id=?`, assignment.AgentID).Scan(&agentRole, &agentEnabled); err != nil {
		return err
	}
	if agentRole != domain.RoleAgent {
		return errors.New("assignment agent has the wrong role")
	}
	if agentEnabled == 0 {
		return &domain.ApplyError{Code: "node_disabled", Path: "agent_id", Message: "assignment agent is disabled"}
	}
	var gatewayLabelsJSON []byte
	if err := tx.QueryRowContext(ctx, `SELECT labels_json FROM nodes WHERE id=?`, assignment.GatewayID).Scan(&gatewayLabelsJSON); err != nil {
		return err
	}
	var gatewayLabels map[string]string
	if len(gatewayLabelsJSON) > 0 {
		if err := json.Unmarshal(gatewayLabelsJSON, &gatewayLabels); err != nil {
			return fmt.Errorf("decode gateway labels: %w", err)
		}
	}
	var gatewaySpec domain.GatewaySpec
	var gatewaySpecDocument []byte
	if err := tx.QueryRowContext(ctx, `SELECT document_json FROM gateway_specs WHERE node_id=?`, assignment.GatewayID).Scan(&gatewaySpecDocument); err == nil {
		if err := json.Unmarshal(gatewaySpecDocument, &gatewaySpec); err != nil {
			return fmt.Errorf("decode gateway spec: %w", err)
		}
		// The public endpoint is a derived part of the assignment and must be
		// one of the currently advertised endpoints.  Without this check an
		// operator could persist a syntactically valid but unreachable endpoint
		// and the Agent would repeatedly dial a value the Gateway never owns.
		if assignment.PublicEndpoint != "" {
			endpointKnown := false
			for _, endpoint := range gatewaySpec.PublicEndpoints {
				if endpoint == assignment.PublicEndpoint {
					endpointKnown = true
					break
				}
			}
			if !endpointKnown {
				return &domain.ApplyError{Code: "endpoint_mismatch", Path: "public_endpoint", Message: fmt.Sprintf("assignment endpoint %q is not advertised by gateway %q", assignment.PublicEndpoint, assignment.GatewayID)}
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var revision int64
	var previous domain.Assignment
	var previousDocument []byte
	hadPrevious := false
	err = tx.QueryRowContext(ctx, `SELECT revision,document_json FROM assignments WHERE id=?`, assignment.ID).Scan(&revision, &previousDocument)
	if err == nil {
		hadPrevious = true
		if decodeErr := json.Unmarshal(previousDocument, &previous); decodeErr != nil {
			return fmt.Errorf("decode existing assignment: %w", decodeErr)
		}
		if assignment.Generation < previous.Generation {
			return &RevisionConflictError{Resource: "assignment_generation", Expected: uint64ToRevision(previous.Generation), Actual: uint64ToRevision(assignment.Generation)}
		}
	}
	// A degraded/draining placement is a relinquished claim during failover.
	// Collect those rows now, but defer deleting them until every validation
	// below has passed so a rejected replacement leaves the old assignment
	// untouched. Active/pending placements remain hard conflicts.
	superseded, supersedeErr := supersededAssignments(ctx, tx, assignment.ServiceIDs, assignment.ID)
	if supersedeErr != nil {
		return supersedeErr
	}
	serviceDocuments := make(map[string]domain.Service, len(assignment.ServiceIDs))
	for _, serviceID := range assignment.ServiceIDs {
		var serviceDocument []byte
		if err := tx.QueryRowContext(ctx, `SELECT document_json FROM services WHERE id=?`, serviceID).Scan(&serviceDocument); err != nil {
			return fmt.Errorf("assignment service %q: %w", serviceID, err)
		}
		var service domain.Service
		if err := json.Unmarshal(serviceDocument, &service); err != nil {
			return fmt.Errorf("assignment service %q: %w", serviceID, err)
		}
		if service.AgentID != assignment.AgentID {
			return fmt.Errorf("assignment service %q belongs to another agent", serviceID)
		}
		if !service.GatewaySelector.Matches(gatewayLabels) {
			return &domain.ApplyError{Code: "selector_mismatch", Path: "service.gateway_selector", Message: fmt.Sprintf("gateway %q does not match service %q selector", assignment.GatewayID, serviceID)}
		}
		serviceDocuments[serviceID] = service
	}
	for _, binding := range assignment.Bindings {
		service, ok := serviceDocuments[binding.ServiceID]
		if !ok {
			return fmt.Errorf("assignment binding service %q is not in service_ids", binding.ServiceID)
		}
		serviceProtocol := service.Protocol
		if serviceProtocol != binding.Protocol {
			return &domain.ApplyError{Code: "protocol_mismatch", Path: "bindings", Message: fmt.Sprintf("binding protocol for service %q does not match the service", binding.ServiceID)}
		}
		if normalizeBind(service.PublicBind) != normalizeBind(binding.Bind) {
			return &domain.ApplyError{Code: "bind_mismatch", Path: "bindings", Message: fmt.Sprintf("binding address for service %q does not match the service public bind", binding.ServiceID)}
		}
		if service.PublicPort != 0 && service.PublicPort != binding.Port {
			return &domain.ApplyError{Code: "port_mismatch", Path: "bindings", Message: fmt.Sprintf("binding port for service %q does not match the service public port", binding.ServiceID)}
		}
		if len(gatewaySpecDocument) > 0 && !portInPool(gatewaySpec.PortPool, binding.Protocol, binding.Port) {
			return &domain.ApplyError{Code: "port_outside_pool", Path: "bindings", Message: fmt.Sprintf("binding port %d is outside gateway %q %s port pool", binding.Port, assignment.GatewayID, binding.Protocol)}
		}
		for _, listener := range gatewaySpec.Listeners {
			if bindingKey(listener.Protocol, listener.Bind, listener.Port) == bindingKey(binding.Protocol, binding.Bind, binding.Port) {
				return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
			}
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		revision = 1
		if options.IfMatch > 0 {
			return &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: 0}
		}
		err = nil
	} else if err == nil {
		if options.IfMatch <= 0 || options.IfMatch != revision {
			return &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: revision}
		}
		revision++
	}
	if err != nil {
		return err
	}
	for _, old := range superseded {
		for _, oldServiceID := range old.ServiceIDs {
			if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id=?`, oldServiceID); deleteErr != nil {
				return deleteErr
			}
		}
		if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM assignments WHERE id=?`, old.ID); deleteErr != nil {
			return deleteErr
		}
		if auditErr := insertAudit(ctx, tx, options.Actor, "supersede", "assignment", old.ID, old.Revision, map[string]string{"replacement": assignment.ID}); auditErr != nil {
			return auditErr
		}
	}
	// Revisions and timestamps are repository-owned metadata. Store the
	// canonical values in the document as well as in their indexed columns so
	// a snapshot built from the row is self-consistent.
	nowTime := time.Now().UTC()
	assignment.Revision = revision
	assignment.UpdatedAt = nowTime
	b, err := json.Marshal(assignment)
	if err != nil {
		return err
	}
	if hadPrevious {
		_, err = tx.ExecContext(ctx, `UPDATE assignments SET gateway_id=?,agent_id=?,document_json=?,generation=?,revision=?,updated_at=? WHERE id=? AND revision=?`, assignment.GatewayID, assignment.AgentID, b, assignment.Generation, revision, nowTime.Format(time.RFC3339Nano), assignment.ID, revision-1)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO assignments(id,gateway_id,agent_id,document_json,generation,revision,updated_at) VALUES(?,?,?,?,?,?,?)`, assignment.ID, assignment.GatewayID, assignment.AgentID, b, assignment.Generation, revision, nowTime.Format(time.RFC3339Nano))
	}
	if err != nil {
		return err
	}
	// A placement write invalidates every participant acknowledgement, even
	// when the caller keeps the same generation. The next complete snapshot
	// application must prove that both endpoints have accepted the document;
	// retaining an acknowledgement from an older binding or endpoint would
	// otherwise open a partially applied assignment.
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, assignment.ID); err != nil {
		return err
	}
	for _, serviceID := range assignment.ServiceIDs {
		var serviceAgent string
		if err := tx.QueryRowContext(ctx, `SELECT agent_id FROM services WHERE id=?`, serviceID).Scan(&serviceAgent); err != nil {
			return fmt.Errorf("assignment service %q: %w", serviceID, err)
		}
		if serviceAgent != assignment.AgentID {
			return fmt.Errorf("assignment service %q belongs to another agent", serviceID)
		}
	}
	if err := replaceAssignmentServicesTx(ctx, tx, assignment); err != nil {
		return err
	}
	serviceSet := make(map[string]struct{}, len(assignment.ServiceIDs))
	for _, serviceID := range assignment.ServiceIDs {
		serviceSet[serviceID] = struct{}{}
	}
	if hadPrevious {
		if previous.GatewayID != assignment.GatewayID || previous.AgentID != assignment.AgentID {
			for _, oldServiceID := range previous.ServiceIDs {
				if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id=?`, oldServiceID); deleteErr != nil {
					return deleteErr
				}
			}
		}
		for _, oldServiceID := range previous.ServiceIDs {
			if _, keep := serviceSet[oldServiceID]; !keep {
				if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id=?`, oldServiceID); deleteErr != nil {
					return deleteErr
				}
			}
		}
	}
	// Degraded and draining placements have relinquished their public
	// listeners. Keep the binding metadata in the assignment document for
	// audit/failover diagnostics, but release any rows left by the previous
	// generation instead of re-inserting them into the occupancy index.
	if assignment.State == domain.AssignmentDegraded || assignment.State == domain.AssignmentDraining {
		if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
			return err
		}
	} else {
		for _, binding := range assignment.Bindings {
			if binding.Port == 0 || binding.ServiceID == "" {
				return errors.New("assignment binding requires service id and port")
			}
			if _, ok := serviceSet[binding.ServiceID]; !ok {
				return fmt.Errorf("assignment binding references unknown service %q", binding.ServiceID)
			}
			_, bindingErr := tx.ExecContext(ctx, `INSERT INTO service_bindings(service_id,gateway_id,protocol,bind,port) VALUES(?,?,?,?,?) ON CONFLICT(service_id) DO UPDATE SET gateway_id=excluded.gateway_id,protocol=excluded.protocol,bind=excluded.bind,port=excluded.port`, binding.ServiceID, assignment.GatewayID, binding.Protocol, normalizeBind(binding.Bind), binding.Port)
			if bindingErr != nil {
				if isSQLiteUniqueConstraint(bindingErr) {
					return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
				}
				return bindingErr
			}
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "upsert", "assignment", assignment.ID, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"id": assignment.ID, "revision": revision}); err != nil {
		return err
	}
	affectedNodes := []string{assignment.GatewayID, assignment.AgentID}
	if hadPrevious {
		affectedNodes = append(affectedNodes, previous.GatewayID, previous.AgentID)
	}
	for _, old := range superseded {
		affectedNodes = append(affectedNodes, old.GatewayID, old.AgentID)
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

func serviceAssignedElsewhere(ctx context.Context, tx *sql.Tx, serviceID, assignmentID string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignment_services WHERE service_id=? AND assignment_id<>?`, serviceID, assignmentID).Scan(&count)
	return count > 0, err
}

func replaceAssignmentServicesTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_services WHERE assignment_id=?`, assignment.ID); err != nil {
		return err
	}
	for _, serviceID := range assignment.ServiceIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_services(assignment_id,service_id) VALUES(?,?)`, assignment.ID, serviceID); err != nil {
			if isSQLiteUniqueConstraint(err) {
				return &domain.ApplyError{Code: "resource_conflict", Path: "service_ids", Message: fmt.Sprintf("service %q is already assigned", serviceID)}
			}
			return err
		}
	}
	return nil
}

// deleteAssignmentBindingsTx releases a placement's public-port occupancy
// rows while retaining the binding metadata in the assignment document.  A
// degraded/draining assignment is fail-closed and must not block a healthy
// replacement from claiming the same (gateway, protocol, bind, port) tuple.
func deleteAssignmentBindingsTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) error {
	for _, serviceID := range assignment.ServiceIDs {
		if strings.TrimSpace(serviceID) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id=?`, serviceID); err != nil {
			return err
		}
	}
	return nil
}

// supersededAssignments returns older placements that have already entered a
// degraded/draining lifecycle state and claim one of the services in a new
// assignment.  A failover is committed as one transaction: these rows and
// their bindings are removed only after the replacement has passed all
// placement validation.  Healthy or pending claims remain hard conflicts.
func supersededAssignments(ctx context.Context, tx *sql.Tx, serviceIDs []string, assignmentID string) ([]domain.Assignment, error) {
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(serviceIDs)), ",")
	args := make([]any, 0, len(serviceIDs)+1)
	args = append(args, assignmentID)
	for _, serviceID := range serviceIDs {
		args = append(args, serviceID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.document_json FROM assignments a JOIN assignment_services assignment_service ON assignment_service.assignment_id=a.id WHERE a.id<>? AND assignment_service.service_id IN (`+placeholders+`) GROUP BY a.id ORDER BY a.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]struct{})
	result := make([]domain.Assignment, 0)
	for rows.Next() {
		var id string
		var document []byte
		if err := rows.Scan(&id, &document); err != nil {
			return nil, err
		}
		var candidate domain.Assignment
		if err := json.Unmarshal(document, &candidate); err != nil {
			return nil, fmt.Errorf("decode assignment %q: %w", id, err)
		}
		candidate.ID = id
		if candidate.State != domain.AssignmentDegraded && candidate.State != domain.AssignmentDraining {
			return nil, &domain.ApplyError{Code: "resource_conflict", Path: "service_ids", Message: fmt.Sprintf("service is already assigned by %q", candidate.ID)}
		}
		if _, exists := seen[candidate.ID]; exists {
			continue
		}
		seen[candidate.ID] = struct{}{}
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) GetAssignment(ctx context.Context, id string) (domain.Assignment, error) {
	var data []byte
	var revision int64
	var indexedID, indexedGateway, indexedAgent string
	var indexedGeneration uint64
	if err := s.db.QueryRowContext(ctx, `SELECT id,gateway_id,agent_id,document_json,revision,generation FROM assignments WHERE id=?`, id).Scan(&indexedID, &indexedGateway, &indexedAgent, &data, &revision, &indexedGeneration); err != nil {
		return domain.Assignment{}, err
	}
	var assignment domain.Assignment
	if err := json.Unmarshal(data, &assignment); err != nil {
		return domain.Assignment{}, err
	}
	if assignment.State == "" {
		assignment.State = domain.AssignmentPending
	}
	if assignment.ID != indexedID || assignment.GatewayID != indexedGateway || assignment.AgentID != indexedAgent || assignment.Generation != indexedGeneration {
		return domain.Assignment{}, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment", Message: "stored assignment metadata does not match its row"}
	}
	assignment.Revision = revision
	if err := assignment.Validate(); err != nil {
		return domain.Assignment{}, fmt.Errorf("stored assignment is invalid: %w", err)
	}
	return assignment, nil
}

func (s *Store) DeleteAssignment(ctx context.Context, id string, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := struct {
		ID      string `json:"id"`
		IfMatch int64  `json:"if_match"`
	}{ID: id, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	var revision int64
	var gatewayID, agentID string
	var document []byte
	if err := tx.QueryRowContext(ctx, `SELECT revision,gateway_id,agent_id,document_json FROM assignments WHERE id=?`, id).Scan(&revision, &gatewayID, &agentID, &document); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: revision}
	}
	var assignment domain.Assignment
	if err := json.Unmarshal(document, &assignment); err != nil {
		return err
	}
	for _, serviceID := range assignment.ServiceIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id=?`, serviceID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE id=? AND revision=?`, id, revision); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", "assignment", id, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, gatewayID, agentID)
}

// UpdateAssignmentState changes only the lifecycle state of an assignment.
// It is used by the health reconciler and runtime actions; the assignment
// placement and its public bindings remain untouched. The revision and audit
// event are updated in the same transaction so an operator cannot overwrite
// a concurrent placement change with a stale health result.
func (s *Store) UpdateAssignmentState(ctx context.Context, id, state string, options WriteOptions) (domain.Assignment, error) {
	if strings.TrimSpace(id) == "" {
		return domain.Assignment{}, sql.ErrNoRows
	}
	if state != domain.AssignmentPending && state != domain.AssignmentApplied && state != domain.AssignmentDegraded && state != domain.AssignmentDraining {
		return domain.Assignment{}, &domain.ApplyError{Code: "invalid_assignment_state", Path: "state", Message: "assignment state is invalid"}
	}
	// `applied` is not an operator-selected lifecycle transition.  It is the
	// result of the two-sided acknowledgement barrier in applyNodeResult: both
	// the Gateway and Agent must have applied the same assignment generation
	// before the public listener is admitted.  Keeping this guard on the
	// generic state-update path prevents internal callers (or a future API
	// endpoint) from accidentally opening a placement with one-sided state.
	if state == domain.AssignmentApplied {
		return domain.Assignment{}, &domain.ApplyError{Code: "state_controller_owned", Path: "state", Message: "assignment state applied is controller-owned"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Assignment{}, err
	}
	defer tx.Rollback()
	request := struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		IfMatch int64  `json:"if_match"`
	}{ID: id, State: state, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return domain.Assignment{}, err
	}
	if hit {
		var assignment domain.Assignment
		var document []byte
		if err := tx.QueryRowContext(ctx, `SELECT document_json FROM assignments WHERE id=?`, id).Scan(&document); err != nil {
			return domain.Assignment{}, err
		}
		if err := json.Unmarshal(document, &assignment); err != nil {
			return domain.Assignment{}, err
		}
		return assignment, s.commitAndNotifyResources(tx, assignment.GatewayID, assignment.AgentID)
	}
	var revision int64
	var document []byte
	if err := tx.QueryRowContext(ctx, `SELECT revision,document_json FROM assignments WHERE id=?`, id).Scan(&revision, &document); err != nil {
		return domain.Assignment{}, err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return domain.Assignment{}, &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: revision}
	}
	var assignment domain.Assignment
	if err := json.Unmarshal(document, &assignment); err != nil {
		return domain.Assignment{}, err
	}
	assignment.State = state
	assignment.Revision = revision + 1
	assignment.UpdatedAt = time.Now().UTC()
	document, err = json.Marshal(assignment)
	if err != nil {
		return domain.Assignment{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, document, assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano), id, revision); err != nil {
		return domain.Assignment{}, err
	}
	if state == domain.AssignmentDegraded || state == domain.AssignmentDraining {
		if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
			return domain.Assignment{}, err
		}
	}
	// A manually requested lifecycle transition supersedes any participant
	// acknowledgements recorded for the previous state. They are only valid
	// after both nodes apply the next complete desired snapshot.
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, id); err != nil {
		return domain.Assignment{}, err
	}
	if err := insertAudit(ctx, tx, options.Actor, "state", "assignment", id, assignment.Revision, map[string]string{"state": state}); err != nil {
		return domain.Assignment{}, err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": assignment.Revision, "state": state}); err != nil {
		return domain.Assignment{}, err
	}
	return assignment, s.commitAndNotifyResources(tx, assignment.GatewayID, assignment.AgentID)
}

// applyNodeResult records the lifecycle consequence of a node applying its
// complete desired snapshot. The control-stream path uses
// applyNodeResultWithError to retain stable rejection error codes; this
// boolean helper remains useful to package-level tests and simple callers.
//
//lint:ignore U1000 package tests exercise the boolean compatibility helper.
func (s *Store) applyNodeResult(ctx context.Context, nodeID string, generation uint64, applied bool, actor string) ([]domain.Assignment, error) {
	return s.applyNodeResultWithError(ctx, nodeID, generation, applied, "", actor)
}

// applyNodeResultWithError records the lifecycle consequence of a node
// applying its complete desired snapshot. Assignment state is controller-
// owned, so a node can only move assignments that it participates in and only
// when the result generation is at least the assignment generation carried by
// that snapshot. The returned assignments let the caller refresh both
// node-scoped snapshots after the transaction commits. The control-stream form
// also preserves the stable rejection code in assignment_acks for diagnostics
// and auditing.
func (s *Store) applyNodeResultWithError(ctx context.Context, nodeID string, generation uint64, applied bool, errorCode, actor string) ([]domain.Assignment, error) {
	if strings.TrimSpace(nodeID) == "" || generation == 0 {
		return nil, errors.New("node result identity and generation are required")
	}
	if generation > math.MaxInt64 {
		return nil, &domain.ApplyError{Code: "invalid_generation", Path: "generation", Message: "node result generation exceeds repository limit"}
	}
	targetState := domain.AssignmentDegraded
	if applied {
		targetState = domain.AssignmentApplied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,gateway_id,agent_id,document_json,revision,generation FROM assignments WHERE gateway_id=? OR agent_id=? ORDER BY id`, nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changed := make([]domain.Assignment, 0)
	now := time.Now().UTC()
	for rows.Next() {
		var assignment domain.Assignment
		var document []byte
		var revision int64
		var assignmentGeneration uint64
		if err := rows.Scan(&assignment.ID, &assignment.GatewayID, &assignment.AgentID, &document, &revision, &assignmentGeneration); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(document, &assignment); err != nil {
			return nil, fmt.Errorf("decode assignment %q: %w", assignment.ID, err)
		}
		// The indexed generation is authoritative for this comparison; the
		// document is checked as well so a corrupted row cannot be advanced by a
		// control result that happens to name the same assignment.
		if assignment.Generation != assignmentGeneration || assignment.Generation > generation || assignment.State == domain.AssignmentDraining {
			continue
		}
		// Persist an acknowledgement for this participant before evaluating the
		// shared lifecycle state. A placement is only opened after both the
		// Gateway and Agent have applied the assignment generation; one node
		// succeeding must never make the other node accept streams prematurely.
		ackStatus := "rejected"
		if applied {
			ackStatus = "applied"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_acks(assignment_id,node_id,generation,status,error_code,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(assignment_id,node_id) DO UPDATE SET generation=excluded.generation,status=excluded.status,error_code=excluded.error_code,updated_at=excluded.updated_at`, assignment.ID, nodeID, assignment.Generation, ackStatus, strings.TrimSpace(errorCode), now.Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if applied {
			if assignment.State == domain.AssignmentDegraded {
				if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
					return nil, err
				}
				continue
			}
			if assignment.State == domain.AssignmentApplied {
				continue
			}
			ready, readyErr := assignmentParticipantsApplied(ctx, tx, assignment)
			if readyErr != nil {
				return nil, readyErr
			}
			if !ready {
				continue
			}
			assignment.State = domain.AssignmentApplied
		} else {
			// A rejected apply is fail-closed. Keep a pending placement out of
			// the data path and mark an already-applied placement degraded until a
			// new scheduling pass provides a complete healthy generation.
			if assignment.State == domain.AssignmentDegraded {
				if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
					return nil, err
				}
				continue
			}
			assignment.State = domain.AssignmentDegraded
			if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
				return nil, err
			}
		}
		assignment.Revision = revision + 1
		assignment.UpdatedAt = now
		updated, marshalErr := json.Marshal(assignment)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if _, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Revision, now.Format(time.RFC3339Nano), assignment.ID, revision); err != nil {
			return nil, err
		}
		if err := insertAudit(ctx, tx, actor, "state", "assignment", assignment.ID, assignment.Revision, map[string]string{"state": targetState, "node_id": nodeID, "generation": fmt.Sprint(generation)}); err != nil {
			return nil, err
		}
		changed = append(changed, assignment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	changedNodes := make([]string, 0, len(changed)*2)
	for _, assignment := range changed {
		changedNodes = append(changedNodes, assignment.GatewayID, assignment.AgentID)
	}
	if err := s.commitAndNotifyResources(tx, changedNodes...); err != nil {
		return nil, err
	}
	return changed, nil
}

func assignmentParticipantsApplied(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT node_id) FROM assignment_acks WHERE assignment_id=? AND generation>=? AND status='applied' AND node_id IN (?,?)`, assignment.ID, assignment.Generation, assignment.GatewayID, assignment.AgentID).Scan(&count)
	return count == 2, err
}

func (s *Store) ListAssignments(ctx context.Context, gatewayID, agentID string) ([]domain.Assignment, error) {
	query := `SELECT id,gateway_id,agent_id,document_json,revision,generation FROM assignments WHERE 1=1`
	args := []any{}
	if gatewayID != "" {
		query += ` AND gateway_id=?`
		args = append(args, gatewayID)
	}
	if agentID != "" {
		query += ` AND agent_id=?`
		args = append(args, agentID)
	}
	query += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Assignment{}
	for rows.Next() {
		var id, indexedGateway, indexedAgent string
		var data []byte
		var revision int64
		var indexedGeneration uint64
		if err := rows.Scan(&id, &indexedGateway, &indexedAgent, &data, &revision, &indexedGeneration); err != nil {
			return nil, err
		}
		var assignment domain.Assignment
		if err := json.Unmarshal(data, &assignment); err != nil {
			return nil, err
		}
		if assignment.State == "" {
			assignment.State = domain.AssignmentPending
		}
		if assignment.ID != id || assignment.GatewayID != indexedGateway || assignment.AgentID != indexedAgent || assignment.Generation != indexedGeneration {
			return nil, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment", Message: "stored assignment metadata does not match its row"}
		}
		assignment.Revision = revision
		if err := assignment.Validate(); err != nil {
			return nil, fmt.Errorf("stored assignment is invalid: %w", err)
		}
		result = append(result, assignment)
	}
	return result, rows.Err()
}

func (s *Store) SaveSnapshot(ctx context.Context, record SnapshotRecord) error {
	if record.NodeID == "" || record.Generation == 0 || record.Checksum == "" || len(record.Document) == 0 {
		return errors.New("snapshot node, generation, checksum and document are required")
	}
	if len(record.Document) > maxSnapshotDocument {
		return errors.New("snapshot document is too large")
	}
	if record.Generation > math.MaxInt64 {
		return &domain.ApplyError{Code: "invalid_generation", Path: "generation", Message: "generation exceeds repository limit"}
	}
	var snapshot domain.DesiredSnapshot
	if err := json.Unmarshal(record.Document, &snapshot); err != nil {
		return fmt.Errorf("snapshot document is invalid: %w", err)
	}
	if snapshot.Gateway != nil {
		if err := s.protectObfuscationPolicy(&snapshot.Gateway.Obfuscation); err != nil {
			return err
		}
	}
	for index := range snapshot.Assignments {
		if err := s.protectObfuscationPolicy(&snapshot.Assignments[index].Obfuscation); err != nil {
			return fmt.Errorf("assignment %q obfuscation: %w", snapshot.Assignments[index].ID, err)
		}
	}
	if protected, err := json.Marshal(snapshot); err == nil {
		record.Document = protected
	} else {
		return fmt.Errorf("encode protected snapshot: %w", err)
	}
	if snapshot.NodeID != record.NodeID || snapshot.Generation != record.Generation || !strings.EqualFold(snapshot.Checksum, record.Checksum) {
		return errors.New("snapshot metadata does not match document")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	// The encrypted node cache and control envelope both treat the checksum as
	// an integrity boundary.  Persisting a document whose claimed checksum is
	// merely self-consistent with the row metadata would let a corrupted or
	// hand-edited snapshot become the Controller's authoritative last-known
	// state, so recompute it before accepting the write.
	computedChecksum, err := snapshot.ComputeChecksum()
	if err != nil {
		return fmt.Errorf("compute snapshot checksum: %w", err)
	}
	if !strings.EqualFold(computedChecksum, record.Checksum) {
		return &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "snapshot checksum does not match its content"}
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current uint64
	var currentChecksum string
	err = tx.QueryRowContext(ctx, `SELECT generation,checksum FROM desired_snapshots WHERE node_id=?`, record.NodeID).Scan(&current, &currentChecksum)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id=?`, record.NodeID).Scan(&exists); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if record.Generation < current {
			expected := current
			if current < math.MaxInt64 {
				expected++
			}
			return &RevisionConflictError{Resource: "desired_snapshot", Expected: uint64ToRevision(expected), Actual: uint64ToRevision(record.Generation)}
		}
		if record.Generation == current {
			if strings.EqualFold(record.Checksum, currentChecksum) {
				return tx.Commit()
			}
			expected := current
			if current < math.MaxInt64 {
				expected++
			}
			return &RevisionConflictError{Resource: "desired_snapshot", Expected: uint64ToRevision(expected), Actual: uint64ToRevision(record.Generation)}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO desired_snapshots(node_id,generation,checksum,document_json,created_at) VALUES(?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET generation=excluded.generation,checksum=excluded.checksum,document_json=excluded.document_json,created_at=excluded.created_at WHERE excluded.generation > desired_snapshots.generation`, record.NodeID, record.Generation, record.Checksum, record.Document, record.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return s.commitAndNotify(tx, record.NodeID)
}

func (s *Store) LoadSnapshot(ctx context.Context, nodeID string) (SnapshotRecord, error) {
	var record SnapshotRecord
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT node_id,generation,checksum,document_json,created_at FROM desired_snapshots WHERE node_id=?`, nodeID).Scan(&record.NodeID, &record.Generation, &record.Checksum, &record.Document, &created)
	if err != nil {
		return SnapshotRecord{}, err
	}
	record.CreatedAt, err = parseStoredTime("snapshot.created_at", created)
	if err != nil {
		return SnapshotRecord{}, err
	}
	if err := validateSnapshotRecord(record); err != nil {
		return SnapshotRecord{}, err
	}
	return record, nil
}

// validateSnapshotRecord treats the persisted document and its indexed
// metadata as one integrity boundary. Keeping this check on the low-level
// loader means scheduling and control-stream code cannot accidentally consume
// a hand-edited or partially restored row merely because its generation field
// looks plausible.
func validateSnapshotRecord(record SnapshotRecord) error {
	if record.NodeID == "" || record.Generation == 0 || record.Checksum == "" || len(record.Document) == 0 {
		return errors.New("stored snapshot metadata is incomplete")
	}
	var snapshot domain.DesiredSnapshot
	if err := json.Unmarshal(record.Document, &snapshot); err != nil {
		return fmt.Errorf("stored snapshot document is invalid: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if snapshot.NodeID != record.NodeID || snapshot.Generation != record.Generation || !strings.EqualFold(snapshot.Checksum, record.Checksum) {
		return &domain.ApplyError{Code: "snapshot_metadata_mismatch", Message: "stored snapshot metadata does not match its document"}
	}
	computed, err := snapshot.ComputeChecksum()
	if err != nil {
		return fmt.Errorf("compute stored snapshot checksum: %w", err)
	}
	if !strings.EqualFold(computed, record.Checksum) {
		return &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "stored snapshot checksum does not match its content"}
	}
	return nil
}

func (s *Store) GetSnapshot(ctx context.Context, nodeID string) (domain.DesiredSnapshot, error) {
	record, err := s.LoadSnapshot(ctx, nodeID)
	if err != nil {
		return domain.DesiredSnapshot{}, err
	}
	var snapshot domain.DesiredSnapshot
	if err := json.Unmarshal(record.Document, &snapshot); err != nil {
		return domain.DesiredSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return domain.DesiredSnapshot{}, err
	}
	if snapshot.NodeID != record.NodeID || snapshot.Generation != record.Generation || !strings.EqualFold(snapshot.Checksum, record.Checksum) {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "snapshot_metadata_mismatch", Message: "stored snapshot metadata does not match its document"}
	}
	computedChecksum, err := snapshot.ComputeChecksum()
	if err != nil {
		return domain.DesiredSnapshot{}, fmt.Errorf("compute snapshot checksum: %w", err)
	}
	if !strings.EqualFold(computedChecksum, record.Checksum) {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "stored snapshot checksum does not match its content"}
	}
	return snapshot, nil
}

func (s *Store) SaveObserved(ctx context.Context, record ObservedRecord) error {
	if record.NodeID == "" || len(record.Document) == 0 {
		return errors.New("observed state node and document are required")
	}
	if len(record.Document) > 16<<20 {
		return errors.New("observed state document is too large")
	}
	if record.Generation > math.MaxInt64 {
		return &domain.ApplyError{Code: "invalid_generation", Path: "applied_generation", Message: "observed generation exceeds repository limit"}
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	if record.UpdatedAt.After(time.Now().UTC().Add(5 * time.Minute)) {
		return &domain.ApplyError{Code: "invalid_observed_state", Path: "updated_at", Message: "observed timestamp is too far in the future"}
	}
	var observed domain.ObservedState
	if err := json.Unmarshal(record.Document, &observed); err != nil {
		return fmt.Errorf("observed document is invalid: %w", err)
	}
	if err := observed.Validate(); err != nil {
		return err
	}
	if !observed.ObservedAt.IsZero() && observed.ObservedAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return &domain.ApplyError{Code: "invalid_observed_state", Path: "observed_at", Message: "observed timestamp is too far in the future"}
	}
	if observed.NodeID != record.NodeID || observed.AppliedGeneration != record.Generation {
		return errors.New("observed metadata does not match document")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id=?`, record.NodeID).Scan(&exists); err != nil {
		return err
	}
	var desiredGeneration uint64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM desired_snapshots WHERE node_id=?`, record.NodeID).Scan(&desiredGeneration); err == nil {
		if record.Generation > desiredGeneration {
			return &RevisionConflictError{Resource: "observed_state", Expected: uint64ToRevision(desiredGeneration), Actual: uint64ToRevision(record.Generation)}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	} else if record.Generation != 0 {
		return &RevisionConflictError{Resource: "observed_state", Expected: 0, Actual: uint64ToRevision(record.Generation)}
	}
	var current uint64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM observed_states WHERE node_id=?`, record.NodeID).Scan(&current); err == nil {
		if record.Generation < current {
			return &RevisionConflictError{Resource: "observed_state", Expected: uint64ToRevision(current), Actual: uint64ToRevision(record.Generation)}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO observed_states(node_id,generation,document_json,updated_at) VALUES(?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET generation=excluded.generation,document_json=excluded.document_json,updated_at=excluded.updated_at`, record.NodeID, record.Generation, record.Document, record.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return s.commitAndNotifyResourceOnly(tx, record.NodeID)
}

func (s *Store) LoadObserved(ctx context.Context, nodeID string) (ObservedRecord, error) {
	var record ObservedRecord
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT node_id,generation,document_json,updated_at FROM observed_states WHERE node_id=?`, nodeID).Scan(&record.NodeID, &record.Generation, &record.Document, &updated)
	if err != nil {
		return ObservedRecord{}, err
	}
	record.UpdatedAt, err = parseStoredTime("observed.updated_at", updated)
	if err != nil {
		return ObservedRecord{}, err
	}
	var observed domain.ObservedState
	if err := json.Unmarshal(record.Document, &observed); err != nil {
		return ObservedRecord{}, fmt.Errorf("stored observed document is invalid: %w", err)
	}
	if err := observed.Validate(); err != nil {
		return ObservedRecord{}, err
	}
	if !observed.ObservedAt.IsZero() && observed.ObservedAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return ObservedRecord{}, &domain.ApplyError{Code: "invalid_observed_state", Path: "observed_at", Message: "observed timestamp is too far in the future"}
	}
	if observed.NodeID != record.NodeID || observed.AppliedGeneration != record.Generation {
		return ObservedRecord{}, errors.New("stored observed metadata does not match its document")
	}
	return record, nil
}

func (s *Store) GetObserved(ctx context.Context, nodeID string) (domain.ObservedState, error) {
	record, err := s.LoadObserved(ctx, nodeID)
	if err != nil {
		return domain.ObservedState{}, err
	}
	var observed domain.ObservedState
	if err := json.Unmarshal(record.Document, &observed); err != nil {
		return domain.ObservedState{}, err
	}
	if err := observed.Validate(); err != nil {
		return domain.ObservedState{}, err
	}
	return observed, nil
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,actor,action,resource,resource_id,revision,attributes_json,created_at FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AuditRecord{}
	for rows.Next() {
		var record AuditRecord
		var attributes []byte
		var created string
		if err := rows.Scan(&record.ID, &record.Actor, &record.Action, &record.Resource, &record.ResourceID, &record.Revision, &attributes, &created); err != nil {
			return nil, err
		}
		if len(attributes) > 0 {
			if err := json.Unmarshal(attributes, &record.Attributes); err != nil {
				return nil, fmt.Errorf("%w: invalid audit attributes: %w", ErrStorageFailure, err)
			}
		}
		record.CreatedAt, err = parseStoredTime("audit.created_at", created)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *Store) putDocument(ctx context.Context, table, nodeID string, value any, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	requestValue := value
	switch typed := value.(type) {
	case domain.GatewaySpec:
		typed.Revision = 0
		typed.Obfuscation = obfuscationRequestPolicy(typed.Obfuscation)
		requestValue = typed
	case domain.AgentSpec:
		typed.Revision = 0
		requestValue = typed
	}
	idempotentRequest := struct {
		Value   any   `json:"value"`
		IfMatch int64 `json:"if_match"`
	}{Value: requestValue, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, idempotentRequest)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	affectedNodes := []string{nodeID}
	if table == "gateway_specs" {
		participants, err := assignmentParticipantIDsTx(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		affectedNodes = append(affectedNodes, participants...)
	}
	if table == "gateway_specs" {
		gateway, ok := value.(domain.GatewaySpec)
		if !ok {
			return errors.New("gateway spec document has an invalid type")
		}
		if err := validateGatewaySpecTx(ctx, tx, gateway); err != nil {
			return err
		}
	} else if table == "agent_specs" {
		agent, ok := value.(domain.AgentSpec)
		if !ok {
			return errors.New("agent spec document has an invalid type")
		}
		if err := validateAgentSpecTx(ctx, tx, agent); err != nil {
			return err
		}
	}
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM `+table+` WHERE node_id=?`, nodeID).Scan(&revision)
	isInsert := errors.Is(err, sql.ErrNoRows)
	if isInsert {
		revision = 1
		if options.IfMatch > 0 {
			return &RevisionConflictError{Resource: table, Expected: options.IfMatch, Actual: 0}
		}
	} else if err == nil {
		if options.IfMatch <= 0 || options.IfMatch != revision {
			return &RevisionConflictError{Resource: table, Expected: options.IfMatch, Actual: revision}
		}
		revision++
	} else {
		return err
	}
	// Revisions are repository-owned metadata. Keep the canonical value in
	// the JSON document as well as in the indexed column.
	switch typed := value.(type) {
	case domain.GatewaySpec:
		typed.Revision = revision
		value = typed
	case domain.AgentSpec:
		typed.Revision = revision
		value = typed
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if isInsert {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+table+`(node_id,document_json,revision,updated_at) VALUES(?,?,?,?)`, nodeID, b, revision, now)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE `+table+` SET document_json=?,revision=?,updated_at=? WHERE node_id=? AND revision=?`, b, revision, now, nodeID, revision-1)
	}
	if err != nil {
		return err
	}
	// A Gateway endpoint is part of the assignment's dial target. Keep every
	// assignment on this Gateway aligned with the newly committed endpoint in
	// the same transaction, and advance its shared generation so the Agent
	// tears down the old QUIC session and reconnects to the new address.
	if table == "gateway_specs" {
		gateway, ok := value.(domain.GatewaySpec)
		if !ok {
			return errors.New("gateway spec document has an invalid type")
		}
		if err := updateAssignmentEndpointsTx(ctx, tx, gateway); err != nil {
			return err
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "upsert", strings.TrimSuffix(table, "_specs"), nodeID, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"node_id": nodeID, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

// updateAssignmentEndpointsTx keeps the derived assignment dial target
// consistent with a GatewaySpec edit. It intentionally runs after the spec
// row is written but before the transaction commits, so an endpoint change
// can never be observed without its corresponding assignment generation.
func updateAssignmentEndpointsTx(ctx context.Context, tx *sql.Tx, spec domain.GatewaySpec) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,agent_id,document_json,revision,generation FROM assignments WHERE gateway_id=? ORDER BY id`, spec.NodeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	endpointSet := make(map[string]struct{}, len(spec.PublicEndpoints))
	for _, endpoint := range spec.PublicEndpoints {
		endpointSet[endpoint] = struct{}{}
	}
	for rows.Next() {
		var assignment domain.Assignment
		var document []byte
		var revision int64
		var indexedGeneration uint64
		if err := rows.Scan(&assignment.ID, &assignment.AgentID, &document, &revision, &indexedGeneration); err != nil {
			return err
		}
		if err := json.Unmarshal(document, &assignment); err != nil {
			return fmt.Errorf("decode assignment %q: %w", assignment.ID, err)
		}
		if assignment.Generation != indexedGeneration {
			return fmt.Errorf("assignment %q generation index is inconsistent", assignment.ID)
		}
		endpoint := assignment.PublicEndpoint
		if _, exists := endpointSet[endpoint]; !exists {
			endpoint = spec.PublicEndpoints[0]
		}
		if endpoint == assignment.PublicEndpoint {
			continue
		}
		if assignment.Generation == math.MaxUint64 {
			return errors.New("assignment generation is exhausted")
		}
		assignment.PublicEndpoint = endpoint
		assignment.Generation++
		// Endpoint edits invalidate acknowledgements, but they must not
		// resurrect a placement that was already fail-closed because one of its
		// identities was disabled/revoked or its Gateway was offline. Such a
		// placement stays degraded until the scheduler creates a fresh, healthy
		// generation.
		if assignment.State != domain.AssignmentDraining && assignment.State != domain.AssignmentDegraded {
			assignment.State = domain.AssignmentPending
		}
		assignment.Revision = revision + 1
		assignment.UpdatedAt = time.Now().UTC()
		updated, marshalErr := json.Marshal(assignment)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,generation=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Generation, assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano), assignment.ID, revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, assignment.ID); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, "system", "derived_endpoint", "assignment", assignment.ID, assignment.Revision, map[string]string{"gateway_id": spec.NodeID, "generation": fmt.Sprint(assignment.Generation)}); err != nil {
			return err
		}
	}
	return rows.Err()
}

// validateGatewaySpecTx checks constraints that involve the existing
// assignment index. Listener bindings and service bindings share one public
// namespace, so a spec edit must not be able to introduce a collision after
// the document itself has passed structural validation. The check runs in
// the same transaction that writes the spec; the database UNIQUE constraint
// remains the final race-safe guard for concurrent assignment writers.
func validateGatewaySpecTx(ctx context.Context, tx *sql.Tx, spec domain.GatewaySpec) error {
	rows, err := tx.QueryContext(ctx, `SELECT protocol,bind,port FROM service_bindings WHERE gateway_id=?`, spec.NodeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var protocol, bind string
		var port uint16
		if err := rows.Scan(&protocol, &bind, &port); err != nil {
			return err
		}
		if !portInPool(spec.PortPool, protocol, port) {
			return &domain.ApplyError{Code: "port_outside_pool", Path: "port_pool", Message: fmt.Sprintf("existing binding %s:%d is outside the new gateway %s port pool", bind, port, protocol)}
		}
		for _, listener := range spec.Listeners {
			if bindingKey(listener.Protocol, listener.Bind, listener.Port) == bindingKey(protocol, bind, port) {
				return &PortConflictError{GatewayID: spec.NodeID, Protocol: protocol, Bind: bind, Port: port}
			}
		}
	}
	return rows.Err()
}

func validateAgentSpecTx(ctx context.Context, tx *sql.Tx, spec domain.AgentSpec) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,gateway_id FROM assignments WHERE agent_id=?`, spec.NodeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var assignmentID, gatewayID string
		if err := rows.Scan(&assignmentID, &gatewayID); err != nil {
			return err
		}
		var labelsJSON []byte
		if err := tx.QueryRowContext(ctx, `SELECT labels_json FROM nodes WHERE id=?`, gatewayID).Scan(&labelsJSON); err != nil {
			return err
		}
		var labels map[string]string
		if len(labelsJSON) > 0 {
			if err := json.Unmarshal(labelsJSON, &labels); err != nil {
				return err
			}
		}
		if !spec.GatewaySelector.Matches(labels) {
			return &domain.ApplyError{Code: "selector_mismatch", Path: "gateway_selector", Message: fmt.Sprintf("agent spec selector no longer matches gateway %q for assignment %q", gatewayID, assignmentID)}
		}
	}
	return rows.Err()
}

func (s *Store) deleteDocument(ctx context.Context, table, nodeID string, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := struct {
		NodeID  string `json:"node_id"`
		IfMatch int64  `json:"if_match"`
	}{NodeID: nodeID, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM `+table+` WHERE node_id=?`, nodeID).Scan(&revision); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: table, Expected: options.IfMatch, Actual: revision}
	}
	if table == "gateway_specs" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignments WHERE gateway_id=?`, nodeID).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return &domain.ApplyError{Code: "resource_conflict", Path: "gateway_spec", Message: "gateway spec has active assignments"}
		}
	} else if table == "agent_specs" {
		var serviceCount, assignmentCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM services WHERE agent_id=?`, nodeID).Scan(&serviceCount); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignments WHERE agent_id=?`, nodeID).Scan(&assignmentCount); err != nil {
			return err
		}
		if serviceCount > 0 || assignmentCount > 0 {
			return &domain.ApplyError{Code: "resource_conflict", Path: "agent_spec", Message: "agent spec has services or assignments"}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE node_id=? AND revision=?`, nodeID, revision); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", strings.TrimSuffix(table, "_specs"), nodeID, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"node_id": nodeID, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, nodeID)
}

func (s *Store) putServiceDocument(ctx context.Context, service domain.Service, options WriteOptions) error {
	requestService := service
	requestService.Revision = 0
	requestService.UpdatedAt = time.Time{}
	idempotentRequest := struct {
		Service domain.Service `json:"service"`
		IfMatch int64          `json:"if_match"`
	}{Service: requestService, IfMatch: options.IfMatch}
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, idempotentRequest)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	affectedNodes := []string{service.AgentID}
	participants, err := assignmentParticipantIDsForServiceTx(ctx, tx, service.ID)
	if err != nil {
		return err
	}
	affectedNodes = append(affectedNodes, participants...)
	var revision int64
	var previousDocument []byte
	var previous domain.Service
	hadPrevious := false
	err = tx.QueryRowContext(ctx, `SELECT revision,document_json FROM services WHERE id=?`, service.ID).Scan(&revision, &previousDocument)
	if errors.Is(err, sql.ErrNoRows) {
		revision = 1
		if options.IfMatch > 0 {
			return &RevisionConflictError{Resource: "service", Expected: options.IfMatch, Actual: 0}
		}
		service.Revision = revision
		service.UpdatedAt = nowTime
		b, marshalErr := json.Marshal(service)
		if marshalErr != nil {
			return marshalErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO services(id,agent_id,document_json,revision,updated_at) VALUES(?,?,?,?,?)`, service.ID, service.AgentID, b, revision, now)
	} else if err == nil {
		hadPrevious = true
		if options.IfMatch <= 0 || options.IfMatch != revision {
			return &RevisionConflictError{Resource: "service", Expected: options.IfMatch, Actual: revision}
		}
		if decodeErr := json.Unmarshal(previousDocument, &previous); decodeErr != nil {
			return fmt.Errorf("decode existing service: %w", decodeErr)
		}
		// An unassigned service may move between Agents. The old Agent's
		// node-scoped snapshot must be invalidated too, otherwise targeted
		// notifications leave the old node serving a stale service document.
		affectedNodes = append(affectedNodes, previous.AgentID)
		if previous.AgentID != service.AgentID || previous.Protocol != service.Protocol || previous.PublicBind != service.PublicBind || previous.PublicPort != service.PublicPort {
			assigned, assignmentErr := serviceHasAssignment(ctx, tx, service.ID)
			if assignmentErr != nil {
				return assignmentErr
			}
			if assigned {
				return &domain.ApplyError{Code: "resource_conflict", Path: "service", Message: "assigned service cannot change agent, protocol, bind, or port"}
			}
		}
		if !selectorsEqual(previous.GatewaySelector, service.GatewaySelector) {
			assignment, assigned, lookupErr := assignmentForService(ctx, tx, service.ID)
			if lookupErr != nil {
				return lookupErr
			}
			if assigned {
				var labelsJSON []byte
				if err := tx.QueryRowContext(ctx, `SELECT labels_json FROM nodes WHERE id=?`, assignment.GatewayID).Scan(&labelsJSON); err != nil {
					return err
				}
				var labels map[string]string
				if len(labelsJSON) > 0 {
					if err := json.Unmarshal(labelsJSON, &labels); err != nil {
						return err
					}
				}
				if !service.GatewaySelector.Matches(labels) {
					return &domain.ApplyError{Code: "selector_mismatch", Path: "service.gateway_selector", Message: "assigned service selector does not match its gateway"}
				}
			}
		}
		revision++
		service.Revision = revision
		service.UpdatedAt = nowTime
		b, marshalErr := json.Marshal(service)
		if marshalErr != nil {
			return marshalErr
		}
		_, err = tx.ExecContext(ctx, `UPDATE services SET agent_id=?,document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, service.AgentID, b, revision, now, service.ID, revision-1)
	}
	if err != nil {
		return err
	}
	if hadPrevious && !sameServiceContent(previous, service) {
		// A service document is consumed by both ends of its assignment. Mark
		// the placement pending and advance the shared assignment generation in
		// the same transaction as the service write; otherwise one node could
		// start using a new local target while its peer still authorizes the old
		// target under an applied assignment.
		if err := bumpAssignmentsForServiceTx(ctx, tx, service.ID); err != nil {
			return err
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "upsert", "service", service.ID, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"id": service.ID, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

func assignmentParticipantIDsTx(ctx context.Context, tx *sql.Tx, nodeID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT gateway_id,agent_id FROM assignments WHERE gateway_id=? OR agent_id=?`, nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var gatewayID, agentID string
		if err := rows.Scan(&gatewayID, &agentID); err != nil {
			return nil, err
		}
		result = append(result, gatewayID, agentID)
	}
	return result, rows.Err()
}

func assignmentParticipantIDsForServiceTx(ctx context.Context, tx *sql.Tx, serviceID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT assignments.gateway_id,assignments.agent_id FROM assignments JOIN assignment_services ON assignment_services.assignment_id=assignments.id WHERE assignment_services.service_id=?`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var gatewayID, agentID string
		if err := rows.Scan(&gatewayID, &agentID); err != nil {
			return nil, err
		}
		result = append(result, gatewayID, agentID)
	}
	return result, rows.Err()
}

// bumpAssignmentsForServiceTx invalidates the shared placement generation for
// every assignment that consumes a changed Service. The resource and
// assignment updates are deliberately part of one SQLite transaction so a
// snapshot builder can never observe the new target with an old applied
// assignment. Degraded/draining assignments remain fail-closed; a later
// scheduler pass can replace them with a fresh placement.
func bumpAssignmentsForServiceTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT assignments.id,assignments.document_json,assignments.revision,assignments.generation FROM assignments JOIN assignment_services ON assignment_services.assignment_id=assignments.id WHERE assignment_services.service_id=? ORDER BY assignments.id`, serviceID)
	if err != nil {
		return err
	}
	type assignmentRow struct {
		id         string
		document   []byte
		revision   int64
		generation uint64
	}
	assigned := make([]assignmentRow, 0)
	for rows.Next() {
		var row assignmentRow
		if err := rows.Scan(&row.id, &row.document, &row.revision, &row.generation); err != nil {
			_ = rows.Close()
			return err
		}
		assigned = append(assigned, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range assigned {
		id, document, revision, indexedGeneration := row.id, row.document, row.revision, row.generation
		var assignment domain.Assignment
		if err := json.Unmarshal(document, &assignment); err != nil {
			return fmt.Errorf("decode assignment %q: %w", id, err)
		}
		contains := false
		for _, candidate := range assignment.ServiceIDs {
			if candidate == serviceID {
				contains = true
				break
			}
		}
		if !contains {
			continue
		}
		if assignment.ID != id || assignment.Generation != indexedGeneration {
			return &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment", Message: "stored assignment metadata does not match its row"}
		}
		if assignment.Generation == math.MaxUint64 {
			return &domain.ApplyError{Code: "invalid_generation", Path: "assignment.generation", Message: "assignment generation is exhausted"}
		}
		if revision == math.MaxInt64 {
			return &domain.ApplyError{Code: "invalid_revision", Path: "assignment.revision", Message: "assignment revision is exhausted"}
		}
		if assignment.State == "" {
			assignment.State = domain.AssignmentPending
		}
		assignment.Generation++
		if assignment.State != domain.AssignmentDegraded && assignment.State != domain.AssignmentDraining {
			assignment.State = domain.AssignmentPending
		}
		assignment.Revision = revision + 1
		assignment.UpdatedAt = time.Now().UTC()
		updated, err := json.Marshal(assignment)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,generation=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Generation, assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano), id, revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, id); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, "system", "derived_service", "assignment", id, assignment.Revision, map[string]string{"service_id": serviceID, "generation": fmt.Sprint(assignment.Generation)}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func serviceHasAssignment(ctx context.Context, tx *sql.Tx, serviceID string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignment_services WHERE service_id=?`, serviceID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func assignmentForService(ctx context.Context, tx *sql.Tx, serviceID string) (domain.Assignment, bool, error) {
	var document []byte
	err := tx.QueryRowContext(ctx, `SELECT assignments.document_json FROM assignments JOIN assignment_services ON assignment_services.assignment_id=assignments.id WHERE assignment_services.service_id=?`, serviceID).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Assignment{}, false, nil
	}
	if err != nil {
		return domain.Assignment{}, false, err
	}
	var assignment domain.Assignment
	if err := json.Unmarshal(document, &assignment); err != nil {
		return domain.Assignment{}, false, err
	}
	return assignment, true, nil
}

func selectorsEqual(left, right domain.Selector) bool {
	if len(left.MatchLabels) != len(right.MatchLabels) {
		return false
	}
	for key, value := range left.MatchLabels {
		if right.MatchLabels[key] != value {
			return false
		}
	}
	return true
}

func insertAudit(ctx context.Context, tx *sql.Tx, actor, action, resource, resourceID string, revision int64, attributes map[string]string) error {
	if strings.TrimSpace(actor) == "" {
		actor = "system"
	}
	b, err := json.Marshal(attributes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(actor,action,resource,resource_id,revision,attributes_json,created_at) VALUES(?,?,?,?,?,?,?)`, actor, action, resource, resourceID, revision, b, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanNode(row scanner) (domain.Node, error) {
	var node domain.Node
	var labels []byte
	var enabled int
	var created, updated string
	if err := row.Scan(&node.ID, &node.Role, &node.Name, &labels, &enabled, &node.CertificateState, &node.CertificateSerial, &node.Revision, &created, &updated); err != nil {
		return domain.Node{}, err
	}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &node.Labels); err != nil {
			return domain.Node{}, err
		}
	}
	node.Enabled = enabled != 0
	parsed, err := parseStoredTime("node.created_at", created)
	if err != nil {
		return domain.Node{}, err
	}
	node.CreatedAt = parsed
	node.UpdatedAt, err = parseStoredTime("node.updated_at", updated)
	if err != nil {
		return domain.Node{}, err
	}
	if err := node.Validate(); err != nil {
		return domain.Node{}, fmt.Errorf("stored node is invalid: %w", err)
	}
	return node, nil
}

func validateNode(node domain.Node) error {
	return node.Validate()
}

func normalizeBind(value string) string {
	value = strings.TrimSpace(value)
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Unmap().String()
	}
	return value
}

func defaultCertificateState(state string) string {
	if state == "" {
		return domain.CertificatePending
	}
	return state
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func uint64ToRevision(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func idempotencyHit(ctx context.Context, tx *sql.Tx, key string, request any) (bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return false, nil
	}
	if len(key) > 128 || strings.ContainsAny(key, "\x00\r\n") {
		return false, errors.New("idempotency key is invalid")
	}
	hash, err := requestHash(request)
	if err != nil {
		return false, err
	}
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT request_hash FROM idempotency_keys WHERE key=?`, key).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stored != hash {
		return false, errors.New("idempotency key was used for a different request")
	}
	return true, nil
}

func recordIdempotency(ctx context.Context, tx *sql.Tx, key string, request, response any) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	if len(key) > 128 || strings.ContainsAny(key, "\x00\r\n") {
		return errors.New("idempotency key is invalid")
	}
	hash, err := requestHash(request)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO idempotency_keys(key,request_hash,response_json,created_at) VALUES(?,?,?,?)`, key, hash, encoded, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func requestHash(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) protectObfuscationPolicy(policy *domain.ObfuscationPolicy) error {
	if policy == nil {
		return nil
	}
	if policy.Mode == "" || policy.Mode == "standard" {
		policy.Key = nil
		policy.PreviousKey = nil
		policy.KeyCiphertext = nil
		policy.PreviousKeyCiphertext = nil
		policy.KeyID = ""
		policy.PreviousKeyID = ""
		return nil
	}
	if policy.Mode != "camouflage" {
		return &domain.ApplyError{Code: "invalid_obfuscation", Path: "obfuscation.mode", Message: "obfuscation mode must be standard or camouflage"}
	}
	if len(policy.Key) > 0 {
		if len(policy.Key) != 32 {
			return errors.New("data-plane obfuscation key must contain exactly 32 bytes")
		}
		ciphertext, err := EncryptSecret(s.masterKey[:], policy.Key)
		if err != nil {
			return fmt.Errorf("encrypt obfuscation key: %w", err)
		}
		policy.KeyCiphertext = ciphertext
		policy.KeyID = obfuscationKeyID(policy.Key)
		policy.Key = nil
	}
	if len(policy.KeyCiphertext) == 0 {
		return errors.New("camouflage mode requires a protected current key")
	}
	current, err := DecryptSecret(s.masterKey[:], policy.KeyCiphertext)
	if err != nil || len(current) != 32 {
		return errors.New("camouflage current key is not a valid Controller-encrypted 32-byte key")
	}
	if policy.KeyID == "" {
		policy.KeyID = obfuscationKeyID(current)
	}
	if len(policy.PreviousKey) > 0 {
		if len(policy.PreviousKey) != 32 {
			return errors.New("previous data-plane obfuscation key must contain exactly 32 bytes")
		}
		ciphertext, err := EncryptSecret(s.masterKey[:], policy.PreviousKey)
		if err != nil {
			return fmt.Errorf("encrypt previous obfuscation key: %w", err)
		}
		policy.PreviousKeyCiphertext = ciphertext
		policy.PreviousKeyID = obfuscationKeyID(policy.PreviousKey)
		policy.PreviousKey = nil
	}
	if len(policy.PreviousKeyCiphertext) > 0 {
		previous, err := DecryptSecret(s.masterKey[:], policy.PreviousKeyCiphertext)
		if err != nil || len(previous) != 32 {
			return errors.New("previous obfuscation key is not a valid Controller-encrypted 32-byte key")
		}
		if policy.PreviousKeyID == "" {
			policy.PreviousKeyID = obfuscationKeyID(previous)
		}
	}
	policy.Key = nil
	policy.PreviousKey = nil
	return nil
}

func obfuscationKeyID(key []byte) string {
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:])
}

func obfuscationRequestPolicy(policy domain.ObfuscationPolicy) domain.ObfuscationPolicy {
	return domain.ObfuscationPolicy{Mode: policy.Mode, KeyID: policy.KeyID, PreviousKeyID: policy.PreviousKeyID, MaxPaddingBytes: policy.MaxPaddingBytes, HandshakeShaping: policy.HandshakeShaping}
}

func snapshotForIdempotency(snapshot domain.DesiredSnapshot) domain.DesiredSnapshot {
	request := snapshot.Clone()
	if request.Gateway != nil {
		request.Gateway.Obfuscation = obfuscationRequestPolicy(request.Gateway.Obfuscation)
	}
	for index := range request.Assignments {
		request.Assignments[index].Obfuscation = obfuscationRequestPolicy(request.Assignments[index].Obfuscation)
	}
	return request
}

// SnapshotDocumentForWire decrypts only the data-plane keys needed by an
// authenticated node. The persisted desired snapshot retains ciphertext and
// is never sent to a node as key material; the returned document is ephemeral
// and contains plaintext keys protected by the mTLS control stream.
func (s *Store) SnapshotDocumentForWire(document []byte) ([]byte, error) {
	var snapshot domain.DesiredSnapshot
	if err := json.Unmarshal(document, &snapshot); err != nil {
		return nil, fmt.Errorf("decode snapshot for wire: %w", err)
	}
	if snapshot.Gateway != nil {
		if err := s.decryptObfuscationPolicyForWire(&snapshot.Gateway.Obfuscation); err != nil {
			return nil, err
		}
	}
	for index := range snapshot.Assignments {
		if err := s.decryptObfuscationPolicyForWire(&snapshot.Assignments[index].Obfuscation); err != nil {
			return nil, fmt.Errorf("assignment %q obfuscation: %w", snapshot.Assignments[index].ID, err)
		}
	}
	if err := snapshot.Validate(); err != nil {
		return nil, fmt.Errorf("snapshot for wire is invalid: %w", err)
	}
	return json.Marshal(snapshot)
}

func (s *Store) decryptObfuscationPolicyForWire(policy *domain.ObfuscationPolicy) error {
	if policy == nil || policy.Mode == "" || policy.Mode == "standard" {
		if policy != nil {
			policy.Key = nil
			policy.PreviousKey = nil
			policy.KeyCiphertext = nil
			policy.PreviousKeyCiphertext = nil
		}
		return nil
	}
	if len(policy.Key) == 0 {
		key, err := DecryptSecret(s.masterKey[:], policy.KeyCiphertext)
		if err != nil || len(key) != 32 {
			return errors.New("current obfuscation key cannot be decrypted")
		}
		policy.Key = key
	}
	if len(policy.PreviousKeyCiphertext) > 0 && len(policy.PreviousKey) == 0 {
		key, err := DecryptSecret(s.masterKey[:], policy.PreviousKeyCiphertext)
		if err != nil || len(key) != 32 {
			return errors.New("previous obfuscation key cannot be decrypted")
		}
		policy.PreviousKey = key
	}
	if len(policy.Key) != 32 || (len(policy.PreviousKey) != 0 && len(policy.PreviousKey) != 32) {
		return errors.New("wire obfuscation key has an invalid length")
	}
	policy.KeyCiphertext = nil
	policy.PreviousKeyCiphertext = nil
	return nil
}

func sameServiceContent(left, right domain.Service) bool {
	left.Revision, right.Revision = 0, 0
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	leftHash, leftErr := requestHash(left)
	rightHash, rightErr := requestHash(right)
	return leftErr == nil && rightErr == nil && leftHash == rightHash
}
