package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
