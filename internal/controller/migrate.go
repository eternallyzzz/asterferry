package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"asterferry/internal/domain"
	_ "modernc.org/sqlite"
)

const (
	legacyDBSchema      = 3
	legacyDBFingerprint = "asterferry-controller-sqlite-v3"
)

// MigrationReport describes the v3 -> v4 database migration without exposing
// any resource document or secret material.
type MigrationReport struct {
	Path                  string
	FromVersion           int
	ToVersion             int
	Assignments           int
	AssignmentServices    int
	LegacyIdempotencyKeys int
	DryRun                bool
	AlreadyCurrent        bool
	BackupPath            string
}

// MigrateDatabase upgrades an on-disk v3 Controller database to v4. The
// operation is deliberately separate from OpenStore: a running Controller
// never performs an implicit schema rewrite. A consistent SQLite copy is
// upgraded first, then the original file is moved aside and the copy is
// published. Any publish failure restores the original file and sidecars.
func MigrateDatabase(ctx context.Context, path string, dryRun bool) (MigrationReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	absPath, err := migrationDatabasePath(path)
	if err != nil {
		return MigrationReport{}, err
	}
	report := MigrationReport{Path: absPath, ToVersion: currentDBSchema, DryRun: dryRun}

	db, err := sql.Open(driverName, sqliteDSN(absPath))
	if err != nil {
		return report, fmt.Errorf("open migration database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return report, fmt.Errorf("ping migration database: %w", err)
	}

	version, fingerprint, err := schemaIdentity(ctx, db)
	if err != nil {
		return report, err
	}
	report.FromVersion = version
	if version == currentDBSchema && fingerprint == dbSchemaFingerprint {
		if err := validateTables(ctx, db, currentSchemaTables()); err != nil {
			return report, fmt.Errorf("validate current database: %w", err)
		}
		report.AlreadyCurrent = true
		return report, nil
	}
	if version != legacyDBSchema || fingerprint != legacyDBFingerprint {
		return report, fmt.Errorf("%w: migration supports schema %d (%s), found schema %d (%s)", ErrIncompatibleDatabase, legacyDBSchema, legacyDBFingerprint, version, fingerprint)
	}
	if err := validateTables(ctx, db, legacySchemaTables()); err != nil {
		return report, fmt.Errorf("validate legacy database: %w", err)
	}
	relations, err := collectAssignmentServices(ctx, db)
	if err != nil {
		return report, err
	}
	report.Assignments = len(relations.assignments)
	report.AssignmentServices = len(relations.rows)
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM idempotency_keys`).Scan(&report.LegacyIdempotencyKeys); err != nil {
		return report, fmt.Errorf("count legacy idempotency keys: %w", err)
	}
	if dryRun {
		return report, nil
	}

	// Checkpoint before copying so a later file replacement cannot accidentally
	// reuse an old WAL sidecar under the new database filename.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return report, fmt.Errorf("checkpoint legacy database: %w", err)
	}
	tempPath, err := reserveMigrationPath(absPath, ".asterferry-migrate-*")
	if err != nil {
		return report, fmt.Errorf("reserve migration file: %w", err)
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tempPath); err != nil {
		return report, fmt.Errorf("copy legacy database: %w", err)
	}
	if err := db.Close(); err != nil {
		return report, fmt.Errorf("close legacy database: %w", err)
	}

	if err := upgradeMigrationCopy(ctx, tempPath, relations.rows); err != nil {
		return report, err
	}
	backupPath, err := publishMigrationCopy(absPath, tempPath)
	if err != nil {
		return report, err
	}
	removeTemp = false
	report.BackupPath = backupPath
	return report, nil
}

func currentSchemaTables() []string {
	return []string{"schema_meta", "users", "api_tokens", "nodes", "gateway_specs", "agent_specs", "services", "service_bindings", "assignments", "assignment_services", "assignment_acks", "desired_snapshots", "observed_states", "audit_events", "idempotency_keys", "enrollment_tokens"}
}

func legacySchemaTables() []string {
	return []string{"schema_meta", "users", "api_tokens", "nodes", "gateway_specs", "agent_specs", "services", "service_bindings", "assignments", "assignment_acks", "desired_snapshots", "observed_states", "audit_events", "idempotency_keys", "enrollment_tokens"}
}

type assignmentServiceMigration struct {
	assignmentID string
	serviceID    string
}

type migrationRelations struct {
	assignments map[string]struct{}
	rows        []assignmentServiceMigration
}

func schemaIdentity(ctx context.Context, db *sql.DB) (int, string, error) {
	var versionText, fingerprint string
	if err := db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&versionText); err != nil {
		if errors.Is(err, sql.ErrNoRows) || isSQLiteMissingSchemaObject(err) {
			return 0, "", fmt.Errorf("%w: schema marker is missing", ErrIncompatibleDatabase)
		}
		return 0, "", fmt.Errorf("read schema version: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='fingerprint'`).Scan(&fingerprint); err != nil {
		if errors.Is(err, sql.ErrNoRows) || isSQLiteMissingSchemaObject(err) {
			return 0, "", fmt.Errorf("%w: schema fingerprint is missing", ErrIncompatibleDatabase)
		}
		return 0, "", fmt.Errorf("read schema fingerprint: %w", err)
	}
	version, err := strconv.Atoi(versionText)
	if err != nil || version < 1 {
		return 0, fingerprint, fmt.Errorf("%w: schema version is invalid", ErrIncompatibleDatabase)
	}
	return version, fingerprint, nil
}

func isSQLiteMissingSchemaObject(err error) bool {
	var coded sqliteError
	return errors.As(err, &coded) && coded.Code() == 1 // SQLITE_ERROR
}

func validateTables(ctx context.Context, db *sql.DB, tables []string) error {
	for _, table := range tables {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("required table %q is missing", table)
		}
	}
	return nil
}

func collectAssignmentServices(ctx context.Context, db *sql.DB) (migrationRelations, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,document_json FROM assignments ORDER BY id`)
	if err != nil {
		return migrationRelations{}, fmt.Errorf("read legacy assignments: %w", err)
	}
	type legacyAssignment struct {
		id       string
		document []byte
	}
	legacyAssignments := make([]legacyAssignment, 0)
	for rows.Next() {
		var assignment legacyAssignment
		if err := rows.Scan(&assignment.id, &assignment.document); err != nil {
			_ = rows.Close()
			return migrationRelations{}, err
		}
		legacyAssignments = append(legacyAssignments, assignment)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return migrationRelations{}, err
	}
	if err := rows.Close(); err != nil {
		return migrationRelations{}, err
	}
	relations := migrationRelations{assignments: make(map[string]struct{})}
	seenServices := make(map[string]string)
	for _, legacy := range legacyAssignments {
		id, document := legacy.id, legacy.document
		var assignment domain.Assignment
		if err := json.Unmarshal(document, &assignment); err != nil {
			return migrationRelations{}, fmt.Errorf("decode legacy assignment %q: %w", id, err)
		}
		if assignment.ID != id || strings.TrimSpace(assignment.ID) == "" {
			return migrationRelations{}, fmt.Errorf("legacy assignment %q has inconsistent identity", id)
		}
		relations.assignments[id] = struct{}{}
		for _, serviceID := range assignment.ServiceIDs {
			if strings.TrimSpace(serviceID) == "" {
				return migrationRelations{}, fmt.Errorf("legacy assignment %q contains an empty service id", id)
			}
			if previous, exists := seenServices[serviceID]; exists {
				return migrationRelations{}, fmt.Errorf("service %q is assigned by both %q and %q", serviceID, previous, id)
			}
			var exists int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM services WHERE id=?`, serviceID).Scan(&exists); err != nil {
				return migrationRelations{}, err
			}
			if exists != 1 {
				return migrationRelations{}, fmt.Errorf("legacy assignment %q references missing service %q", id, serviceID)
			}
			seenServices[serviceID] = id
			relations.rows = append(relations.rows, assignmentServiceMigration{assignmentID: id, serviceID: serviceID})
		}
	}
	return relations, nil
}

func upgradeMigrationCopy(ctx context.Context, path string, relations []assignmentServiceMigration) error {
	db, err := sql.Open(driverName, sqliteDSN(path))
	if err != nil {
		return fmt.Errorf("open migration copy: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration copy: %w", err)
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE assignment_services (assignment_id TEXT NOT NULL REFERENCES assignments(id) ON DELETE CASCADE, service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE, PRIMARY KEY(assignment_id,service_id), UNIQUE(service_id))`,
		`CREATE INDEX idx_assignments_agent ON assignments(agent_id)`,
		`CREATE INDEX idx_assignment_services_service ON assignment_services(service_id)`,
		`DELETE FROM idempotency_keys`,
		`UPDATE schema_meta SET value=? WHERE key='schema_version'`,
		`UPDATE schema_meta SET value=? WHERE key='fingerprint'`,
	}
	if _, err := tx.ExecContext(ctx, statements[0]); err != nil {
		return fmt.Errorf("create assignment service relation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, statements[1]); err != nil {
		return fmt.Errorf("create assignment agent index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, statements[2]); err != nil {
		return fmt.Errorf("create assignment service index: %w", err)
	}
	for _, relation := range relations {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_services(assignment_id,service_id) VALUES(?,?)`, relation.assignmentID, relation.serviceID); err != nil {
			return fmt.Errorf("populate assignment service relation: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, statements[3]); err != nil {
		return fmt.Errorf("clear legacy idempotency keys: %w", err)
	}
	if _, err := tx.ExecContext(ctx, statements[4], strconv.Itoa(currentDBSchema)); err != nil {
		return fmt.Errorf("write migrated schema version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, statements[5], dbSchemaFingerprint); err != nil {
		return fmt.Errorf("write migrated schema fingerprint: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version=4`); err != nil {
		return fmt.Errorf("write sqlite user version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrated database: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close migration copy: %w", err)
	}
	return nil
}

func reserveMigrationPath(path, pattern string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return "", err
	}
	reserved := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(reserved)
		return "", err
	}
	if err := os.Remove(reserved); err != nil {
		return "", err
	}
	return reserved, nil
}

func migrationDatabasePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return "", errors.New("database migration requires a regular on-disk SQLite path")
	}
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	if info, err := os.Stat(absPath); err != nil {
		return "", fmt.Errorf("stat database: %w", err)
	} else if !info.Mode().IsRegular() {
		return "", errors.New("database migration target is not a regular file")
	}
	return absPath, nil
}

func publishMigrationCopy(originalPath, copyPath string) (string, error) {
	backupPath, err := reserveMigrationPath(originalPath, ".asterferry-pre-v4-*")
	if err != nil {
		return "", fmt.Errorf("reserve migration backup: %w", err)
	}
	suffixes := []string{"-wal", "-shm", "-journal"}
	movedSidecars := make([]string, 0, len(suffixes))
	restore := func() error {
		var restoreErrs []error
		if _, statErr := os.Stat(originalPath); errors.Is(statErr, os.ErrNotExist) {
			if err := os.Rename(backupPath, originalPath); err != nil {
				restoreErrs = append(restoreErrs, err)
			}
		}
		for _, suffix := range movedSidecars {
			if err := os.Rename(backupPath+suffix, originalPath+suffix); err != nil {
				restoreErrs = append(restoreErrs, err)
			}
		}
		return errors.Join(restoreErrs...)
	}
	if err := os.Rename(originalPath, backupPath); err != nil {
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("move legacy database to backup: %w", err)
	}
	for _, suffix := range suffixes {
		if _, statErr := os.Stat(originalPath + suffix); errors.Is(statErr, os.ErrNotExist) {
			continue
		} else if statErr != nil {
			restoreErr := restore()
			return "", errors.Join(fmt.Errorf("inspect legacy database sidecar: %w", statErr), restoreErr)
		}
		if err := os.Rename(originalPath+suffix, backupPath+suffix); err != nil {
			restoreErr := restore()
			return "", errors.Join(fmt.Errorf("move legacy database sidecar: %w", err), restoreErr)
		}
		movedSidecars = append(movedSidecars, suffix)
	}
	if err := os.Rename(copyPath, originalPath); err != nil {
		restoreErr := restore()
		return "", errors.Join(fmt.Errorf("publish migrated database: %w", err), restoreErr)
	}
	return backupPath, nil
}
