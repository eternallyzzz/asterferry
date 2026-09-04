package controller

import (
	"asterferry/internal/atomicfile"
	"asterferry/internal/jsonutil"
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
)

func copyFile(source, destination string, mode os.FileMode) error {
	b, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return atomicfile.AtomicWrite(destination, b, mode)
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func writeBackupManifest(directory string, createdAt time.Time, databaseSchema string, payloadFiles []string) error {
	manifest := backupManifest{
		Version:          backupManifestVersion,
		CreatedAt:        createdAt.UTC(),
		ControllerSchema: databaseSchemaMarker(),
		DatabaseSchema:   databaseSchema,
		Files:            make([]backupManifestFile, 0, len(payloadFiles)),
	}
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
	if manifest.Version != backupManifestVersion || manifest.CreatedAt.IsZero() || manifest.ControllerSchema != databaseSchemaMarker() {
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
