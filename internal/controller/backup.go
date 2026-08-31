package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"asterferry/internal/atomicfile"
	"asterferry/internal/jsonutil"
)

const backupManifestVersion = 1

type backupManifest struct {
	Version   int                  `json:"version"`
	CreatedAt time.Time            `json:"created_at"`
	Files     []backupManifestFile `json:"files"`
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
	store, err := OpenStore(config.DatabasePath, masterKey)
	if err != nil {
		return "", err
	}
	defer store.Close()
	backupDB := filepath.Join(staging, "controller.db")
	// VACUUM INTO produces a consistent copy even while API writes continue.
	if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, backupDB); err != nil {
		return "", fmt.Errorf("backup sqlite database: %w", err)
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
	if err := writeBackupManifest(staging, time.Now().UTC()); err != nil {
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
	source = filepath.Clean(strings.TrimSpace(source))
	if source == "." || source == "" {
		return errors.New("backup source is required")
	}
	if destination == "" {
		destination = filepath.Dir(config.DatabasePath)
	}
	destination = filepath.Clean(destination)
	if err := verifyBackupManifest(source); err != nil {
		return err
	}
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

func writeBackupManifest(directory string, createdAt time.Time) error {
	manifest := backupManifest{Version: backupManifestVersion, CreatedAt: createdAt.UTC(), Files: make([]backupManifestFile, 0, len(backupPayloadFiles))}
	for _, name := range backupPayloadFiles {
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

func verifyBackupManifest(directory string) error {
	data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return fmt.Errorf("backup manifest: %w", err)
	}
	var manifest backupManifest
	if err := jsonutil.DecodeStrict(data, &manifest); err != nil {
		return fmt.Errorf("decode backup manifest: %w", err)
	}
	if manifest.Version != backupManifestVersion || manifest.CreatedAt.IsZero() {
		return errors.New("backup manifest version or timestamp is invalid")
	}
	expected := make(map[string]struct{}, len(backupPayloadFiles))
	for _, name := range backupPayloadFiles {
		expected[name] = struct{}{}
	}
	if len(manifest.Files) != len(expected) {
		return errors.New("backup manifest does not contain the required payload set")
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, entry := range manifest.Files {
		if _, ok := expected[entry.Name]; !ok || strings.ContainsAny(entry.Name, `/\\`) {
			return fmt.Errorf("backup manifest contains an invalid file name %q", entry.Name)
		}
		if _, ok := seen[entry.Name]; ok {
			return fmt.Errorf("backup manifest contains duplicate file %q", entry.Name)
		}
		seen[entry.Name] = struct{}{}
		if entry.Size < 0 || len(entry.SHA256) != sha256.Size*2 {
			return fmt.Errorf("backup manifest entry %q is invalid", entry.Name)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil {
			return fmt.Errorf("backup manifest entry %q has an invalid digest", entry.Name)
		}
		actual, err := hashBackupFile(context.Background(), filepath.Join(directory, entry.Name))
		if err != nil {
			return fmt.Errorf("verify backup file %q: %w", entry.Name, err)
		}
		if actual.Size != entry.Size || !strings.EqualFold(actual.SHA256, entry.SHA256) {
			return fmt.Errorf("backup file %q failed manifest verification", entry.Name)
		}
	}
	if len(seen) != len(expected) {
		return errors.New("backup manifest is missing a required payload file")
	}
	return nil
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
