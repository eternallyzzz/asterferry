package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteCreatesParentAndAppliesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "secret")
	if err := Write(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "secret" {
		t.Fatalf("written data = %q, err=%v", data, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("file mode = %o, want 600", got)
		}
	}
}

func TestAtomicWriteReplacesAndCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("atomic data = %q, err=%v", data, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, tempPrefix[:len(tempPrefix)-1]+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %#v", matches)
	}
}
