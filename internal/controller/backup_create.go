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
	payloadFiles, err := backupPayloadFilesWithNodeInstallers(config, backend)
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
		repositories, err := OpenControllerRepositoriesWithConfig(config, masterKey)
		if err != nil {
			return "", err
		}
		defer repositories.Close()
		backupDB := filepath.Join(staging, "controller.db")
		// VACUUM INTO produces a consistent copy even while API writes continue.
		if _, err := repositories.Resources.db.ExecContext(ctx, `VACUUM INTO ?`, backupDB); err != nil {
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
	for _, item := range []struct {
		name string
		mode os.FileMode
	}{
		{bootstrapInstallerUnix, 0o600},
		{bootstrapInstallerWindows, 0o600},
		{nodeReleaseMetadataName, 0o600},
	} {
		if _, err := os.Stat(nodeInstallerPath(config, item.name)); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("inspect Node installer asset %q: %w", item.name, err)
		}
		if err := copyFile(nodeInstallerPath(config, item.name), filepath.Join(staging, item.name), item.mode); err != nil {
			return "", fmt.Errorf("backup Node installer asset %q: %w", item.name, err)
		}
	}
	if err := writeBackupManifest(staging, time.Now().UTC(), databaseSchema, payloadFiles); err != nil {
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

func backupPayloadFilesWithNodeInstallers(config Config, backend databaseBackend) ([]string, error) {
	files := append([]string(nil), backupPayloadFilesForBackend(backend)...)
	for _, name := range backupNodeInstallerFiles {
		info, err := os.Stat(nodeInstallerPath(config, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect Node installer asset %q: %w", name, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("Node installer asset %q is a directory", name)
		}
		files = append(files, name)
	}
	return files, nil
}
