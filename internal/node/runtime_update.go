package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"asterferry/internal/buildinfo"
	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/update"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	nodeUpdateStateApplying       = "applying"
	nodeUpdateStateUpToDate       = "up_to_date"
	nodeUpdateStateFailed         = "failed"
	nodeUpdateStateRolledBack     = "rolled_back"
	nodeUpdateStateManualRequired = "manual_required"
	nodeUpdateStateWaiting        = "waiting"
	nodeUpdateStateReplacing      = "replacing"
	nodeUpdateStateRecovering     = "recovering"
	nodeUpgradeCapability         = "node-upgrade-v1"
)

var ErrNodeUpdateRestart = errors.New("node update requested process restart")

type nodeUpgradeRequest struct {
	Version                     string `json:"version"`
	AssetName                   string `json:"asset_name"`
	AssetURL                    string `json:"asset_url"`
	SHA256                      string `json:"sha256"`
	ReleaseManifestURL          string `json:"release_manifest_url"`
	ReleaseManifestSignatureURL string `json:"release_manifest_signature_url"`
	HealthTimeoutSeconds        int    `json:"health_timeout_seconds,omitempty"`
}

func (r *Runtime) startNodeUpgrade(ctx context.Context, action *v1.Action, send func(*v1.NodeMessage) error) error {
	if action == nil {
		return errors.New("node upgrade action is empty")
	}
	var request nodeUpgradeRequest
	if len(action.GetPayloadJson()) == 0 || json.Unmarshal(action.GetPayloadJson(), &request) != nil {
		return r.nodeUpgradeFailure(send, action.GetId(), "upgrade payload is invalid")
	}
	request.Version = strings.TrimPrefix(strings.TrimSpace(request.Version), "v")
	if !update.IsStable(request.Version) || strings.TrimSpace(request.AssetName) == "" || strings.TrimSpace(request.AssetURL) == "" {
		return r.nodeUpgradeFailure(send, action.GetId(), "upgrade payload is not a stable verified release")
	}
	if strings.ToLower(runtime.GOOS) != "windows" && strings.ToLower(runtime.GOOS) != "linux" {
		return r.nodeUpgradeFailure(send, action.GetId(), "self-upgrade is unsupported on this operating system")
	}
	if !strings.EqualFold(strings.TrimSpace(r.runtimeOpts.ServiceMode), "windows-service") && !strings.EqualFold(strings.TrimSpace(r.runtimeOpts.ServiceMode), "systemd") && !strings.EqualFold(strings.TrimSpace(r.runtimeOpts.ServiceMode), "wsl") {
		return r.nodeUpgradeFailure(send, action.GetId(), "self-upgrade requires a Windows service, systemd or WSL installation")
	}
	if !strings.EqualFold(strings.TrimSpace(r.runtimeOpts.ServiceMode), "foreground") && len(r.runtimeOpts.ProcessArgs) == 0 {
		return r.nodeUpgradeFailure(send, action.GetId(), "restart arguments are unavailable; run the current installer once")
	}
	current := buildinfo.Current().Version
	if current == "dev" || current == "unknown" {
		return r.nodeUpgradeFailure(send, action.GetId(), "development Node builds require one manual installer upgrade")
	}
	comparison, err := update.CompareVersions(current, request.Version)
	if err != nil || comparison >= 0 {
		if err != nil {
			return r.nodeUpgradeFailure(send, action.GetId(), "current Node version is not a published semantic release")
		}
		return r.nodeUpgradeFailure(send, action.GetId(), "target Node version is not newer than the running version")
	}
	activeBinary, err := os.Executable()
	if err != nil {
		return r.nodeUpgradeFailure(send, action.GetId(), fmt.Sprintf("locate active Node executable: %v", err))
	}
	activeBinary, err = filepath.Abs(activeBinary)
	if err != nil {
		return r.nodeUpgradeFailure(send, action.GetId(), err.Error())
	}
	if err := ensureNodeWritableDirectory(filepath.Dir(activeBinary)); err != nil {
		return r.nodeUpgradeFailure(send, action.GetId(), fmt.Sprintf("active Node binary is not service-writable; run the new installer once: %v", err))
	}
	client := r.updateHTTPClient
	if client == nil {
		client = &http.Client{Timeout: update.DownloadTimeout}
	}
	if strings.TrimSpace(request.ReleaseManifestURL) == "" || strings.TrimSpace(request.ReleaseManifestSignatureURL) == "" {
		return r.nodeUpgradeFailure(send, action.GetId(), "signed release manifest is required for Node self-upgrade")
	}
	manifestCtx, manifestCancel := context.WithTimeout(ctx, update.DownloadTimeout)
	manifestClient := update.Client{HTTPClient: client, UserAgent: "asterferry-node-updater", ManifestVerifier: r.updateManifestVerifier}
	_, err = manifestClient.VerifyReleaseManifest(manifestCtx, request.ReleaseManifestURL, request.ReleaseManifestSignatureURL, request.Version, request.AssetName, request.SHA256)
	manifestCancel()
	if err != nil {
		return r.nodeUpgradeFailure(send, action.GetId(), fmt.Sprintf("verify signed Node release manifest: %v", err))
	}
	if err := sendNodeUpgradeEvent(send, action.GetId(), nodeUpdateStateApplying, request.Version, current, r.runtimeOpts.ServiceMode, ""); err != nil {
		return err
	}
	statusPath := strings.TrimSpace(r.runtimeOpts.UpdateStatusPath)
	if statusPath == "" {
		statusPath = filepath.Join(filepath.Dir(r.runtimeOpts.CachePath), "node-update.json")
	}
	workDir := filepath.Join(filepath.Dir(statusPath), "updates")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return r.nodeUpgradeFailure(send, action.GetId(), err.Error())
	}
	archivePath := filepath.Join(workDir, request.AssetName)
	downloadCtx, cancel := context.WithTimeout(ctx, update.DownloadTimeout)
	err = update.Download(downloadCtx, client, update.Asset{Name: request.AssetName, URL: request.AssetURL}, archivePath, request.SHA256)
	cancel()
	if err != nil {
		return r.nodeUpgradeFailure(send, action.GetId(), fmt.Sprintf("download and verify Node release: %v", err))
	}
	binaryName := filepath.Base(activeBinary)
	stagedPath := filepath.Join(workDir, ".asterferry-"+request.Version+"-staged-"+strconv.FormatInt(time.Now().UnixNano(), 10)+"-"+binaryName)
	if err := update.ExtractBinary(archivePath, stagedPath, binaryName); err != nil {
		return r.nodeUpgradeFailure(send, action.GetId(), err.Error())
	}
	backupPath := filepath.Join(workDir, ".asterferry-"+request.Version+"-previous-"+strconv.FormatInt(time.Now().UnixNano(), 10)+"-"+binaryName)
	timeout := update.HealthTimeout
	if request.HealthTimeoutSeconds > 0 {
		timeout = time.Duration(request.HealthTimeoutSeconds) * time.Second
		if timeout > 10*time.Minute {
			timeout = 10 * time.Minute
		}
	}
	pidPath := filepath.Join(filepath.Dir(statusPath), "node.pid")
	helperArgs := []string{"node", "update-helper", "--pid", strconv.Itoa(os.Getpid()), "--binary", activeBinary, "--staged", stagedPath, "--backup", backupPath, "--ready-path", statusPath, "--target", request.Version, "--action-id", action.GetId(), "--mode", r.runtimeOpts.ServiceMode, "--service-name", r.runtimeOpts.ServiceName, "--status-path", statusPath, "--pid-file", pidPath, "--timeout", update.ReplacementTimeoutSeconds(timeout)}
	for _, argument := range r.runtimeOpts.ProcessArgs {
		helperArgs = append(helperArgs, "--restart-arg", argument)
	}
	startHelper := r.startUpdateHelper
	if startHelper == nil {
		startHelper = startNodeUpdateHelper
	}
	if _, err := startHelper(activeBinary, helperArgs); err != nil {
		return r.nodeUpgradeFailure(send, action.GetId(), fmt.Sprintf("start Node update helper: %v", err))
	}
	return ErrNodeUpdateRestart
}

func startNodeUpdateHelper(binaryPath string, args []string) (int, error) {
	command := exec.Command(binaryPath, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return 0, err
	}
	return command.Process.Pid, nil
}

func (r *Runtime) nodeUpgradeFailure(send func(*v1.NodeMessage) error, actionID, message string) error {
	if err := sendNodeUpgradeEvent(send, actionID, nodeUpdateStateFailed, "", buildinfo.Current().Version, r.runtimeOpts.ServiceMode, message); err != nil {
		return err
	}
	return nil
}

func sendNodeUpgradeEvent(send func(*v1.NodeMessage) error, actionID, state, version, currentVersion, deployment, failure string) error {
	attributes := map[string]string{"state": state, "current_version": currentVersion, "deployment": deployment}
	if actionID != "" {
		attributes["action_id"] = actionID
	}
	if version != "" {
		attributes["version"] = version
	}
	if failure != "" {
		attributes["error"] = failure
	}
	data, err := json.Marshal(attributes)
	if err != nil {
		return err
	}
	return send(&v1.NodeMessage{Body: &v1.NodeMessage_EventBatch{EventBatch: &v1.EventBatch{Events: []*v1.Event{{
		Id: runtimeConnectionID(), Type: "node_upgrade", Message: state, AttributesJson: data, CreatedAt: timestamppb.New(time.Now().UTC()),
	}}}}})
}

func ensureNodeWritableDirectory(path string) error {
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

func (r *Runtime) sendPendingNodeUpdate(send func(*v1.NodeMessage) error) error {
	r.updateReportMu.Lock()
	defer r.updateReportMu.Unlock()
	if r.updateReportSent {
		return nil
	}
	path := strings.TrimSpace(r.runtimeOpts.UpdateStatusPath)
	if path == "" {
		return nil
	}
	result, err := update.ReadReplacementResult(path)
	if err != nil || result.State == "" {
		return nil
	}
	current := buildinfo.Current().Version
	if result.State == update.ReplacementStatePrepared || result.State == nodeUpdateStateWaiting || result.State == nodeUpdateStateReplacing || result.State == nodeUpdateStateRecovering {
		if result.Version == "" || result.Version != current {
			return nil
		}
		result.State = "healthy"
		result.Error = ""
		update.WriteReplacementResult(path, result)
	}
	state := result.State
	if state == "healthy" {
		state = nodeUpdateStateUpToDate
	}
	if state != nodeUpdateStateUpToDate && state != nodeUpdateStateRolledBack && state != nodeUpdateStateFailed && state != nodeUpdateStateManualRequired {
		return nil
	}
	if err := sendNodeUpgradeEvent(send, result.ActionID, state, result.Version, current, r.runtimeOpts.ServiceMode, result.Error); err != nil {
		return err
	}
	update.CleanupReplacementArtifacts(result)
	r.updateReportSent = true
	return nil
}
