package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"asterferry/internal/update"
)

const (
	controllerCrashChildEnv  = "ASTERFERRY_CONTROLLER_UPDATE_CRASH_CHILD"
	controllerCrashMarkerEnv = "ASTERFERRY_CONTROLLER_UPDATE_CRASH_MARKER"
	controllerCrashParentEnv = "ASTERFERRY_CONTROLLER_UPDATE_CRASH_PARENT"
	controllerCrashBinaryEnv = "ASTERFERRY_CONTROLLER_UPDATE_CRASH_BINARY"
	controllerCrashStagedEnv = "ASTERFERRY_CONTROLLER_UPDATE_CRASH_STAGED"
	controllerCrashBackupEnv = "ASTERFERRY_CONTROLLER_UPDATE_CRASH_BACKUP"
	controllerCrashStatusEnv = "ASTERFERRY_CONTROLLER_UPDATE_CRASH_STATUS"
	controllerCrashTargetEnv = "ASTERFERRY_CONTROLLER_UPDATE_CRASH_TARGET"
)

func TestMain(m *testing.M) {
	if os.Getenv(controllerCrashChildEnv) == "after-replace" {
		runControllerCrashChild()
	}
	os.Exit(m.Run())
}

func runControllerCrashChild() {
	parentPID, err := strconv.Atoi(os.Getenv(controllerCrashParentEnv))
	if err != nil {
		os.Exit(2)
	}
	options := update.ReplacementOptions{
		ParentPID:   parentPID,
		BinaryPath:  os.Getenv(controllerCrashBinaryEnv),
		StagedPath:  os.Getenv(controllerCrashStagedEnv),
		BackupPath:  os.Getenv(controllerCrashBackupEnv),
		Target:      os.Getenv(controllerCrashTargetEnv),
		TargetState: UpdateStateUpToDate,
		Mode:        "systemd",
		StatusPath:  os.Getenv(controllerCrashStatusEnv),
		Timeout:     10 * time.Second,
		AfterReplace: func() {
			_ = os.WriteFile(os.Getenv(controllerCrashMarkerEnv), []byte("replaced\n"), 0o600)
			select {}
		},
	}
	if err := update.RunReplacementHelper(context.Background(), options); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestControllerReplacementRecoversAfterHelperIsKilledImmediatelyAfterRename(t *testing.T) {
	useBootstrapTestBuildVersion(t, "1.1.0")
	root := t.TempDir()
	target := "1.1.0"
	ready := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ready.Close()

	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(testExecutable)
	if err != nil {
		t.Fatal(err)
	}
	staged := append(append([]byte(nil), original...), []byte("staged-controller-binary")...)
	binaryPath := filepath.Join(root, "asterferry-controller")
	stagedPath := filepath.Join(root, "staged-controller")
	backupPath := filepath.Join(root, "previous-controller")
	statusPath := filepath.Join(root, "controller-update.json")
	markerPath := filepath.Join(root, "rename-complete")
	for path, data := range map[string][]byte{binaryPath: original, stagedPath: staged} {
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	parentPID := startExitedControllerTestProcess(t)
	child := exec.Command(testExecutable, "-test.run=^$")
	child.Env = append(os.Environ(),
		controllerCrashChildEnv+"=after-replace",
		controllerCrashMarkerEnv+"="+markerPath,
		controllerCrashParentEnv+"="+strconv.Itoa(parentPID),
		controllerCrashBinaryEnv+"="+binaryPath,
		controllerCrashStagedEnv+"="+stagedPath,
		controllerCrashBackupEnv+"="+backupPath,
		controllerCrashStatusEnv+"="+statusPath,
		controllerCrashTargetEnv+"="+target,
	)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited && child.Process != nil && update.ProcessAlive(child.Process.Pid) {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	waitForControllerFile(t, markerPath)

	result, err := update.ReadReplacementResult(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != update.ReplacementStateReplacing {
		t.Fatalf("journal state before forced helper termination = %#v, want replacing", result)
	}
	if got, err := os.ReadFile(binaryPath); err != nil || !bytes.Equal(got, staged) {
		t.Fatalf("live binary after helper interruption = %d bytes, %v", len(got), err)
	}
	if got, err := os.ReadFile(backupPath); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("backup binary after helper interruption = %d bytes, %v", len(got), err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("interrupted update helper exited cleanly")
	}
	waited = true

	config := DefaultConfig(root)
	config.HTTPListen = ready.Listener.Addr().String()
	config.UpdateCheckEnabled = false
	repositories, err := openTestRepositories(filepath.Join(root, "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	restarted := NewUpdateManager(config, repositories.Resources)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarted.Start(ctx)
	waitForControllerState(t, statusPath, UpdateStateUpToDate)
	waitForPersistedControllerState(t, repositories.Resources, config, UpdateStateUpToDate, target)

	result, err = update.ReadReplacementResult(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != UpdateStateUpToDate || result.Version != target {
		t.Fatalf("recovered replacement result = %#v", result)
	}
	if _, err := os.Stat(backupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous binary was not cleaned after recovery: %v", err)
	}
	persisted, err := repositories.Resources.GetControllerUpdateState(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != UpdateStateUpToDate || persisted.TargetVersion != target || persisted.CurrentVersion != target {
		t.Fatalf("persisted recovery state = %#v", persisted)
	}
}

func TestControllerReplacementMismatchBecomesManualRequired(t *testing.T) {
	useBootstrapTestBuildVersion(t, "1.1.0")
	root := t.TempDir()
	config := DefaultConfig(root)
	config.UpdateCheckEnabled = false
	repositories, err := openTestRepositories(filepath.Join(root, "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	manager := NewUpdateManager(config, repositories.Resources)
	statusPath := manager.controllerUpdateStatusPath()
	backupPath := filepath.Join(root, "previous-controller")
	if err := os.WriteFile(backupPath, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	update.WriteReplacementResult(statusPath, update.ReplacementResult{
		Version:    "1.2.0",
		State:      update.ReplacementStateWaiting,
		BinaryPath: filepath.Join(root, "asterferry-controller"),
		BackupPath: backupPath,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)
	waitForControllerState(t, statusPath, UpdateStateManualRequired)
	result, err := update.ReadReplacementResult(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != UpdateStateManualRequired || result.Error == "" {
		t.Fatalf("mismatched replacement result = %#v", result)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("manual recovery removed the previous binary: %v", err)
	}
}

func TestPreparedControllerReplacementReconcilesDiskState(t *testing.T) {
	useBootstrapTestBuildVersion(t, "1.1.0")
	ready := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ready.Close()

	for _, test := range []struct {
		name           string
		wantState      string
		wantBackup     bool
		wantStaged     bool
		setup          func(t *testing.T, binaryPath, stagedPath, backupPath string)
		omitIdentities bool
	}{
		{
			name:      "target installed while journal is prepared",
			wantState: UpdateStateUpToDate,
			setup: func(t *testing.T, binaryPath, stagedPath, backupPath string) {
				t.Helper()
				writeControllerRecoveryFile(t, binaryPath, []byte("target"))
				writeControllerRecoveryFile(t, stagedPath, []byte("target"))
				writeControllerRecoveryFile(t, backupPath, []byte("original"))
			},
		},
		{
			name:      "original remains while journal is prepared",
			wantState: UpdateStateFailed,
			setup: func(t *testing.T, binaryPath, stagedPath, _ string) {
				t.Helper()
				writeControllerRecoveryFile(t, binaryPath, []byte("original"))
				writeControllerRecoveryFile(t, stagedPath, []byte("target"))
			},
		},
		{
			name:       "partial replacement",
			wantState:  UpdateStateManualRequired,
			wantBackup: true,
			wantStaged: true,
			setup: func(t *testing.T, _, stagedPath, backupPath string) {
				t.Helper()
				writeControllerRecoveryFile(t, stagedPath, []byte("target"))
				writeControllerRecoveryFile(t, backupPath, []byte("original"))
			},
		},
		{
			name:           "legacy journal does not bypass reconciliation",
			wantState:      UpdateStateManualRequired,
			wantBackup:     true,
			wantStaged:     true,
			omitIdentities: true,
			setup: func(t *testing.T, binaryPath, stagedPath, backupPath string) {
				t.Helper()
				writeControllerRecoveryFile(t, binaryPath, []byte("target"))
				writeControllerRecoveryFile(t, stagedPath, []byte("target"))
				writeControllerRecoveryFile(t, backupPath, []byte("original"))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			config := DefaultConfig(root)
			config.HTTPListen = ready.Listener.Addr().String()
			config.UpdateCheckEnabled = false
			repositories, err := openTestRepositories(filepath.Join(root, "controller.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer repositories.Close()
			manager := NewUpdateManager(config, repositories.Resources)
			binaryPath := filepath.Join(root, "asterferry")
			stagedPath := filepath.Join(root, "updates", "staged")
			backupPath := filepath.Join(root, "updates", "previous")
			if err := os.MkdirAll(filepath.Dir(stagedPath), 0o700); err != nil {
				t.Fatal(err)
			}
			test.setup(t, binaryPath, stagedPath, backupPath)
			result := update.ReplacementResult{
				Version:        "1.1.0",
				State:          update.ReplacementStatePrepared,
				BinaryPath:     binaryPath,
				StagedPath:     stagedPath,
				BackupPath:     backupPath,
				OriginalSHA256: controllerRecoverySHA256([]byte("original")),
				TargetSHA256:   controllerRecoverySHA256([]byte("target")),
			}
			if test.omitIdentities {
				result.SchemaVersion = 1
				result.OriginalSHA256 = ""
				result.TargetSHA256 = ""
			}
			if err := update.WriteReplacementResult(manager.controllerUpdateStatusPath(), result); err != nil {
				t.Fatal(err)
			}

			manager.recoverPendingReplacement(context.Background())
			recovered, err := update.ReadReplacementResult(manager.controllerUpdateStatusPath())
			if err != nil {
				t.Fatal(err)
			}
			if recovered.State != test.wantState {
				t.Fatalf("recovered replacement result = %#v, want state %q", recovered, test.wantState)
			}
			if _, err := os.Stat(backupPath); (err == nil) != test.wantBackup {
				t.Fatalf("backup presence = %v, want %v (err=%v)", err == nil, test.wantBackup, err)
			}
			if _, err := os.Stat(stagedPath); (err == nil) != test.wantStaged {
				t.Fatalf("staged presence = %v, want %v (err=%v)", err == nil, test.wantStaged, err)
			}
		})
	}
}

func writeControllerRecoveryFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
}

func controllerRecoverySHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func TestControllerStatusReportsPendingReplacementStates(t *testing.T) {
	useBootstrapTestBuildVersion(t, "1.1.0")
	root := t.TempDir()
	config := DefaultConfig(root)
	config.UpdateCheckEnabled = false
	repositories, err := openTestRepositories(filepath.Join(root, "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	manager := NewUpdateManager(config, repositories.Resources)
	for _, pendingState := range []string{
		update.ReplacementStatePrepared,
		update.ReplacementStateReplacing,
		update.ReplacementStateWaiting,
		update.ReplacementStateRecovering,
	} {
		update.WriteReplacementResult(manager.controllerUpdateStatusPath(), update.ReplacementResult{
			Version: "1.1.0",
			State:   pendingState,
		})
		state, err := manager.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if state.State != UpdateStateApplying || state.TargetVersion != "1.1.0" {
			t.Fatalf("Status for pending state %q = %#v", pendingState, state)
		}
	}
}

func startExitedControllerTestProcess(t *testing.T) int {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^$")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if err := command.Wait(); err != nil {
		t.Fatalf("test parent process failed: %v", err)
	}
	return pid
}

func waitForControllerFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForControllerState(t *testing.T, path, expected string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if result, err := update.ReadReplacementResult(path); err == nil {
			if result.State == expected {
				return
			}
			if result.State == UpdateStateManualRequired && expected != UpdateStateManualRequired {
				t.Fatalf("replacement entered manual_required while waiting for %s: %#v", expected, result)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for replacement state %q", expected)
}

func waitForPersistedControllerState(t *testing.T, resources *ResourceRepository, config Config, expectedState, expectedVersion string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state, err := resources.GetControllerUpdateState(context.Background(), config)
		if err == nil && state.State == expectedState && state.TargetVersion == expectedVersion && state.CurrentVersion == expectedVersion {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for persisted Controller state %q/%q", expectedState, expectedVersion)
}
