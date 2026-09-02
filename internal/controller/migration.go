package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SQLiteToPostgresMigrationOptions describes the explicit, one-way migration
// from a stopped SQLite Controller to an empty PostgreSQL schema. Migration is
// intentionally a command rather than an automatic startup behavior: an
// operator must choose the target database and the generated configuration.
type SQLiteToPostgresMigrationOptions struct {
	SourceConfig     Config
	TargetURL        string
	OutputConfigPath string
	MaxOpenConns     int
	DryRun           bool
}

type SQLiteToPostgresMigrationReport struct {
	RowsByTable map[string]int64
	TotalRows   int64
}

var migrationTableOrder = []string{
	"users",
	"api_tokens",
	"nodes",
	"node_specs",
	"services",
	"service_bindings",
	"assignments",
	"assignment_services",
	"assignment_acks",
	"desired_snapshots",
	"observed_states",
	"audit_events",
	"idempotency_keys",
	"enrollment_tokens",
	"node_bootstraps",
	"runtime_connections",
	"runtime_events",
	"runtime_traffic_rollups",
	"runtime_settings",
}

// MigrateSQLiteToPostgres validates the source and target, then copies every
// application table in foreign-key order inside one PostgreSQL transaction.
// The target must be empty. A dry run opens both databases only to validate
// connectivity/schema and count source rows; it does not create or modify
// target tables and does not write a configuration file.
func MigrateSQLiteToPostgres(ctx context.Context, options SQLiteToPostgresMigrationOptions) (SQLiteToPostgresMigrationReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := options.SourceConfig.Validate(); err != nil {
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("validate source config: %w", err)
	}
	sourceBackend, err := validateDatabaseConfig(options.SourceConfig)
	if err != nil {
		return SQLiteToPostgresMigrationReport{}, err
	}
	if sourceBackend != databaseBackendSQLite {
		return SQLiteToPostgresMigrationReport{}, errors.New("source database must use sqlite")
	}
	targetURL := strings.TrimSpace(options.TargetURL)
	targetConfig := Config{
		DatabaseDriver:       DatabaseDriverPostgres,
		DatabaseURL:          targetURL,
		DatabaseMaxOpenConns: options.MaxOpenConns,
	}
	if _, err := validateDatabaseConfig(targetConfig); err != nil {
		return SQLiteToPostgresMigrationReport{}, err
	}
	if !options.DryRun && strings.TrimSpace(options.OutputConfigPath) == "" {
		return SQLiteToPostgresMigrationReport{}, errors.New("output config path is required unless --dry-run is used")
	}

	sourceDB, openedBackend, err := openConfiguredDatabase(ctx, options.SourceConfig)
	if err != nil {
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("open source sqlite database: %w", err)
	}
	defer sourceDB.Close()
	if openedBackend != databaseBackendSQLite {
		return SQLiteToPostgresMigrationReport{}, errors.New("source database backend changed while opening")
	}
	compatible, empty, err := inspectDatabase(ctx, sourceDB, databaseBackendSQLite)
	if err != nil {
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("inspect source database: %w", err)
	}
	if empty || !compatible {
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("source database is not a compatible Controller schema: %w", ErrIncompatibleDatabase)
	}
	if err := validateRequiredTables(ctx, sourceDB, databaseBackendSQLite); err != nil {
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("validate source database: %w", err)
	}

	targetDB, openedBackend, err := openConfiguredDatabase(ctx, targetConfig)
	if err != nil {
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("open target postgres database: %w", err)
	}
	compatible, empty, err = inspectDatabase(ctx, targetDB, databaseBackendPostgres)
	if err != nil {
		_ = targetDB.Close()
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("inspect target database: %w", err)
	}
	if compatible || !empty {
		_ = targetDB.Close()
		return SQLiteToPostgresMigrationReport{}, errors.New("target PostgreSQL schema is not empty; choose a new database or schema")
	}
	if openedBackend != databaseBackendPostgres {
		_ = targetDB.Close()
		return SQLiteToPostgresMigrationReport{}, errors.New("target database backend changed while opening")
	}

	sourceTx, err := sourceDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = targetDB.Close()
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("begin source snapshot: %w", err)
	}
	defer sourceTx.Rollback()
	report, err := countMigrationRows(ctx, sourceTx)
	if err != nil {
		_ = sourceTx.Rollback()
		_ = targetDB.Close()
		return SQLiteToPostgresMigrationReport{}, err
	}
	if options.DryRun {
		_ = sourceTx.Rollback()
		if err := targetDB.Close(); err != nil {
			return SQLiteToPostgresMigrationReport{}, err
		}
		return report, nil
	}
	if err := targetDB.Close(); err != nil {
		return SQLiteToPostgresMigrationReport{}, err
	}

	targetStore, err := OpenStoreWithConfig(targetConfig, make([]byte, masterKeyBytes))
	if err != nil {
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("initialize target postgres schema: %w", err)
	}
	defer targetStore.Close()

	targetTx, err := targetStore.db.BeginTx(ctx, nil)
	if err != nil {
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("begin target migration: %w", err)
	}
	for _, table := range migrationTableOrder {
		copied, copyErr := copyMigrationTable(ctx, sourceTx, targetTx, table)
		if copyErr != nil {
			_ = targetTx.Rollback()
			return SQLiteToPostgresMigrationReport{}, fmt.Errorf("copy %s: %w", table, copyErr)
		}
		if copied != report.RowsByTable[table] {
			_ = targetTx.Rollback()
			return SQLiteToPostgresMigrationReport{}, fmt.Errorf("copy %s: source row count changed during migration", table)
		}
	}
	if err := resetPostgresSequences(ctx, targetTx); err != nil {
		_ = targetTx.Rollback()
		return SQLiteToPostgresMigrationReport{}, err
	}
	if err := targetTx.Commit(); err != nil {
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("commit PostgreSQL migration: %w", err)
	}

	outputConfig := options.SourceConfig
	outputConfig.DatabaseDriver = DatabaseDriverPostgres
	outputConfig.DatabasePath = ""
	outputConfig.DatabaseURL = targetURL
	outputConfig.DatabaseMaxOpenConns = options.MaxOpenConns
	outputConfig.SourcePath = ""
	if err := SaveConfig(options.OutputConfigPath, outputConfig); err != nil {
		return SQLiteToPostgresMigrationReport{}, fmt.Errorf("write migrated controller config: %w", err)
	}
	return report, nil
}

type migrationQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func countMigrationRows(ctx context.Context, db migrationQueryer) (SQLiteToPostgresMigrationReport, error) {
	report := SQLiteToPostgresMigrationReport{RowsByTable: make(map[string]int64, len(migrationTableOrder))}
	for _, table := range migrationTableOrder {
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+quoteMigrationIdentifier(table)).Scan(&count); err != nil {
			return SQLiteToPostgresMigrationReport{}, fmt.Errorf("count %s: %w", table, err)
		}
		report.RowsByTable[table] = count
		report.TotalRows += count
	}
	return report, nil
}

func copyMigrationTable(ctx context.Context, source migrationQueryer, target *sql.Tx, table string) (int64, error) {
	rows, err := source.QueryContext(ctx, "SELECT * FROM "+quoteMigrationIdentifier(table))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	if len(columns) == 0 {
		return 0, errors.New("table has no columns")
	}
	placeholders := make([]string, len(columns))
	for index := range placeholders {
		placeholders[index] = "?"
	}
	quotedColumns := make([]string, len(columns))
	for index, column := range columns {
		quotedColumns[index] = quoteMigrationIdentifier(column)
	}
	statement := "INSERT INTO " + quoteMigrationIdentifier(table) + " (" + strings.Join(quotedColumns, ",") + ") VALUES (" + strings.Join(placeholders, ",") + ")"
	count := int64(0)
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(values))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return 0, err
		}
		if _, err := target.ExecContext(ctx, statement, values...); err != nil {
			return 0, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

func resetPostgresSequences(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{"audit_events", "runtime_events"} {
		query := fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s','id'), COALESCE(MAX(id), 1), MAX(id) IS NOT NULL) FROM %s", table, quoteMigrationIdentifier(table))
		var sequenceValue int64
		if err := tx.QueryRowContext(ctx, query).Scan(&sequenceValue); err != nil {
			return fmt.Errorf("reset PostgreSQL %s sequence: %w", table, err)
		}
	}
	return nil
}

func quoteMigrationIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
