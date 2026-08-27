package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"asterferry/internal/atomicfile"
)

func Backup(ctx context.Context, config Config, destination string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := config.Validate(); err != nil {
		return "", err
	}
	destination = filepath.Clean(strings.TrimSpace(destination))
	if destination == "." || destination == "" {
		return "", errors.New("backup destination is required")
	}
	if info, err := os.Stat(destination); err == nil && info.IsDir() {
		destination = filepath.Join(destination, "asterferry-controller-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", err
	}
	store, err := OpenStore(config.DatabasePath)
	if err != nil {
		return "", err
	}
	defer store.Close()
	backupDB := filepath.Join(destination, "controller.db")
	// VACUUM INTO produces a consistent copy even while API writes continue.
	if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, backupDB); err != nil {
		return "", fmt.Errorf("backup sqlite database: %w", err)
	}
	if err := copyFile(configPathFor(config), filepath.Join(destination, "controller.json"), 0o600); err != nil {
		return "", err
	}
	if err := copyFile(config.MasterKeyPath, filepath.Join(destination, "master.key"), 0o600); err != nil {
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
		if err := copyFile(item.source, filepath.Join(destination, item.name), 0o600); err != nil {
			return "", err
		}
	}
	return destination, nil
}

func Restore(config Config, source, destination string) error {
	source = filepath.Clean(strings.TrimSpace(source))
	if source == "." || source == "" {
		return errors.New("backup source is required")
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

func copyFile(source, destination string, mode os.FileMode) error {
	b, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return atomicfile.AtomicWrite(destination, b, mode)
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }
