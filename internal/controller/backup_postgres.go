package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runPostgresDump(ctx context.Context, databaseURL, schema, destination string) error {
	if _, err := exec.LookPath("pg_dump"); err != nil {
		return errors.New("PostgreSQL backup requires pg_dump in PATH")
	}
	connection, environment := postgresUtilityConnection(databaseURL)
	command := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-owner", "--no-privileges", "--schema", schema, "--file", destination, "--dbname", connection)
	command.Env = appendDatabaseEnvironment(environment)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("pg_dump failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func postgresCurrentSchema(ctx context.Context, databaseURL string) (string, error) {
	config := Config{DatabaseDriver: DatabaseDriverPostgres, DatabaseURL: databaseURL}
	db, backend, err := openConfiguredDatabase(ctx, config)
	if err != nil {
		return "", fmt.Errorf("open PostgreSQL database for backup: %w", err)
	}
	defer db.Close()
	if backend != databaseBackendPostgres {
		return "", errors.New("backup database is not PostgreSQL")
	}
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return "", fmt.Errorf("read PostgreSQL schema: %w", err)
	}
	if strings.TrimSpace(schema) == "" || strings.ContainsAny(schema, "\x00\r\n") {
		return "", errors.New("PostgreSQL current schema is invalid")
	}
	return schema, nil
}

func runPostgresRestore(ctx context.Context, databaseURL, source string) error {
	if _, err := exec.LookPath("pg_restore"); err != nil {
		return errors.New("PostgreSQL restore requires pg_restore in PATH")
	}
	connection, environment := postgresUtilityConnection(databaseURL)
	// pg_dump includes the selected schema as a pre-data object. --clean with
	// --if-exists lets restore recreate that schema when the target connection
	// uses a dedicated search_path, while the preflight still refuses a target
	// containing application tables.
	command := exec.CommandContext(ctx, "pg_restore", "--exit-on-error", "--clean", "--if-exists", "--no-owner", "--no-privileges", "--dbname", connection, source)
	command.Env = appendDatabaseEnvironment(environment)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("pg_restore failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func postgresUtilityConnection(databaseURL string) (string, map[string]string) {
	value := strings.TrimSpace(databaseURL)
	environment := make(map[string]string)
	parsed, err := url.Parse(value)
	if err == nil && (strings.EqualFold(parsed.Scheme, "postgres") || strings.EqualFold(parsed.Scheme, "postgresql")) {
		if parsed.User != nil {
			if password, ok := parsed.User.Password(); ok {
				environment["PGPASSWORD"] = password
				parsed.User = url.User(parsed.User.Username())
			}
		}
		query := parsed.Query()
		if _, alreadySet := environment["PGPASSWORD"]; !alreadySet {
			if password := query.Get("password"); password != "" {
				environment["PGPASSWORD"] = password
			}
		}
		if query.Has("password") {
			query.Del("password")
			parsed.RawQuery = query.Encode()
		}
		value = parsed.String()
	}
	return value, environment
}

func appendDatabaseEnvironment(values map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replace := values[key]; replace {
				continue
			}
		}
		environment = append(environment, entry)
	}
	for key, value := range values {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func restorePostgresBackup(config Config, source, destination string, manifest backupManifest) error {
	if strings.TrimSpace(destination) == "" {
		return errors.New("destination Controller data directory is required for PostgreSQL restore")
	}
	destination = filepath.Clean(destination)
	if err := ensurePostgresTargetEmpty(context.Background(), config); err != nil {
		return err
	}
	targetSchema, err := postgresCurrentSchema(context.Background(), config.DatabaseURL)
	if err != nil {
		return err
	}
	if manifest.DatabaseSchema == "" || manifest.DatabaseSchema != targetSchema {
		return fmt.Errorf("backup PostgreSQL schema %q does not match target schema %q", manifest.DatabaseSchema, targetSchema)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	material := map[string]string{
		"master.key":     filepath.Join(destination, "master.key"),
		"ca.key":         filepath.Join(destination, "ca", "ca.key"),
		"ca.crt":         filepath.Join(destination, "ca", "ca.crt"),
		"controller.key": filepath.Join(destination, "tls", "controller.key"),
		"controller.crt": filepath.Join(destination, "tls", "controller.crt"),
	}
	for _, name := range []string{"master.key", "ca.key", "ca.crt", "controller.key", "controller.crt"} {
		if err := copyFile(filepath.Join(source, name), material[name], 0o600); err != nil {
			return err
		}
	}
	var restored Config
	data, err := os.ReadFile(filepath.Join(source, "controller.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &restored); err != nil {
		return fmt.Errorf("decode backup controller config: %w", err)
	}
	restored.DatabaseDriver = DatabaseDriverPostgres
	restored.DatabasePath = ""
	restored.DatabaseURL = config.DatabaseURL
	restored.DatabaseMaxOpenConns = config.DatabaseMaxOpenConns
	restored.MasterKeyPath = filepath.Join(destination, "master.key")
	restored.CAKeyPath = filepath.Join(destination, "ca", "ca.key")
	restored.CACertPath = filepath.Join(destination, "ca", "ca.crt")
	restored.TLSKeyPath = filepath.Join(destination, "tls", "controller.key")
	restored.TLSCertPath = filepath.Join(destination, "tls", "controller.crt")
	restored.SourcePath = filepath.Join(destination, "controller.json")
	if err := restored.Validate(); err != nil {
		return fmt.Errorf("validate restored controller config: %w", err)
	}
	if err := runPostgresRestore(context.Background(), config.DatabaseURL, filepath.Join(source, "controller.postgres.dump")); err != nil {
		return err
	}
	if err := resetRestoredControlState(context.Background(), restored); err != nil {
		return fmt.Errorf("reset restored Controller sessions and lease: %w", err)
	}
	key, err := LoadOrCreateMasterKey(restored.MasterKeyPath)
	if err != nil {
		return err
	}
	store, err := OpenControllerRepositoriesWithConfig(restored, key)
	if err != nil {
		return fmt.Errorf("validate restored PostgreSQL database: %w", err)
	}
	if err := store.Close(); err != nil {
		return err
	}
	if err := SaveConfig(restored.SourcePath, restored); err != nil {
		return fmt.Errorf("write restored controller config: %w", err)
	}
	return nil
}

func ensurePostgresTargetEmpty(ctx context.Context, config Config) error {
	db, backend, err := openConfiguredDatabase(ctx, config)
	if err != nil {
		return fmt.Errorf("open target PostgreSQL database: %w", err)
	}
	defer db.Close()
	if backend != databaseBackendPostgres {
		return errors.New("restore target is not PostgreSQL")
	}
	dialect := newDatabaseDialect(backend)
	compatible, empty, err := inspectDatabase(ctx, db, dialect)
	if err != nil {
		return fmt.Errorf("inspect target PostgreSQL database: %w", err)
	}
	if compatible || !empty {
		return errors.New("target PostgreSQL schema is not empty")
	}
	return nil
}
