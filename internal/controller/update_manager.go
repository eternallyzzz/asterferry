package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"asterferry/internal/atomicfile"
	"asterferry/internal/buildinfo"
	"asterferry/internal/update"
)

const (
	updateCheckTimeout         = 30 * time.Second
	updateRestartResponseDelay = 250 * time.Millisecond
)

type UpdateManager struct {
	config            Config
	resources         *ResourceRepository
	client            update.Client
	mu                sync.Mutex
	applyMu           sync.Mutex
	latest            *update.Release
	restart           func(error)
	startUpdateHelper func(string, []string) error
}

var ErrControllerUpdateRestart = errors.New("Controller update requested process restart")

func NewUpdateManager(config Config, resources *ResourceRepository) *UpdateManager {
	return &UpdateManager{config: config, resources: resources, client: update.NewGitHubClient(update.DefaultRepository)}
}

func (m *UpdateManager) SetRestartCallback(callback func(error)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.restart = callback
	m.mu.Unlock()
}

func (m *UpdateManager) Start(ctx context.Context) {
	if m == nil || ctx == nil {
		return
	}
	go m.recoverPendingReplacement(ctx)
	if !m.config.UpdateCheckEnabled {
		return
	}
	go func() {
		m.checkWithTimeout(ctx)
		interval := time.Duration(m.config.UpdateCheckIntervalSeconds) * time.Second
		if interval <= 0 {
			interval = update.DefaultCheckEvery
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.checkWithTimeout(ctx)
			}
		}
	}()
}

func (m *UpdateManager) checkWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, updateCheckTimeout)
	defer cancel()
	if _, err := m.Check(ctx); err != nil && ctx.Err() == nil {
		slog.Default().Warn("stable release check failed", "error", err)
	}
}

func (m *UpdateManager) Status(ctx context.Context) (ControllerUpdateState, error) {
	if m == nil || m.resources == nil {
		return ControllerUpdateState{}, errors.New("update manager is not initialized")
	}
	state, err := m.resources.GetControllerUpdateState(ctx, m.config)
	if err != nil {
		return ControllerUpdateState{}, err
	}
	applyControllerRuntimeState(&state, m.config)
	if result, resultErr := update.ReadReplacementResult(m.controllerUpdateStatusPath()); resultErr == nil {
		switch result.State {
		case UpdateStateRolledBack, UpdateStateFailed:
			state.State = result.State
			state.TargetVersion = result.Version
			state.LastError = result.Error
		case UpdateStateManualRequired:
			state.State = UpdateStateManualRequired
			state.TargetVersion = result.Version
			state.LastError = result.Error
		case update.ReplacementStatePrepared, update.ReplacementStateReplacing, update.ReplacementStateWaiting, update.ReplacementStateRecovering:
			state.State = UpdateStateApplying
			state.TargetVersion = result.Version
			if state.LastError == "" {
				state.LastError = "Controller replacement journal is pending recovery"
			}
		case UpdateStateUpToDate:
			if buildinfo.Current().Version == result.Version {
				state.CurrentVersion = result.Version
				state.State = UpdateStateUpToDate
				state.TargetVersion = result.Version
				state.LastError = ""
				update.CleanupReplacementArtifacts(result)
			}
		}
	}
	return state, nil
}

// recoverPendingReplacement closes the helper-killed-after-rename window. A
// newly started target process proves that the rename completed; it then
// confirms the local readiness endpoint before marking the journal terminal.
// If readiness never returns, the live target is left untouched and the
// operator receives a manual-required state with the previous binary path
// retained for forensic/manual rollback.
func (m *UpdateManager) recoverPendingReplacement(parent context.Context) {
	result, err := update.ReadReplacementResult(m.controllerUpdateStatusPath())
	if err != nil {
		return
	}
	if result.State == UpdateStateUpToDate && buildinfo.Current().Version != result.Version {
		return
	}
	if result.State == UpdateStateUpToDate || result.State == UpdateStateRolledBack || result.State == UpdateStateFailed || result.State == UpdateStateManualRequired {
		if result.State == UpdateStateUpToDate && buildinfo.Current().Version == result.Version {
			update.CleanupReplacementArtifacts(result)
		}
		state, stateErr := m.resources.GetControllerUpdateState(context.Background(), m.config)
		if stateErr == nil {
			state.State = result.State
			state.TargetVersion = result.Version
			state.LastError = result.Error
			if result.State == UpdateStateUpToDate {
				state.CurrentVersion = buildinfo.Current().Version
			}
			_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		}
		return
	}
	if !update.ReplacementNeedsRecovery(result.State) {
		return
	}
	if strings.TrimSpace(result.Version) == "" || buildinfo.Current().Version != result.Version {
		message := "replacement journal target does not match the running Controller version"
		if strings.TrimSpace(result.Version) == "" {
			message = "replacement journal has no target Controller version"
		}
		result.State = UpdateStateManualRequired
		result.Error = message
		update.WriteReplacementResult(m.controllerUpdateStatusPath(), result)
		state, stateErr := m.resources.GetControllerUpdateState(context.Background(), m.config)
		if stateErr == nil {
			state.State = UpdateStateManualRequired
			state.TargetVersion = result.Version
			state.LastError = message
			_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		}
		return
	}
	result.State = update.ReplacementStateRecovering
	result.Error = ""
	update.WriteReplacementResult(m.controllerUpdateStatusPath(), result)
	healthURL, err := controllerHealthURL(m.config.HTTPListen)
	if err == nil {
		healthCtx, cancel := context.WithTimeout(parent, update.HealthTimeout)
		err = update.WaitForHTTPSHealth(healthCtx, healthURL)
		cancel()
	}
	state, stateErr := m.resources.GetControllerUpdateState(context.Background(), m.config)
	if err != nil {
		message := fmt.Sprintf("replacement recovery could not prove Controller readiness: %v", err)
		result.State = UpdateStateManualRequired
		result.Error = message
		update.WriteReplacementResult(m.controllerUpdateStatusPath(), result)
		if stateErr == nil {
			state.State = UpdateStateManualRequired
			state.TargetVersion = result.Version
			state.LastError = message
			_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		}
		return
	}
	result.State = UpdateStateUpToDate
	result.Error = ""
	update.WriteReplacementResult(m.controllerUpdateStatusPath(), result)
	update.CleanupReplacementArtifacts(result)
	if stateErr == nil {
		state.State = UpdateStateUpToDate
		state.CurrentVersion = buildinfo.Current().Version
		state.TargetVersion = result.Version
		state.LastError = ""
		_ = m.resources.SaveControllerUpdateState(context.Background(), state)
	}
}

func (m *UpdateManager) controllerUpdateStatusPath() string {
	if m == nil {
		return ""
	}
	return filepath.Join(filepath.Dir(m.config.MasterKeyPath), "controller-update.json")
}

func (m *UpdateManager) Check(ctx context.Context) (ControllerUpdateState, error) {
	if m == nil || m.resources == nil {
		return ControllerUpdateState{}, errors.New("update manager is not initialized")
	}
	state, err := m.Status(ctx)
	if err != nil {
		return ControllerUpdateState{}, err
	}
	if !m.config.UpdateCheckEnabled {
		state.State = UpdateStateDisabled
		return state, nil
	}
	state.State = UpdateStateChecking
	state.LastError = ""
	if err := m.resources.SaveControllerUpdateState(ctx, state); err != nil {
		return ControllerUpdateState{}, err
	}
	release, err := m.client.LatestStable(ctx)
	state.LastCheckedAt = time.Now().UTC()
	if err != nil {
		state.State = UpdateStateFailed
		state.LastError = err.Error()
		_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		return state, err
	}
	state.LatestVersion = release.Version
	state.LatestURL = release.URL
	state.PublishedAt = release.PublishedAt
	m.mu.Lock()
	m.latest = &release
	m.mu.Unlock()
	comparison, compareErr := update.CompareVersions(state.CurrentVersion, release.Version)
	if !state.Supported {
		state.State = UpdateStateUnsupported
	} else if compareErr != nil || state.CurrentVersion == "dev" || state.CurrentVersion == "unknown" {
		state.State = UpdateStateUnsupported
		if compareErr != nil {
			state.LastError = "current Controller version is not a published semantic release"
		}
	} else if comparison < 0 {
		state.State = UpdateStateAvailable
	} else {
		if comparison == 0 {
			if syncErr := m.syncNodeInstallerAssets(ctx, release); syncErr != nil {
				state.State = UpdateStateFailed
				state.LastError = fmt.Sprintf("Controller is current but Node installer assets could not be synchronized: %v", syncErr)
			} else {
				state.State = UpdateStateUpToDate
			}
		} else {
			state.State = UpdateStateUpToDate
		}
	}
	if err := m.resources.SaveControllerUpdateState(ctx, state); err != nil {
		return ControllerUpdateState{}, err
	}
	return state, nil
}

func (m *UpdateManager) LatestRelease(ctx context.Context) (update.Release, error) {
	m.mu.Lock()
	if m.latest != nil {
		release := *m.latest
		m.mu.Unlock()
		return release, nil
	}
	m.mu.Unlock()
	state, err := m.Check(ctx)
	if err != nil {
		return update.Release{}, err
	}
	if state.LatestVersion == "" {
		return update.Release{}, errors.New("no stable release is available")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.latest == nil {
		return update.Release{}, errors.New("stable release metadata was not retained")
	}
	return *m.latest, nil
}

func (m *UpdateManager) ControllerAsset(release update.Release) (update.Asset, string, error) {
	name := update.ArchiveName(release.Version, runtime.GOOS, runtime.GOARCH)
	asset, err := release.Asset(name)
	if err != nil {
		return update.Asset{}, "", err
	}
	checksum, err := release.Checksum(name)
	if err != nil {
		return update.Asset{}, "", err
	}
	return asset, checksum, nil
}

func (m *UpdateManager) ApplyController(ctx context.Context, targetVersion string) (ControllerUpdateState, error) {
	if m == nil || m.resources == nil {
		return ControllerUpdateState{}, errors.New("update manager is not initialized")
	}
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	state, err := m.Status(ctx)
	if err != nil {
		return ControllerUpdateState{}, err
	}
	if !state.Supported {
		state.State = UpdateStateUnsupported
		state.LastError = controllerUpdateReason(state)
		_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		return state, errors.New(state.LastError)
	}
	if state.State == UpdateStateApplying {
		return state, errors.New("Controller update is already in progress")
	}
	if strings.TrimSpace(state.CurrentVersion) == "dev" || strings.TrimSpace(state.CurrentVersion) == "unknown" {
		state.State = UpdateStateManualRequired
		state.LastError = "a development Controller build cannot be upgraded by release automation"
		_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		return state, errors.New(state.LastError)
	}
	release, err := m.LatestRelease(ctx)
	if err != nil {
		state.State = UpdateStateFailed
		state.LastError = err.Error()
		_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		return state, err
	}
	if !release.ManifestVerified || strings.TrimSpace(release.ManifestURL) == "" || strings.TrimSpace(release.ManifestSignatureURL) == "" {
		return m.controllerUpdateFailure(state, errors.New("stable release metadata is missing a verified signed manifest"))
	}
	if requested := strings.TrimPrefix(strings.TrimSpace(targetVersion), "v"); requested != "" && requested != release.Version {
		return state, fmt.Errorf("requested Controller version %s is not the latest stable release %s", requested, release.Version)
	}
	comparison, err := update.CompareVersions(state.CurrentVersion, release.Version)
	if err != nil {
		state.State = UpdateStateManualRequired
		state.LastError = err.Error()
		_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		return state, err
	}
	if comparison >= 0 {
		state.State = UpdateStateUpToDate
		state.LatestVersion = release.Version
		state.LastError = ""
		_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		return state, nil
	}
	asset, checksum, err := m.ControllerAsset(release)
	if err != nil {
		state.State = UpdateStateFailed
		state.LastError = err.Error()
		_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		return state, err
	}
	activeBinary, err := os.Executable()
	if err != nil {
		return m.controllerUpdateFailure(state, fmt.Errorf("locate active Controller executable: %w", err))
	}
	activeBinary, err = filepath.Abs(activeBinary)
	if err != nil {
		return m.controllerUpdateFailure(state, err)
	}
	if err := ensureWritableDirectory(filepath.Dir(activeBinary)); err != nil {
		state.State = UpdateStateManualRequired
		state.LastError = fmt.Sprintf("active Controller binary is not service-writable; run the new Controller installer once: %v", err)
		_ = m.resources.SaveControllerUpdateState(context.Background(), state)
		return state, errors.New(state.LastError)
	}
	workDir := filepath.Join(filepath.Dir(m.config.MasterKeyPath), "updates")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return m.controllerUpdateFailure(state, err)
	}
	archivePath := filepath.Join(workDir, asset.Name)
	downloadCtx, cancel := context.WithTimeout(ctx, update.DownloadTimeout)
	err = update.Download(downloadCtx, m.client.HTTPClient, asset, archivePath, checksum)
	cancel()
	if err != nil {
		return m.controllerUpdateFailure(state, err)
	}
	binaryName := filepath.Base(activeBinary)
	stagedPath := filepath.Join(workDir, ".asterferry-"+release.Version+"-staged-"+strconv.FormatInt(time.Now().UnixNano(), 10)+"-"+binaryName)
	if err := update.ExtractBinary(archivePath, stagedPath, binaryName); err != nil {
		return m.controllerUpdateFailure(state, err)
	}
	backupPath := filepath.Join(workDir, ".asterferry-"+release.Version+"-previous-"+strconv.FormatInt(time.Now().UnixNano(), 10)+"-"+binaryName)
	statusPath := m.controllerUpdateStatusPath()
	pidPath := filepath.Join(filepath.Dir(m.config.MasterKeyPath), "controller.pid")
	healthURL, err := controllerHealthURL(m.config.HTTPListen)
	if err != nil {
		return m.controllerUpdateFailure(state, err)
	}
	m.mu.Lock()
	restart := m.restart
	m.mu.Unlock()
	if restart == nil {
		return m.controllerUpdateFailure(state, errors.New("Controller restart callback is not configured"))
	}
	if len(m.config.ProcessArgs) == 0 {
		return m.controllerUpdateFailure(state, errors.New("Controller restart arguments are not configured; restart the service with the new installer"))
	}
	state.State = UpdateStateApplying
	state.TargetVersion = release.Version
	state.PreviousVersion = state.CurrentVersion
	state.LastError = ""
	state.LatestVersion = release.Version
	if err := m.resources.SaveControllerUpdateState(ctx, state); err != nil {
		return state, err
	}
	helperArgs := []string{"controller", "update-helper", "--pid", strconv.Itoa(os.Getpid()), "--binary", activeBinary, "--staged", stagedPath, "--backup", backupPath, "--health-url", healthURL, "--target", release.Version, "--mode", m.config.ServiceMode, "--service-name", m.config.ServiceName, "--status-path", statusPath, "--pid-file", pidPath, "--timeout", update.ReplacementTimeoutSeconds(update.HealthTimeout)}
	for _, argument := range m.config.ProcessArgs {
		helperArgs = append(helperArgs, "--restart-arg", argument)
	}
	startHelper := m.startUpdateHelper
	if startHelper == nil {
		startHelper = func(binaryPath string, args []string) error {
			command := exec.Command(binaryPath, args...)
			command.Stdout = os.Stdout
			command.Stderr = os.Stderr
			return command.Start()
		}
	}
	if err := startHelper(activeBinary, helperArgs); err != nil {
		return m.controllerUpdateFailure(state, fmt.Errorf("start Controller update helper: %w", err))
	}
	// Let the HTTP handler flush its 202 response before closing the listener.
	// The helper is already waiting for this process to exit, so the short
	// delay does not change the replacement ordering.
	time.AfterFunc(updateRestartResponseDelay, func() { restart(ErrControllerUpdateRestart) })
	return state, nil
}

func (m *UpdateManager) controllerUpdateFailure(state ControllerUpdateState, err error) (ControllerUpdateState, error) {
	state.State = UpdateStateFailed
	state.LastError = err.Error()
	_ = m.resources.SaveControllerUpdateState(context.Background(), state)
	return state, err
}

func ensureWritableDirectory(path string) error {
	temporary, err := os.CreateTemp(path, ".asterferry-write-check-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func controllerHealthURL(listen string) (string, error) {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "", errors.New("Controller HTTPS listen address is empty")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("parse Controller HTTPS listen address: %w", err)
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "0.0.0.0" || host == "::" || host == "::0" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", errors.New("Controller HTTPS health probe requires a loopback listen address")
	}
	return (&url.URL{Scheme: "https", Host: net.JoinHostPort(host, port), Path: "/readyz"}).String(), nil
}

func (m *UpdateManager) NodeAsset(release update.Release, platform, architecture string) (update.Asset, string, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	architecture = strings.ToLower(strings.TrimSpace(architecture))
	name := update.ArchiveName(release.Version, platform, architecture)
	asset, err := release.Asset(name)
	if err != nil {
		return update.Asset{}, "", err
	}
	checksum, err := release.Checksum(name)
	if err != nil {
		return update.Asset{}, "", err
	}
	return asset, checksum, nil
}

func (m *UpdateManager) syncNodeInstallerAssets(ctx context.Context, release update.Release) error {
	if m == nil || strings.TrimSpace(m.config.NodeInstallersDir) == "" {
		return nil
	}
	if metadata, err := loadNodeReleaseMetadata(m.config); err == nil && metadata.Version == release.Version {
		allPresent := true
		for _, name := range []string{bootstrapInstallerUnix, bootstrapInstallerWindows, nodeReleaseMetadataName} {
			if info, statErr := os.Stat(nodeInstallerPath(m.config, name)); statErr != nil || info.IsDir() {
				allPresent = false
				break
			}
		}
		if allPresent {
			return nil
		}
	}
	if err := os.MkdirAll(m.config.NodeInstallersDir, 0o750); err != nil {
		return err
	}
	workDir := filepath.Join(filepath.Dir(m.config.MasterKeyPath), "updates", "node-installers")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return err
	}
	for _, name := range []string{bootstrapInstallerUnix, bootstrapInstallerWindows, nodeReleaseMetadataName} {
		asset, err := release.Asset(name)
		if err != nil {
			return err
		}
		checksum, err := release.Checksum(name)
		if err != nil {
			return err
		}
		staged := filepath.Join(workDir, "."+name+"-"+release.Version+".download")
		downloadCtx, cancel := context.WithTimeout(ctx, update.DownloadTimeout)
		err = update.Download(downloadCtx, m.client.HTTPClient, asset, staged, checksum)
		cancel()
		if err != nil {
			return fmt.Errorf("download %s: %w", name, err)
		}
		data, err := os.ReadFile(staged)
		_ = os.Remove(staged)
		if err != nil {
			return err
		}
		if err := atomicfile.AtomicWrite(nodeInstallerPath(m.config, name), data, 0o640); err != nil {
			return fmt.Errorf("publish %s: %w", name, err)
		}
	}
	return nil
}

func applyControllerRuntimeState(state *ControllerUpdateState, config Config) {
	if state == nil {
		return
	}
	info := buildinfo.Current()
	state.CurrentVersion = info.Version
	state.Channel = "stable"
	state.Deployment = strings.ToLower(strings.TrimSpace(config.ServiceMode))
	if state.Deployment == "" {
		state.Deployment = "foreground"
	}
	state.Supported = controllerSelfUpdateSupported(state.Deployment)
	if !state.Supported && state.State != UpdateStateDisabled && state.State != UpdateStateFailed && state.State != UpdateStateApplying {
		state.State = UpdateStateUnsupported
	}
}

func controllerUpdateReason(state ControllerUpdateState) string {
	if state.Supported {
		return ""
	}
	return fmt.Sprintf("Controller self-upgrade is unsupported for deployment mode %q; update the image or launch it with a supported service mode", state.Deployment)
}
