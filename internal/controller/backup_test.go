package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupPublishesAndRestoreVerifiesManifest(t *testing.T) {
	controllerDir := filepath.Join(t.TempDir(), "controller")
	result, err := Init(context.Background(), InitOptions{Dir: controllerDir, Password: "a-very-long-admin-password"})
	if err != nil {
		t.Fatal(err)
	}
	backupRoot := t.TempDir()
	backupDir, err := Backup(context.Background(), result.Config, backupRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "manifest.json")); err != nil {
		t.Fatalf("backup manifest is missing: %v", err)
	}
	for _, name := range append(append([]string(nil), backupPayloadFiles...), "manifest.json") {
		if _, err := os.Stat(filepath.Join(backupDir, name)); err != nil {
			t.Fatalf("backup file %q is missing: %v", name, err)
		}
	}

	destination := filepath.Join(t.TempDir(), "restored")
	if err := Restore(result.Config, backupDir, destination); err != nil {
		t.Fatal(err)
	}
	restoredConfig, err := LoadConfig(filepath.Join(destination, "controller.json"))
	if err != nil {
		t.Fatal(err)
	}
	if restoredConfig.DatabasePath != filepath.Join(destination, "controller.db") || restoredConfig.MasterKeyPath != filepath.Join(destination, "master.key") {
		t.Fatalf("restored config paths were not rebased: %#v", restoredConfig)
	}
	key, err := LoadOrCreateMasterKey(restoredConfig.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(restoredConfig.DatabasePath, key)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	if err := os.WriteFile(filepath.Join(backupDir, "controller.json"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	untouched := filepath.Join(t.TempDir(), "untouched")
	if err := Restore(result.Config, backupDir, untouched); err == nil {
		t.Fatal("tampered backup was restored")
	}
	if _, err := os.Stat(untouched); !os.IsNotExist(err) {
		t.Fatalf("manifest failure created a restore destination: %v", err)
	}
}

func TestInitForcePreservesUnrelatedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "controller")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "operator-notes.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Init(context.Background(), InitOptions{Dir: dir, Password: "a-very-long-admin-password", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(keep)
	if err != nil || string(data) != "keep" {
		t.Fatalf("unrelated file was not preserved: %q %v", data, err)
	}
	if result.ConfigPath != filepath.Join(dir, "controller.json") {
		t.Fatalf("unexpected initialized config path: %s", result.ConfigPath)
	}
}
