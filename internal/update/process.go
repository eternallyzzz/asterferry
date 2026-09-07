package update

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	ReplacementResultSchemaVersion = 1
	ReplacementStatePrepared       = "prepared"
	ReplacementStateReplacing      = "replacing"
	ReplacementStateWaiting        = "waiting"
	ReplacementStateRecovering     = "recovering"
)

type ReplacementOptions struct {
	ParentPID   int
	BinaryPath  string
	StagedPath  string
	BackupPath  string
	HealthURL   string
	ReadyPath   string
	TargetState string
	Target      string
	ActionID    string
	Mode        string
	ServiceName string
	RestartArgs []string
	StatusPath  string
	PIDFile     string
	Timeout     time.Duration
	// AfterReplace is a test-only crash-injection seam. Production callers
	// leave it nil; when set it runs after the live executable has been
	// replaced and before the helper records the waiting state.
	AfterReplace func()
}

type ReplacementResult struct {
	SchemaVersion int       `json:"schema_version,omitempty"`
	ActionID      string    `json:"action_id,omitempty"`
	Version       string    `json:"version"`
	State         string    `json:"state"`
	Error         string    `json:"error,omitempty"`
	BinaryPath    string    `json:"binary_path,omitempty"`
	StagedPath    string    `json:"staged_path,omitempty"`
	BackupPath    string    `json:"backup_path,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// RunReplacementHelper waits for the daemon that launched it, replaces its
// executable, and waits for the new process to become healthy. It is shared by
// Controller and Node so rollback behavior stays identical on Windows,
// systemd and WSL.
func RunReplacementHelper(ctx context.Context, options ReplacementOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.ParentPID <= 0 || strings.TrimSpace(options.BinaryPath) == "" || strings.TrimSpace(options.StagedPath) == "" {
		return errors.New("update helper requires parent pid, binary path and staged path")
	}
	if options.Timeout <= 0 {
		options.Timeout = HealthTimeout
	}
	if options.TargetState == "" {
		options.TargetState = "healthy"
	}
	writeReplacementResult(options.StatusPath, replacementResult(options, ReplacementStatePrepared, nil))
	if err := WaitForProcessExit(ctx, options.ParentPID, time.Minute); err != nil {
		writeReplacementResult(options.StatusPath, replacementResult(options, "failed", err))
		return err
	}
	writeReplacementResult(options.StatusPath, replacementResult(options, ReplacementStateReplacing, nil))
	if err := replaceExecutable(options.BinaryPath, options.StagedPath, options.BackupPath); err != nil {
		writeReplacementResult(options.StatusPath, replacementResult(options, "failed", err))
		if selfManagedMode(options.Mode) {
			if _, startErr := startReplacement(options.BinaryPath, options.RestartArgs); startErr != nil {
				err = errors.Join(err, fmt.Errorf("restart previous executable: %w", startErr))
				writeReplacementResult(options.StatusPath, replacementResult(options, "failed", err))
			}
		}
		return err
	}
	if options.AfterReplace != nil {
		options.AfterReplace()
	}
	writeReplacementResult(options.StatusPath, replacementResult(options, ReplacementStateWaiting, nil))
	if selfManagedMode(options.Mode) {
		pid, err := startReplacement(options.BinaryPath, options.RestartArgs)
		if err != nil {
			if restoreErr := restoreExecutable(options.BinaryPath, options.BackupPath); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore previous executable: %w", restoreErr))
			}
			writeReplacementResult(options.StatusPath, replacementResult(options, "rolled_back", err))
			if oldPID, startErr := startReplacement(options.BinaryPath, options.RestartArgs); startErr == nil {
				writePIDFile(options.PIDFile, oldPID)
			} else {
				err = errors.Join(err, fmt.Errorf("restart previous executable: %w", startErr))
				writeReplacementResult(options.StatusPath, replacementResult(options, "rolled_back", err))
			}
			return err
		}
		writePIDFile(options.PIDFile, pid)
	}
	healthErr := waitForReplacementHealth(ctx, options)
	if healthErr != nil {
		replacementPID := readPIDFile(options.PIDFile)
		if replacementPID > 0 && replacementPID != os.Getpid() && replacementPID != options.ParentPID {
			if stopErr := StopProcess(context.Background(), replacementPID); stopErr != nil {
				healthErr = errors.Join(healthErr, fmt.Errorf("stop unhealthy replacement process: %w", stopErr))
			}
		}
		if restoreErr := restoreExecutable(options.BinaryPath, options.BackupPath); restoreErr != nil {
			healthErr = errors.Join(healthErr, fmt.Errorf("restore previous executable: %w", restoreErr))
		}
		writeReplacementResult(options.StatusPath, replacementResult(options, "rolled_back", healthErr))
		if selfManagedMode(options.Mode) {
			if pid, startErr := startReplacement(options.BinaryPath, options.RestartArgs); startErr == nil {
				writePIDFile(options.PIDFile, pid)
			}
		}
		return healthErr
	}
	result := replacementResult(options, options.TargetState, nil)
	writeReplacementResult(options.StatusPath, result)
	CleanupReplacementArtifacts(result)
	return nil
}

func replacementResult(options ReplacementOptions, state string, err error) ReplacementResult {
	result := ReplacementResult{
		SchemaVersion: ReplacementResultSchemaVersion,
		ActionID:      options.ActionID,
		Version:       options.Target,
		State:         state,
		BinaryPath:    options.BinaryPath,
		StagedPath:    options.StagedPath,
		BackupPath:    options.BackupPath,
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func selfManagedMode(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), "wsl") || strings.EqualFold(strings.TrimSpace(mode), "foreground")
}

func waitForReplacementHealth(ctx context.Context, options ReplacementOptions) error {
	healthCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	if strings.TrimSpace(options.HealthURL) != "" {
		return WaitForHTTPSHealth(healthCtx, options.HealthURL)
	}
	if strings.TrimSpace(options.ReadyPath) != "" {
		return WaitForReadyState(healthCtx, options.ReadyPath, options.Target, options.TargetState)
	}
	return errors.New("update helper has no health check")
}

func startReplacement(binaryPath string, args []string) (int, error) {
	if len(args) == 0 {
		return 0, errors.New("update helper has no restart arguments")
	}
	command := exec.Command(binaryPath, args...)
	command.Dir = filepath.Dir(binaryPath)
	command.Stdin = nil
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return 0, err
	}
	return command.Process.Pid, nil
}

func writePIDFile(path string, pid int) {
	if strings.TrimSpace(path) == "" || pid <= 0 {
		return
	}
	_ = os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

func readPIDFile(path string) int {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func WritePIDFile(path string, pid int) {
	writePIDFile(path, pid)
}

func RemovePIDFileIfOwner(path string, pid int) {
	if pid <= 0 || readPIDFile(path) != pid {
		return
	}
	_ = os.Remove(path)
}

func StopProcess(ctx context.Context, pid int) error {
	if pid <= 0 || !ProcessAlive(pid) {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Kill(); err != nil && ProcessAlive(pid) {
		return err
	}
	return WaitForProcessExit(ctx, pid, 10*time.Second)
}

func replaceExecutable(binaryPath, stagedPath, backupPath string) error {
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o700); err != nil {
		return err
	}
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(binaryPath, backupPath); err != nil {
		if copyErr := copyFile(binaryPath, backupPath, 0o700); copyErr != nil {
			return fmt.Errorf("backup executable: %w", err)
		}
		if removeErr := os.Remove(binaryPath); removeErr != nil {
			return removeErr
		}
	}
	if err := os.Rename(stagedPath, binaryPath); err != nil {
		if copyErr := copyFile(stagedPath, binaryPath, 0o755); copyErr != nil {
			_ = restoreExecutable(binaryPath, backupPath)
			return fmt.Errorf("install staged executable: %w", err)
		}
		_ = os.Remove(stagedPath)
	}
	return os.Chmod(binaryPath, 0o755)
}

func restoreExecutable(binaryPath, backupPath string) error {
	if err := os.Remove(binaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(backupPath, binaryPath); err != nil {
		if copyErr := copyFile(backupPath, binaryPath, 0o755); copyErr != nil {
			return err
		}
		_ = os.Remove(backupPath)
	}
	return os.Chmod(binaryPath, 0o755)
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func WaitForHTTPSHealth(ctx context.Context, endpoint string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "/readyz" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("health endpoint must be a loopback HTTPS /readyz URL without credentials, query or fragment")
	}
	host := strings.Trim(parsed.Hostname(), "[]")
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("health endpoint must resolve to a loopback address")
		}
	}
	// This probe is intentionally limited to the local Controller/Node
	// listener. It does not validate a remote certificate, and it never follows
	// a redirect to turn the exception into a remote request.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}, Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }} // #nosec G402 -- loopback-only readiness probe; release bytes are independently signature and SHA256 verified.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check failed: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func WaitForReadyState(ctx context.Context, path, version, expectedState string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil {
			var result ReplacementResult
			if json.Unmarshal(data, &result) == nil && result.Version == version && result.State == expectedState {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check failed: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func writeReplacementResult(path string, result ReplacementResult) {
	if strings.TrimSpace(path) == "" {
		return
	}
	result.UpdatedAt = time.Now().UTC()
	if result.SchemaVersion == 0 {
		result.SchemaVersion = ReplacementResultSchemaVersion
	}
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".asterferry-update-*")
	if err != nil {
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return
	}
	if temporary.Close() != nil {
		return
	}
	_ = os.Chmod(temporaryPath, 0o600)
	_ = os.Rename(temporaryPath, path)
}

func WriteReplacementResult(path string, result ReplacementResult) {
	writeReplacementResult(path, result)
}

func ReadReplacementResult(path string) (ReplacementResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ReplacementResult{}, err
	}
	var result ReplacementResult
	if err := json.Unmarshal(data, &result); err != nil {
		return ReplacementResult{}, err
	}
	if result.SchemaVersion == 0 {
		result.SchemaVersion = ReplacementResultSchemaVersion
	}
	if result.SchemaVersion != ReplacementResultSchemaVersion {
		return ReplacementResult{}, fmt.Errorf("replacement result schema version %d is unsupported", result.SchemaVersion)
	}
	return result, nil
}

// ReplacementNeedsRecovery identifies journal states that can be left behind
// after the helper is killed between the rename and the new process becoming
// healthy.
func ReplacementNeedsRecovery(state string) bool {
	switch state {
	case ReplacementStatePrepared, ReplacementStateReplacing, ReplacementStateWaiting, ReplacementStateRecovering:
		return true
	default:
		return false
	}
}

// CleanupReplacementArtifacts removes only the staged and previous files
// recorded by the local replacement journal. It deliberately never removes
// BinaryPath, which may now be the live executable.
func CleanupReplacementArtifacts(result ReplacementResult) {
	for _, path := range []string{result.StagedPath, result.BackupPath} {
		if strings.TrimSpace(path) != "" {
			_ = os.Remove(path)
		}
	}
}

func ReplacementTimeoutSeconds(value time.Duration) string {
	if value <= 0 {
		value = HealthTimeout
	}
	return strconv.FormatInt(int64(value/time.Second), 10)
}
