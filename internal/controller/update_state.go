package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"asterferry/internal/buildinfo"
	"asterferry/internal/domain"
)

const (
	controllerUpdateStateKey = "controller_update_state"
	nodeUpdateStatePrefix    = "node_update:"
	updateStateSchemaVersion = 1
)

const (
	UpdateStateDisabled       = "disabled"
	UpdateStateChecking       = "checking"
	UpdateStateUpToDate       = "up_to_date"
	UpdateStateAvailable      = "available"
	UpdateStateApplying       = "applying"
	UpdateStateFailed         = "failed"
	UpdateStateRolledBack     = "rolled_back"
	UpdateStateManualRequired = "manual_required"
	UpdateStateUnsupported    = "unsupported"
)

type ControllerUpdateState struct {
	SchemaVersion   int       `json:"schema_version"`
	CurrentVersion  string    `json:"current_version"`
	Channel         string    `json:"channel"`
	Deployment      string    `json:"deployment"`
	Supported       bool      `json:"supported"`
	State           string    `json:"state"`
	LatestVersion   string    `json:"latest_version,omitempty"`
	LatestURL       string    `json:"latest_url,omitempty"`
	PublishedAt     time.Time `json:"published_at,omitempty"`
	LastCheckedAt   time.Time `json:"last_checked_at,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	TargetVersion   string    `json:"target_version,omitempty"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type NodeUpdateState struct {
	SchemaVersion  int       `json:"schema_version"`
	NodeID         string    `json:"node_id"`
	ActionID       string    `json:"action_id,omitempty"`
	CurrentVersion string    `json:"current_version,omitempty"`
	TargetVersion  string    `json:"target_version,omitempty"`
	State          string    `json:"state"`
	Supported      bool      `json:"supported"`
	Deployment     string    `json:"deployment,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func defaultControllerUpdateState(config Config) ControllerUpdateState {
	info := buildinfo.Current()
	deployment := strings.TrimSpace(config.ServiceMode)
	if deployment == "" {
		deployment = "foreground"
	}
	supported := controllerSelfUpdateSupported(deployment)
	state := UpdateStateUnsupported
	if !config.UpdateCheckEnabled {
		state = UpdateStateDisabled
	} else if supported {
		state = UpdateStateChecking
	}
	return ControllerUpdateState{
		SchemaVersion:  updateStateSchemaVersion,
		CurrentVersion: info.Version,
		Channel:        "stable",
		Deployment:     deployment,
		Supported:      supported,
		State:          state,
		UpdatedAt:      time.Now().UTC(),
	}
}

func controllerSelfUpdateSupported(deployment string) bool {
	deployment = strings.ToLower(strings.TrimSpace(deployment))
	return deployment == "windows-service" || deployment == "systemd" || deployment == "wsl"
}

func (s *ResourceRepository) loadUpdateSetting(ctx context.Context, key string, destination any) error {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM runtime_settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(value), destination); err != nil {
		return fmt.Errorf("decode update state %s: %w", key, err)
	}
	return nil
}

func (s *ResourceRepository) saveUpdateSetting(ctx context.Context, key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > 1<<20 {
		return errors.New("update state is too large")
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, string(encoded), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return s.commitWriteTx(ctx, tx)
}

func (s *ResourceRepository) GetControllerUpdateState(ctx context.Context, config Config) (ControllerUpdateState, error) {
	state := defaultControllerUpdateState(config)
	if err := s.loadUpdateSetting(ctx, controllerUpdateStateKey, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state, nil
		}
		return ControllerUpdateState{}, err
	}
	if state.SchemaVersion != updateStateSchemaVersion {
		return ControllerUpdateState{}, fmt.Errorf("controller update state schema version %d is unsupported", state.SchemaVersion)
	}
	return state, nil
}

func (s *ResourceRepository) SaveControllerUpdateState(ctx context.Context, state ControllerUpdateState) error {
	state.SchemaVersion = updateStateSchemaVersion
	state.UpdatedAt = time.Now().UTC()
	return s.saveUpdateSetting(ctx, controllerUpdateStateKey, state)
}

func nodeUpdateStateKey(nodeID string) string {
	return nodeUpdateStatePrefix + strings.TrimSpace(nodeID)
}

func (s *ResourceRepository) GetNodeUpdateState(ctx context.Context, nodeID string) (NodeUpdateState, error) {
	state := NodeUpdateState{SchemaVersion: updateStateSchemaVersion, NodeID: nodeID, State: UpdateStateUnsupported}
	if err := s.loadUpdateSetting(ctx, nodeUpdateStateKey(nodeID), &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state, nil
		}
		return NodeUpdateState{}, err
	}
	if state.SchemaVersion != updateStateSchemaVersion {
		return NodeUpdateState{}, fmt.Errorf("node update state schema version %d is unsupported", state.SchemaVersion)
	}
	return state, nil
}

func (s *ResourceRepository) GetApplyingNodeUpdate(ctx context.Context) (NodeUpdateState, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT value FROM runtime_settings WHERE key LIKE ?`, nodeUpdateStatePrefix+"%")
	if err != nil {
		return NodeUpdateState{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return NodeUpdateState{}, false, err
		}
		var state NodeUpdateState
		if err := json.Unmarshal([]byte(encoded), &state); err != nil {
			return NodeUpdateState{}, false, fmt.Errorf("decode node update state: %w", err)
		}
		if state.SchemaVersion != updateStateSchemaVersion {
			return NodeUpdateState{}, false, fmt.Errorf("node update state schema version %d is unsupported", state.SchemaVersion)
		}
		if state.State == UpdateStateApplying {
			return state, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return NodeUpdateState{}, false, err
	}
	return NodeUpdateState{}, false, nil
}

func (s *ResourceRepository) SaveNodeUpdateState(ctx context.Context, state NodeUpdateState) error {
	if err := domain.ValidateID(state.NodeID, "node_id"); err != nil {
		return err
	}
	state.SchemaVersion = updateStateSchemaVersion
	state.UpdatedAt = time.Now().UTC()
	return s.saveUpdateSetting(ctx, nodeUpdateStateKey(state.NodeID), state)
}

// SaveNodeUpdateStateIfActive records the request-side applying state without
// racing a terminal result emitted by the Node immediately after the action
// is published. The write transaction serializes this compare-and-set for
// SQLite and PostgreSQL alike.
func (s *ResourceRepository) SaveNodeUpdateStateIfActive(ctx context.Context, state NodeUpdateState) (NodeUpdateState, bool, error) {
	if err := domain.ValidateID(state.NodeID, "node_id"); err != nil {
		return NodeUpdateState{}, false, err
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return NodeUpdateState{}, false, err
	}
	defer tx.Rollback()

	current := NodeUpdateState{SchemaVersion: updateStateSchemaVersion, NodeID: state.NodeID, State: UpdateStateUnsupported}
	var encodedCurrent string
	err = tx.QueryRowContext(ctx, `SELECT value FROM runtime_settings WHERE key=?`, nodeUpdateStateKey(state.NodeID)).Scan(&encodedCurrent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return NodeUpdateState{}, false, err
	}
	if err == nil {
		if decodeErr := json.Unmarshal([]byte(encodedCurrent), &current); decodeErr != nil {
			return NodeUpdateState{}, false, fmt.Errorf("decode update state %s: %w", nodeUpdateStateKey(state.NodeID), decodeErr)
		}
		if current.SchemaVersion != updateStateSchemaVersion {
			return NodeUpdateState{}, false, fmt.Errorf("node update state schema version %d is unsupported", current.SchemaVersion)
		}
	}
	if current.ActionID == state.ActionID && current.ActionID != "" && isTerminalNodeUpdateState(current.State) {
		if err := s.commitWriteTx(ctx, tx); err != nil {
			return NodeUpdateState{}, false, err
		}
		return current, false, nil
	}

	state.SchemaVersion = updateStateSchemaVersion
	state.UpdatedAt = time.Now().UTC()
	encoded, err := json.Marshal(state)
	if err != nil {
		return NodeUpdateState{}, false, err
	}
	if len(encoded) > 1<<20 {
		return NodeUpdateState{}, false, errors.New("update state is too large")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, nodeUpdateStateKey(state.NodeID), string(encoded), state.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		return NodeUpdateState{}, false, err
	}
	if err := s.commitWriteTx(ctx, tx); err != nil {
		return NodeUpdateState{}, false, err
	}
	return state, true, nil
}

func isTerminalNodeUpdateState(state string) bool {
	switch state {
	case UpdateStateUpToDate, UpdateStateFailed, UpdateStateRolledBack, UpdateStateManualRequired:
		return true
	default:
		return false
	}
}

func nodeSupportsSelfUpdate(deployment string, capabilities []string) (bool, string) {
	deployment = strings.ToLower(strings.TrimSpace(deployment))
	if deployment != "windows-service" && deployment != "systemd" && deployment != "wsl" {
		return false, "self-upgrade is supported only for Windows services, systemd and WSL"
	}
	for _, capability := range capabilities {
		if capability == "node-upgrade-v1" {
			return true, ""
		}
	}
	return false, "Node must be upgraded once with the current installer before Controller-managed upgrades are available"
}
