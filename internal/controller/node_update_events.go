package controller

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

func (s *ControlServer) applyNodeUpgradeEvent(ctx context.Context, nodeID string, attributes map[string]string) error {
	stateName := strings.TrimSpace(attributes["state"])
	switch stateName {
	case UpdateStateApplying, UpdateStateUpToDate, UpdateStateFailed, UpdateStateRolledBack, UpdateStateManualRequired:
	default:
		return errors.New("node upgrade event state is invalid")
	}
	current, err := s.resources.GetNodeUpdateState(ctx, nodeID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	current.NodeID = nodeID
	if value := strings.TrimSpace(attributes["action_id"]); value != "" {
		current.ActionID = value
	}
	if value := strings.TrimSpace(attributes["version"]); value != "" {
		current.TargetVersion = value
	}
	if value := strings.TrimSpace(attributes["current_version"]); value != "" {
		current.CurrentVersion = value
	}
	if value := strings.TrimSpace(attributes["deployment"]); value != "" {
		current.Deployment = value
	}
	current.State = stateName
	current.Supported = true
	current.LastError = strings.TrimSpace(attributes["error"])
	if stateName == UpdateStateUpToDate {
		current.LastError = ""
	}
	return s.resources.SaveNodeUpdateState(ctx, current)
}
