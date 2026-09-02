package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"asterferry/internal/atomicfile"
	"asterferry/internal/jsonutil"
)

const backupManifestVersion = 1

type backupManifest struct {
	Version        int                  `json:"version"`
	CreatedAt      time.Time            `json:"created_at"`
	DatabaseSchema string               `json:"database_schema,omitempty"`
	Files          []backupManifestFile `json:"files"`
}

type backupManifestFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

var backupPayloadFiles = []string{
	"controller.db",
	"controller.json",
	"master.key",
	"ca.key",
	"ca.crt",
	"controller.key",
	"controller.crt",
}

var backupPayloadFilesPostgres = []string{
	"controller.postgres.dump",
	"controller.json",
	"master.key",
	"ca.key",
	"ca.crt",
	"controller.key",
	"controller.crt",
}

func backupPayloadFilesForBackend(backend databaseBackend) []string {
	if backend == databaseBackendPostgres {
		return backupPayloadFilesPostgres
	}
	return backupPayloadFiles
}

func Backup(ctx context.Context, config Config, destination string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := config.Validate(); err != nil {
		return "", err
	}
	backend, err := validateDatabaseConfig(config)
	if err != nil {
		return "", err
	}
	destination = filepath.Clean(strings.TrimSpace(destination))
	if destination == "." || destination == "" {
		return "", errors.New("backup destination is required")
	}
	if info, err := os.Stat(destination); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("backup destination %q is not a directory", destination)
		}
		destination = filepath.Join(destination, "asterferry-controller-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".backup-*")
	if err != nil {
		return "", fmt.Errorf("create backup staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	masterKey, err := LoadOrCreateMasterKey(config.MasterKeyPath)
	if err != nil {
		return "", err
	}
	databaseSchema := ""
	if backend == databaseBackendSQLite {
		store, err := OpenStoreWithConfig(config, masterKey)
		if err != nil {
			return "", err
		}
		defer store.Close()
		backupDB := filepath.Join(staging, "controller.db")
		// VACUUM INTO produces a consistent copy even while API writes continue.
		if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, backupDB); err != nil {
			return "", fmt.Errorf("backup sqlite database: %w", err)
		}
	} else {
		if err := validateConfiguredDatabase(ctx, config, backend); err != nil {
			return "", err
		}
		schema, err := postgresCurrentSchema(ctx, config.DatabaseURL)
		if err != nil {
			return "", err
		}
		databaseSchema = schema
		if err := runPostgresDump(ctx, config.DatabaseURL, schema, filepath.Join(staging, "controller.postgres.dump")); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := copyFile(configPathFor(config), filepath.Join(staging, "controller.json"), 0o600); err != nil {
		return "", err
	}
	if err := copyFile(config.MasterKeyPath, filepath.Join(staging, "master.key"), 0o600); err != nil {
		return "", err
	}
	// Keep the CA and Controller TLS identity with the database. Without these
	// files a restored database would reject every enrolled node until a new CA
	// was generated (which would invalidate all existing certificates).
	for _, item := range []struct {
		source string
		name   string
	}{
		{config.CAKeyPath, "ca.key"},
		{config.CACertPath, "ca.crt"},
		{config.TLSKeyPath, "controller.key"},
		{config.TLSCertPath, "controller.crt"},
	} {
		if err := copyFile(item.source, filepath.Join(staging, item.name), 0o600); err != nil {
			return "", err
		}
	}
	if err := writeBackupManifest(staging, time.Now().UTC(), databaseSchema, backupPayloadFilesForBackend(backend)); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(staging, destination); err != nil {
		return "", fmt.Errorf("publish backup: %w", err)
	}
	published = true
	return destination, nil
}

func Restore(config Config, source, destination string) error {
	if err := config.Validate(); err != nil {
		return err
	}
	backend, err := validateDatabaseConfig(config)
	if err != nil {
		return err
	}
	source = filepath.Clean(strings.TrimSpace(source))
	if source == "." || source == "" {
		return errors.New("backup source is required")
	}
	files := backupPayloadFilesForBackend(backend)
	manifest, err := verifyBackupManifest(source, files)
	if err != nil {
		return err
	}
	if backend == databaseBackendPostgres {
		return restorePostgresBackup(config, source, destination, manifest)
	}
	if destination == "" {
		destination = filepath.Dir(config.DatabasePath)
	}
	destination = filepath.Clean(destination)
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	dbPath := filepath.Join(source, "controller.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	if err := copyFile(dbPath, filepath.Join(destination, filepath.Base(config.DatabasePath)), 0o600); err != nil {
		return err
	}
	// Restore is deliberately self-contained: a destination directory is a
	// complete Controller installation.  Do not write key material back to the
	// paths from the source config (which may point at the old host), otherwise
	// an apparently successful restore could leave the new database without the
	// identity that issued its node certificates.
	material := map[string]string{
		"master.key":     filepath.Join(destination, "master.key"),
		"ca.key":         filepath.Join(destination, "ca", "ca.key"),
		"ca.crt":         filepath.Join(destination, "ca", "ca.crt"),
		"controller.key": filepath.Join(destination, "tls", "controller.key"),
		"controller.crt": filepath.Join(destination, "tls", "controller.crt"),
	}
	for _, item := range []struct {
		name string
	}{
		{"master.key"},
		{"ca.key"},
		{"ca.crt"},
		{"controller.key"},
		{"controller.crt"},
	} {
		if sourcePath := filepath.Join(source, item.name); fileExists(sourcePath) {
			if err := copyFile(sourcePath, material[item.name], 0o600); err != nil {
				return err
			}
		}
	}
	if backupConfig := filepath.Join(source, "controller.json"); fileExists(backupConfig) {
		// Rebase every filesystem path in the copied configuration.  Backups are
		// portable artifacts; retaining absolute paths from the source host would
		// silently make the restored Controller use the wrong database or keys.
		data, err := os.ReadFile(backupConfig)
		if err != nil {
			return err
		}
		var restored Config
		if err := json.Unmarshal(data, &restored); err != nil {
			return fmt.Errorf("decode backup controller config: %w", err)
		}
		restored = resolveConfigPaths(restored, filepath.Dir(backupConfig))
		dbName := filepath.Base(config.DatabasePath)
		if dbName == "." || dbName == "" || dbName == string(filepath.Separator) {
			dbName = "controller.db"
		}
		restored.DatabasePath = filepath.Join(destination, dbName)
		restored.MasterKeyPath = filepath.Join(destination, "master.key")
		restored.CAKeyPath = filepath.Join(destination, "ca", "ca.key")
		restored.CACertPath = filepath.Join(destination, "ca", "ca.crt")
		restored.TLSKeyPath = filepath.Join(destination, "tls", "controller.key")
		restored.TLSCertPath = filepath.Join(destination, "tls", "controller.crt")
		if err := SaveConfig(filepath.Join(destination, "controller.json"), restored); err != nil {
			return fmt.Errorf("write restored controller config: %w", err)
		}
	}
	return nil
}

func configPathFor(config Config) string {
	if strings.TrimSpace(config.SourcePath) != "" {
		return filepath.Clean(config.SourcePath)
	}
	// Config paths all live below one controller directory by default. When a
	// caller loaded a custom config, use the conventional sibling path.
	return filepath.Join(filepath.Dir(config.DatabasePath), "controller.json")
}

func validateConfiguredDatabase(ctx context.Context, config Config, backend databaseBackend) error {
	db, openedBackend, err := openConfiguredDatabase(ctx, config)
	if err != nil {
		return fmt.Errorf("open controller database: %w", err)
	}
	defer db.Close()
	if openedBackend != backend {
		return errors.New("database backend changed while opening")
	}
	compatible, empty, err := inspectDatabase(ctx, db, backend)
	if err != nil {
		return fmt.Errorf("inspect controller database: %w", err)
	}
	if empty || !compatible {
		return ErrIncompatibleDatabase
	}
	if err := validateRequiredTables(ctx, db, backend); err != nil {
		return fmt.Errorf("validate controller database: %w", err)
	}
	return nil
}

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
	key, err := LoadOrCreateMasterKey(restored.MasterKeyPath)
	if err != nil {
		return err
	}
	store, err := OpenStoreWithConfig(restored, key)
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
	compatible, empty, err := inspectDatabase(ctx, db, backend)
	if err != nil {
		return fmt.Errorf("inspect target PostgreSQL database: %w", err)
	}
	if compatible || !empty {
		return errors.New("target PostgreSQL schema is not empty")
	}
	return nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	b, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return atomicfile.AtomicWrite(destination, b, mode)
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func writeBackupManifest(directory string, createdAt time.Time, databaseSchema string, payloadFiles []string) error {
	manifest := backupManifest{Version: backupManifestVersion, CreatedAt: createdAt.UTC(), DatabaseSchema: databaseSchema, Files: make([]backupManifestFile, 0, len(payloadFiles))}
	for _, name := range payloadFiles {
		entry, err := hashBackupFile(context.Background(), filepath.Join(directory, name))
		if err != nil {
			return fmt.Errorf("hash backup file %q: %w", name, err)
		}
		manifest.Files = append(manifest.Files, entry)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.AtomicWrite(filepath.Join(directory, "manifest.json"), append(data, '\n'), 0o600)
}

func verifyBackupManifest(directory string, payloadFiles []string) (backupManifest, error) {
	data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return backupManifest{}, fmt.Errorf("backup manifest: %w", err)
	}
	var manifest backupManifest
	if err := jsonutil.DecodeStrict(data, &manifest); err != nil {
		return backupManifest{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if manifest.Version != backupManifestVersion || manifest.CreatedAt.IsZero() {
		return backupManifest{}, errors.New("backup manifest version or timestamp is invalid")
	}
	expected := make(map[string]struct{}, len(payloadFiles))
	for _, name := range payloadFiles {
		expected[name] = struct{}{}
	}
	if len(manifest.Files) != len(expected) {
		return backupManifest{}, errors.New("backup manifest does not contain the required payload set")
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, entry := range manifest.Files {
		if _, ok := expected[entry.Name]; !ok || strings.ContainsAny(entry.Name, `/\\`) {
			return backupManifest{}, fmt.Errorf("backup manifest contains an invalid file name %q", entry.Name)
		}
		if _, ok := seen[entry.Name]; ok {
			return backupManifest{}, fmt.Errorf("backup manifest contains duplicate file %q", entry.Name)
		}
		seen[entry.Name] = struct{}{}
		if entry.Size < 0 || len(entry.SHA256) != sha256.Size*2 {
			return backupManifest{}, fmt.Errorf("backup manifest entry %q is invalid", entry.Name)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil {
			return backupManifest{}, fmt.Errorf("backup manifest entry %q has an invalid digest", entry.Name)
		}
		actual, err := hashBackupFile(context.Background(), filepath.Join(directory, entry.Name))
		if err != nil {
			return backupManifest{}, fmt.Errorf("verify backup file %q: %w", entry.Name, err)
		}
		if actual.Size != entry.Size || !strings.EqualFold(actual.SHA256, entry.SHA256) {
			return backupManifest{}, fmt.Errorf("backup file %q failed manifest verification", entry.Name)
		}
	}
	if len(seen) != len(expected) {
		return backupManifest{}, errors.New("backup manifest is missing a required payload file")
	}
	return manifest, nil
}

func hashBackupFile(ctx context.Context, path string) (backupManifestFile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	info, err := os.Lstat(path)
	if err != nil {
		return backupManifestFile{}, err
	}
	if !info.Mode().IsRegular() {
		return backupManifestFile{}, errors.New("backup payload is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return backupManifestFile{}, err
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return backupManifestFile{}, err
	} else if !info.Mode().IsRegular() {
		return backupManifestFile{}, errors.New("backup payload is not a regular file")
	}
	digest := sha256.New()
	var size int64
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return backupManifestFile{}, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			if _, err := digest.Write(buffer[:n]); err != nil {
				return backupManifestFile{}, err
			}
			size += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return backupManifestFile{}, readErr
		}
	}
	return backupManifestFile{Name: filepath.Base(path), Size: size, SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
}
