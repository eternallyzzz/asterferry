package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"asterferry/internal/update"
)

type NodeUpdateStatus struct {
	SchemaVersion  int       `json:"schema_version"`
	NodeID         string    `json:"node_id"`
	ActionID       string    `json:"action_id,omitempty"`
	CurrentVersion string    `json:"current_version,omitempty"`
	TargetVersion  string    `json:"target_version,omitempty"`
	LatestVersion  string    `json:"latest_version,omitempty"`
	LatestURL      string    `json:"latest_url,omitempty"`
	State          string    `json:"state"`
	Supported      bool      `json:"supported"`
	Deployment     string    `json:"deployment,omitempty"`
	Platform       string    `json:"platform,omitempty"`
	Architecture   string    `json:"architecture,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (s *Server) controllerUpdate(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/controller/update")
	switch {
	case path == "" || path == "/":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		state, err := s.update.Status(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"reason": controllerUpdateReason(state), "status": state})
	case path == "/check" && r.Method == http.MethodPost:
		user, ok := s.authorize(w, r, RoleAdmin)
		if !ok {
			return
		}
		state, err := s.update.Check(r.Context())
		if err != nil {
			_ = s.resources.RecordEvent(context.Background(), user.Username, "", "controller_update_check_failed", err.Error(), "controller", nil)
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"reason": controllerUpdateReason(state), "status": state})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"reason": controllerUpdateReason(state), "status": state})
	case path == "/apply" && r.Method == http.MethodPost:
		user, ok := s.authorize(w, r, RoleAdmin)
		if !ok {
			return
		}
		var input struct {
			Version string `json:"version,omitempty"`
		}
		if r.ContentLength != 0 {
			if err := decodeJSON(r, &input, 16<<10); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
		}
		state, err := s.update.ApplyController(r.Context(), input.Version)
		if err != nil {
			status := http.StatusConflict
			if state.State == UpdateStateFailed {
				status = http.StatusServiceUnavailable
			}
			_ = s.resources.RecordEvent(context.Background(), user.Username, "", "controller_update_apply_failed", err.Error(), "controller", nil)
			writeJSON(w, status, map[string]any{"reason": controllerUpdateReason(state), "status": state, "error": map[string]string{"code": "controller_update_failed", "message": err.Error()}})
			return
		}
		_ = s.resources.RecordEvent(context.Background(), user.Username, "", "controller_update_apply_requested", fmt.Sprintf("target=%s", state.TargetVersion), "controller", map[string]string{"target_version": state.TargetVersion})
		writeJSON(w, http.StatusAccepted, map[string]any{"reason": controllerUpdateReason(state), "status": state})
	default:
		writeError(w, http.StatusNotFound, "not_found", "Controller update resource was not found")
	}
}

func (s *Server) nodeUpdateStatus(ctx context.Context, nodeID string) (NodeUpdateStatus, error) {
	state, err := s.resources.GetNodeUpdateState(ctx, nodeID)
	if err != nil {
		return NodeUpdateStatus{}, err
	}
	status := NodeUpdateStatus{
		SchemaVersion: state.SchemaVersion, NodeID: nodeID, ActionID: state.ActionID, CurrentVersion: state.CurrentVersion,
		TargetVersion: state.TargetVersion, State: state.State, Supported: state.Supported, Deployment: state.Deployment,
		Reason: state.Reason, LastError: state.LastError, UpdatedAt: state.UpdatedAt,
	}
	observed, observedErr := s.resources.GetObserved(ctx, nodeID)
	if observedErr == nil && observed.SystemInfo != nil {
		info := observed.SystemInfo
		status.CurrentVersion = strings.TrimSpace(info.NodeVersion)
		status.Deployment = strings.TrimSpace(info.ServiceMode)
		status.Platform = strings.ToLower(strings.TrimSpace(info.OS))
		status.Architecture = strings.ToLower(strings.TrimSpace(info.Architecture))
	}
	supported, reason := nodeSupportsSelfUpdate(status.Deployment, s.changes.NodeCapabilities(nodeID))
	if supported && status.Platform != "linux" && status.Platform != "windows" {
		supported = false
		reason = "self-upgrade is supported only for native Linux or Windows Nodes"
	}
	if supported && status.Architecture != "amd64" && !(status.Platform == "linux" && status.Architecture == "arm64") {
		supported = false
		reason = "the published Node release does not support this platform architecture"
	}
	status.Supported = supported
	status.Reason = reason
	if !supported {
		status.State = UpdateStateUnsupported
	} else if status.State == UpdateStateUnsupported {
		status.State = UpdateStateUpToDate
	}
	if latest, latestErr := s.update.Status(ctx); latestErr == nil {
		status.LatestVersion = latest.LatestVersion
		status.LatestURL = latest.LatestURL
	}
	return status, nil
}

func (s *Server) nodeUpdate(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if _, ok := s.authorize(w, r, RoleViewer); !ok {
		return
	}
	status, err := s.nodeUpdateStatus(r.Context(), nodeID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) requestNodeUpgrade(w http.ResponseWriter, r *http.Request, nodeID string, user User) {
	s.nodeUpgradeMu.Lock()
	defer s.nodeUpgradeMu.Unlock()
	status, err := s.nodeUpdateStatus(r.Context(), nodeID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if active, activeOK, activeErr := s.resources.GetApplyingNodeUpdate(r.Context()); activeErr != nil {
		writeStoreError(w, activeErr)
		return
	} else if activeOK && active.NodeID != nodeID {
		status.State = UpdateStateApplying
		status.Reason = fmt.Sprintf("Node %s is currently upgrading; confirm another Node after it finishes", active.NodeID)
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{"code": "node_update_in_progress", "message": status.Reason}, "status": status})
		return
	}
	if !status.Supported {
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{"code": "node_update_unsupported", "message": status.Reason}, "status": status})
		return
	}
	if status.State == UpdateStateApplying {
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{"code": "node_update_in_progress", "message": "a Node upgrade is already in progress"}, "status": status})
		return
	}
	if status.CurrentVersion == "" || status.Platform == "" || status.Architecture == "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{"code": "node_update_manual_required", "message": "Node system information is not available; upgrade it once manually before using Controller-managed upgrades"}, "status": status})
		return
	}
	release, err := s.update.LatestRelease(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	comparison, err := update.CompareVersions(status.CurrentVersion, release.Version)
	if err != nil {
		status.State = UpdateStateManualRequired
		status.Reason = "Node version is not a published semantic release; run the installer once manually"
		status.LastError = err.Error()
		_ = s.resources.SaveNodeUpdateState(context.Background(), NodeUpdateState{NodeID: nodeID, CurrentVersion: status.CurrentVersion, State: status.State, Supported: false, Deployment: status.Deployment, Reason: status.Reason, LastError: status.LastError})
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{"code": "node_update_manual_required", "message": status.Reason}, "status": status})
		return
	}
	if comparison >= 0 {
		status.State = UpdateStateUpToDate
		status.LatestVersion = release.Version
		writeJSON(w, http.StatusOK, status)
		return
	}
	asset, checksum, err := s.update.NodeAsset(release, status.Platform, status.Architecture)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	payload, err := json.Marshal(map[string]any{
		"version": release.Version, "asset_name": asset.Name, "asset_url": asset.URL, "sha256": checksum,
		"release_manifest_url": release.ManifestURL, "release_manifest_signature_url": release.ManifestSignatureURL,
		"health_timeout_seconds": int(update.HealthTimeout / time.Second),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_update_request_failed", err.Error())
		return
	}
	actionID, delivered, err := s.resources.RequestNodeActionWithID(r.Context(), nodeID, "node_upgrade", string(payload), WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !delivered {
		status.State = UpdateStateManualRequired
		status.Reason = "Node is not connected; reconnect it and confirm the upgrade again"
		status.LastError = "upgrade action was durably audited but was not delivered to a live Node stream"
		status.UpdatedAt = time.Now().UTC()
		_, _, saveErr := s.resources.SaveNodeUpdateStateIfActive(r.Context(), NodeUpdateState{NodeID: nodeID, ActionID: actionID, CurrentVersion: status.CurrentVersion, TargetVersion: release.Version, State: status.State, Supported: true, Deployment: status.Deployment, Reason: status.Reason, LastError: status.LastError})
		if saveErr != nil {
			writeStoreError(w, saveErr)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{"code": "node_update_not_delivered", "message": status.Reason}, "status": status})
		return
	}
	nodeState := NodeUpdateState{NodeID: nodeID, ActionID: actionID, CurrentVersion: status.CurrentVersion, TargetVersion: release.Version, State: UpdateStateApplying, Supported: true, Deployment: status.Deployment}
	if saved, _, err := s.resources.SaveNodeUpdateStateIfActive(r.Context(), nodeState); err != nil {
		writeStoreError(w, err)
		return
	} else {
		// The Node may have downloaded, restarted and reported its terminal
		// state before the HTTP request got here. Preserve that result in the
		// response instead of reporting a stale applying state.
		if isTerminalNodeUpdateState(saved.State) && saved.ActionID == actionID {
			status = NodeUpdateStatus{SchemaVersion: saved.SchemaVersion, NodeID: nodeID, ActionID: saved.ActionID, CurrentVersion: saved.CurrentVersion, TargetVersion: saved.TargetVersion, State: saved.State, Supported: saved.Supported, Deployment: saved.Deployment, Reason: saved.Reason, LastError: saved.LastError, UpdatedAt: saved.UpdatedAt, LatestVersion: release.Version}
			writeJSON(w, http.StatusAccepted, map[string]any{"node_id": nodeID, "action": "upgrade", "requested_by": user.Username, "state": "delivered", "status": status})
			return
		}
	}
	status.ActionID = actionID
	status.TargetVersion = release.Version
	status.LatestVersion = release.Version
	status.State = UpdateStateApplying
	status.UpdatedAt = time.Now().UTC()
	responseState := "queued"
	if delivered {
		responseState = "delivered"
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"node_id": nodeID, "action": "upgrade", "requested_by": user.Username, "state": responseState, "status": status})
}
