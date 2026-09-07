package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"asterferry/internal/buildinfo"
	"asterferry/internal/domain"
)

const updateStateSchemaVersion = 1

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

type sqlRowScanner interface {
	Scan(dest ...any) error
}

func scanControllerUpdateState(scanner sqlRowScanner, state *ControllerUpdateState) error {
	var schemaVersion, supported int
	var publishedAt, lastCheckedAt sql.NullString
	var updatedAt string
	if err := scanner.Scan(
		&schemaVersion, &state.CurrentVersion, &state.Channel, &state.Deployment, &supported,
		&state.State, &state.LatestVersion, &state.LatestURL, &publishedAt, &lastCheckedAt,
		&state.LastError, &state.TargetVersion, &state.PreviousVersion, &updatedAt,
	); err != nil {
		return err
	}
	state.SchemaVersion = schemaVersion
	state.Supported = supported != 0
	var err error
	state.PublishedAt, err = parseNullableStoredTime("controller_update_state.published_at", publishedAt)
	if err != nil {
		return err
	}
	state.LastCheckedAt, err = parseNullableStoredTime("controller_update_state.last_checked_at", lastCheckedAt)
	if err != nil {
		return err
	}
	state.UpdatedAt, err = parseStoredTime("controller_update_state.updated_at", updatedAt)
	return err
}

func scanNodeUpdateState(scanner sqlRowScanner, state *NodeUpdateState) error {
	var schemaVersion, supported int
	var updatedAt string
	if err := scanner.Scan(
		&state.NodeID, &schemaVersion, &state.ActionID, &state.CurrentVersion, &state.TargetVersion,
		&state.State, &supported, &state.Deployment, &state.Reason, &state.LastError, &updatedAt,
	); err != nil {
		return err
	}
	state.SchemaVersion = schemaVersion
	state.Supported = supported != 0
	var err error
	state.UpdatedAt, err = parseStoredTime("node_update_states.updated_at", updatedAt)
	return err
}

func parseNullableStoredTime(field string, value sql.NullString) (time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return time.Time{}, nil
	}
	return parseStoredTime(field, value.String)
}

func nullableStoredTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func (s *ResourceRepository) GetControllerUpdateState(ctx context.Context, config Config) (ControllerUpdateState, error) {
	state := defaultControllerUpdateState(config)
	err := scanControllerUpdateState(s.db.QueryRowContext(ctx, `SELECT schema_version,current_version,channel,deployment,supported,state,latest_version,latest_url,published_at,last_checked_at,last_error,target_version,previous_version,updated_at FROM controller_update_state WHERE singleton=1`), &state)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
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
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO controller_update_state(singleton,schema_version,current_version,channel,deployment,supported,state,latest_version,latest_url,published_at,last_checked_at,last_error,target_version,previous_version,updated_at) VALUES(1,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(singleton) DO UPDATE SET schema_version=excluded.schema_version,current_version=excluded.current_version,channel=excluded.channel,deployment=excluded.deployment,supported=excluded.supported,state=excluded.state,latest_version=excluded.latest_version,latest_url=excluded.latest_url,published_at=excluded.published_at,last_checked_at=excluded.last_checked_at,last_error=excluded.last_error,target_version=excluded.target_version,previous_version=excluded.previous_version,updated_at=excluded.updated_at`,
		state.SchemaVersion, state.CurrentVersion, state.Channel, state.Deployment, boolInt(state.Supported), state.State,
		state.LatestVersion, state.LatestURL, nullableStoredTime(state.PublishedAt), nullableStoredTime(state.LastCheckedAt),
		state.LastError, state.TargetVersion, state.PreviousVersion, state.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return s.commitWriteTx(ctx, tx)
}

func (s *ResourceRepository) GetNodeUpdateState(ctx context.Context, nodeID string) (NodeUpdateState, error) {
	state := NodeUpdateState{SchemaVersion: updateStateSchemaVersion, NodeID: strings.TrimSpace(nodeID), State: UpdateStateUnsupported}
	err := scanNodeUpdateState(s.db.QueryRowContext(ctx, `SELECT node_id,schema_version,action_id,current_version,target_version,state,supported,deployment,reason,last_error,updated_at FROM node_update_states WHERE node_id=?`, nodeID), &state)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return NodeUpdateState{}, err
	}
	if state.SchemaVersion != updateStateSchemaVersion {
		return NodeUpdateState{}, fmt.Errorf("node update state schema version %d is unsupported", state.SchemaVersion)
	}
	return state, nil
}

func (s *ResourceRepository) GetApplyingNodeUpdate(ctx context.Context) (NodeUpdateState, bool, error) {
	state := NodeUpdateState{}
	err := scanNodeUpdateState(s.db.QueryRowContext(ctx, `SELECT node_id,schema_version,action_id,current_version,target_version,state,supported,deployment,reason,last_error,updated_at FROM node_update_states WHERE state=? ORDER BY updated_at,node_id LIMIT 1`, UpdateStateApplying), &state)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeUpdateState{}, false, nil
	}
	if err != nil {
		return NodeUpdateState{}, false, err
	}
	if state.SchemaVersion != updateStateSchemaVersion {
		return NodeUpdateState{}, false, fmt.Errorf("node update state schema version %d is unsupported", state.SchemaVersion)
	}
	return state, true, nil
}

func (s *ResourceRepository) SaveNodeUpdateState(ctx context.Context, state NodeUpdateState) error {
	if err := domain.ValidateID(state.NodeID, "node_id"); err != nil {
		return err
	}
	state.SchemaVersion = updateStateSchemaVersion
	state.UpdatedAt = time.Now().UTC()
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertNodeUpdateState(ctx, tx, state); err != nil {
		return err
	}
	return s.commitWriteTx(ctx, tx)
}

func upsertNodeUpdateState(ctx context.Context, tx *sql.Tx, state NodeUpdateState) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO node_update_states(node_id,schema_version,action_id,current_version,target_version,state,supported,deployment,reason,last_error,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET schema_version=excluded.schema_version,action_id=excluded.action_id,current_version=excluded.current_version,target_version=excluded.target_version,state=excluded.state,supported=excluded.supported,deployment=excluded.deployment,reason=excluded.reason,last_error=excluded.last_error,updated_at=excluded.updated_at`,
		state.NodeID, state.SchemaVersion, state.ActionID, state.CurrentVersion, state.TargetVersion, state.State, boolInt(state.Supported), state.Deployment, state.Reason, state.LastError, state.UpdatedAt.Format(time.RFC3339Nano))
	return err
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
	err = scanNodeUpdateState(tx.QueryRowContext(ctx, `SELECT node_id,schema_version,action_id,current_version,target_version,state,supported,deployment,reason,last_error,updated_at FROM node_update_states WHERE node_id=?`+s.selectForUpdateClause(), state.NodeID), &current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return NodeUpdateState{}, false, err
	}
	if err == nil {
		if current.SchemaVersion != updateStateSchemaVersion {
			return NodeUpdateState{}, false, fmt.Errorf("node update state schema version %d is unsupported", current.SchemaVersion)
		}
		if current.ActionID == state.ActionID && current.ActionID != "" && isTerminalNodeUpdateState(current.State) {
			if err := s.commitWriteTx(ctx, tx); err != nil {
				return NodeUpdateState{}, false, err
			}
			return current, false, nil
		}
	}

	state.SchemaVersion = updateStateSchemaVersion
	state.UpdatedAt = time.Now().UTC()
	if err := upsertNodeUpdateState(ctx, tx, state); err != nil {
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
