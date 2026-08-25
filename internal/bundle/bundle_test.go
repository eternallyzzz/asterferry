package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAndEnsureRuntimeDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gateway.yaml", "agent.yaml"} {
		if err := os.WriteFile(filepath.Join(root, "config", name), []byte("role: test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.EnsureRuntimeDirs(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Config("unknown"); err == nil {
		t.Fatal("unknown bundle role should be rejected")
	}
	for _, path := range []string{b.RunDir, b.LogsDir} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Fatalf("runtime directory %q: %v", path, err)
		}
	}
}

func TestOpenRejectsIncompleteBundle(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("incomplete bundle should be rejected")
	}
	if _, err := Open(""); err == nil {
		t.Fatal("empty bundle path should be rejected")
	}
}
