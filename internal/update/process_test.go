package update

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	updateChildEnv   = "ASTERFERRY_UPDATE_TEST_CHILD"
	updateStatusEnv  = "ASTERFERRY_UPDATE_TEST_STATUS"
	updateVersionEnv = "ASTERFERRY_UPDATE_TEST_VERSION"
)

func TestMain(m *testing.M) {
	switch os.Getenv(updateChildEnv) {
	case "parent", "fail":
		os.Exit(0)
	case "healthy":
		writeReplacementResult(os.Getenv(updateStatusEnv), ReplacementResult{
			ActionID: "child-action",
			Version:  os.Getenv(updateVersionEnv),
			State:    "healthy",
		})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunReplacementHelperSelfManagedSuccess(t *testing.T) {
	root := t.TempDir()
	binaryPath, stagedPath := copyTestBinaryPair(t, root, false)
	statusPath := filepath.Join(root, "node-update.json")
	pidPath := filepath.Join(root, "node.pid")
	target := "2.0.0"
	t.Setenv(updateChildEnv, "healthy")
	t.Setenv(updateStatusEnv, statusPath)
	t.Setenv(updateVersionEnv, target)
	parentPID := startUpdateTestChild(t, "parent")

	err := RunReplacementHelper(context.Background(), ReplacementOptions{
		ParentPID:   parentPID,
		BinaryPath:  binaryPath,
		StagedPath:  stagedPath,
		BackupPath:  filepath.Join(root, "previous"),
		ReadyPath:   statusPath,
		TargetState: "healthy",
		Target:      target,
		Mode:        "wsl",
		RestartArgs: []string{"-test.run=^$"},
		StatusPath:  statusPath,
		PIDFile:     pidPath,
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ReadReplacementResult(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != target || result.State != "healthy" {
		t.Fatalf("successful replacement result = %#v", result)
	}
	if result.SchemaVersion != ReplacementResultSchemaVersion || result.BinaryPath == "" || result.BackupPath == "" || result.OriginalSHA256 == "" || result.TargetSHA256 == "" {
		t.Fatalf("replacement journal fields = %#v", result)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("replacement PID file is missing: %v", err)
	}
}

func TestRunReplacementHelperSelfManagedRollsBackOnHealthFailure(t *testing.T) {
	root := t.TempDir()
	binaryPath, stagedPath := copyTestBinaryPair(t, root, true)
	original, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(root, "node-update.json")
	t.Setenv(updateChildEnv, "fail")
	t.Setenv(updateStatusEnv, statusPath)
	t.Setenv(updateVersionEnv, "2.0.0")
	parentPID := startUpdateTestChild(t, "parent")

	err = RunReplacementHelper(context.Background(), ReplacementOptions{
		ParentPID:   parentPID,
		BinaryPath:  binaryPath,
		StagedPath:  stagedPath,
		BackupPath:  filepath.Join(root, "previous"),
		ReadyPath:   statusPath,
		TargetState: "healthy",
		Target:      "2.0.0",
		Mode:        "wsl",
		RestartArgs: []string{"-test.run=^$"},
		StatusPath:  statusPath,
		PIDFile:     filepath.Join(root, "node.pid"),
		Timeout:     3 * time.Second,
	})
	if err == nil {
		t.Fatal("unhealthy replacement was accepted")
	}
	result, readErr := ReadReplacementResult(statusPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if result.State != "rolled_back" {
		t.Fatalf("rollback result = %#v", result)
	}
	restored, readErr := os.ReadFile(binaryPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(restored) != string(original) {
		t.Fatalf("rollback did not restore the previous executable: original=%d/%x restored=%d/%x", len(original), sha256.Sum256(original), len(restored), sha256.Sum256(restored))
	}
}

func TestWaitForHTTPSHealthRetriesUntilReady(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := WaitForHTTPSHealth(context.Background(), server.URL+"/readyz"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 2 {
		t.Fatalf("health probe requests = %d, want a retry", requests.Load())
	}
	if err := WaitForHTTPSHealth(context.Background(), "://invalid"); err == nil {
		t.Fatal("invalid health endpoint was accepted")
	}
	if err := WaitForHTTPSHealth(context.Background(), "https://example.com/readyz"); err == nil {
		t.Fatal("remote health endpoint was accepted")
	}
}

func TestReplacementRecoveryStatesAreJournaled(t *testing.T) {
	for _, state := range []string{ReplacementStatePrepared, ReplacementStateReplacing, ReplacementStateWaiting, ReplacementStateRecovering} {
		if !ReplacementNeedsRecovery(state) {
			t.Fatalf("journal state %q was not marked recoverable", state)
		}
	}
	for _, state := range []string{"healthy", "rolled_back", "failed", "manual_required"} {
		if ReplacementNeedsRecovery(state) {
			t.Fatalf("terminal journal state %q was marked recoverable", state)
		}
	}
}

func TestReadReplacementResultRejectsUnsupportedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replacement.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":2,"state":"waiting"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReplacementResult(path); err == nil {
		t.Fatal("replacement journal schema 2 was accepted")
	}
}

func TestClassifyReplacementFiles(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "binary")
	stagedPath := filepath.Join(root, "staged")
	backupPath := filepath.Join(root, "backup")
	original := []byte("original executable")
	target := []byte("target executable")
	tampered := []byte("tampered backup")
	if err := os.WriteFile(binaryPath, original, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedPath, target, 0o755); err != nil {
		t.Fatal(err)
	}
	originalSHA256, err := replacementFileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	targetSHA256, err := replacementFileSHA256(stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	result := ReplacementResult{
		SchemaVersion:  ReplacementResultSchemaVersion,
		BinaryPath:     binaryPath,
		StagedPath:     stagedPath,
		BackupPath:     backupPath,
		OriginalSHA256: originalSHA256,
		TargetSHA256:   targetSHA256,
	}
	for _, test := range []struct {
		name  string
		setup func(t *testing.T)
		want  ReplacementDiskState
	}{
		{
			name: "target installed",
			setup: func(t *testing.T) {
				t.Helper()
				removeReplacementTestFiles(t, binaryPath, stagedPath, backupPath)
				writeReplacementTestFile(t, binaryPath, target)
				writeReplacementTestFile(t, backupPath, original)
			},
			want: ReplacementDiskStateTargetInstalled,
		},
		{
			name: "target installed with staged residue",
			setup: func(t *testing.T) {
				t.Helper()
				removeReplacementTestFiles(t, binaryPath, stagedPath, backupPath)
				writeReplacementTestFile(t, binaryPath, target)
				writeReplacementTestFile(t, stagedPath, target)
				writeReplacementTestFile(t, backupPath, original)
			},
			want: ReplacementDiskStateTargetInstalled,
		},
		{
			name: "original intact",
			setup: func(t *testing.T) {
				t.Helper()
				removeReplacementTestFiles(t, binaryPath, stagedPath, backupPath)
				writeReplacementTestFile(t, binaryPath, original)
				writeReplacementTestFile(t, stagedPath, target)
			},
			want: ReplacementDiskStateOriginalIntact,
		},
		{
			name: "partial",
			setup: func(t *testing.T) {
				t.Helper()
				removeReplacementTestFiles(t, binaryPath, stagedPath, backupPath)
				writeReplacementTestFile(t, stagedPath, target)
				writeReplacementTestFile(t, backupPath, original)
			},
			want: ReplacementDiskStatePartial,
		},
		{
			name: "tampered backup",
			setup: func(t *testing.T) {
				t.Helper()
				removeReplacementTestFiles(t, binaryPath, stagedPath, backupPath)
				writeReplacementTestFile(t, binaryPath, target)
				writeReplacementTestFile(t, backupPath, tampered)
			},
			want: ReplacementDiskStateIndeterminate,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.setup(t)
			got, err := ClassifyReplacementFiles(result)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("replacement disk state = %q, want %q", got, test.want)
			}
		})
	}
}

func writeReplacementTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func removeReplacementTestFiles(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func TestPIDFileOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.pid")
	WritePIDFile(path, 1234)
	RemovePIDFileIfOwner(path, 4321)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	RemovePIDFileIfOwner(path, 1234)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PID file after owner removal = %v", err)
	}
}

func copyTestBinaryPair(t *testing.T, root string, makeStagedDifferent bool) (string, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(root, "asterferry-test")
	if strings.EqualFold(filepath.Ext(executable), ".exe") {
		binaryPath += ".exe"
	}
	stagedPath := filepath.Join(root, "staged")
	if err := os.WriteFile(binaryPath, data, 0o755); err != nil {
		t.Fatal(err)
	}
	staged := append([]byte(nil), data...)
	if makeStagedDifferent {
		staged = append(staged, []byte("stage-marker")...)
	}
	if err := os.WriteFile(stagedPath, staged, 0o755); err != nil {
		t.Fatal(err)
	}
	return binaryPath, stagedPath
}

func startUpdateTestChild(t *testing.T, mode string) int {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^$")
	command.Env = append(os.Environ(), updateChildEnv+"="+mode)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if err := command.Wait(); err != nil {
		t.Fatalf("update test child failed: %v", err)
	}
	return pid
}
