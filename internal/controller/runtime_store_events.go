package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) RecordRuntimeEvent(ctx context.Context, nodeID, eventID, eventType string, payload []byte, createdAt time.Time) error {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return err
	}
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		eventID, _ = randomID()
	}
	if err := domain.ValidateID(eventID, "runtime_event.id"); err != nil {
		return err
	}
	eventType = strings.TrimSpace(eventType)
	if eventType != "runtime_connection" && eventType != "runtime_snapshot" && eventType != "runtime_action_result" {
		return errors.New("runtime event type is unsupported")
	}
	if len(payload) == 0 || len(payload) > 4<<20 {
		return errors.New("runtime event payload is empty or too large")
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	createdAt = createdAt.UTC()
	var connectionID string
	var connection *domain.RuntimeConnection
	var snapshot *domain.RuntimeSnapshot
	var runtimeEvent domain.RuntimeEvent
	switch eventType {
	case "runtime_connection":
		if err := json.Unmarshal(payload, &runtimeEvent); err != nil {
			return errors.New("runtime connection event payload is invalid")
		}
		if err := runtimeEvent.Validate(); err != nil {
			return fmt.Errorf("validate runtime connection event: %w", err)
		}
		if runtimeEvent.NodeID != nodeID {
			return errors.New("runtime event node identity does not match stream")
		}
		connectionID = runtimeEvent.ConnectionID
		if runtimeEvent.Connection != nil {
			connection = runtimeEvent.Connection
		}
	case "runtime_snapshot":
		if err := json.Unmarshal(payload, &snapshot); err != nil || snapshot == nil {
			return errors.New("runtime snapshot payload is invalid")
		}
		if err := snapshot.Validate(); err != nil {
			return fmt.Errorf("validate runtime snapshot: %w", err)
		}
		if snapshot.NodeID != nodeID {
			return errors.New("runtime snapshot node identity does not match stream")
		}
	case "runtime_action_result":
		var result struct {
			ActionID string `json:"action_id"`
			Action   string `json:"action"`
			Affected int    `json:"affected"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(payload, &result); err != nil || strings.TrimSpace(result.Action) == "" || len(result.Error) > 2048 {
			return errors.New("runtime action result payload is invalid")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	persistEvent := true
	if eventType == "runtime_snapshot" {
		// Snapshots are repair frames, not lifecycle history.  Keep at most one
		// durable repair point per node/minute; otherwise a 100-node Controller
		// would write millions of identical state frames per month.
		var last string
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM runtime_events WHERE node_id=? AND event_type='runtime_snapshot' ORDER BY id DESC LIMIT 1`, nodeID).Scan(&last)
		if err == nil {
			if previous, parseErr := parseStoredTime("runtime_snapshot.created_at", last); parseErr == nil && createdAt.Sub(previous) < time.Minute {
				persistEvent = false
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	inserted := int64(1)
	if persistEvent {
		insertSQL := `INSERT OR IGNORE INTO runtime_events(event_id,node_id,connection_id,event_type,payload_json,created_at) VALUES(?,?,?,?,?,?)`
		if s.dialect != nil && s.dialect.backend() == databaseBackendPostgres {
			insertSQL = `INSERT INTO runtime_events(event_id,node_id,connection_id,event_type,payload_json,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`
		}
		result, err := tx.ExecContext(ctx, insertSQL, eventID, nodeID, connectionID, eventType, payload, createdAt.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		inserted, err = result.RowsAffected()
		if err != nil {
			return err
		}
	}
	if inserted == 0 {
		return tx.Commit()
	}
	if connection != nil {
		if err := upsertRuntimeConnectionTx(ctx, tx, *connection, createdAt); err != nil {
			return err
		}
		opened, closed, rejected, limited := uint64(0), uint64(0), uint64(0), uint64(0)
		switch runtimeEvent.Type {
		case domain.RuntimeEventOpened:
			opened = 1
		case domain.RuntimeEventClosed:
			closed = 1
		case domain.RuntimeEventRejected:
			rejected = 1
		case domain.RuntimeEventRateLimited:
			limited = 1
		}
		if err := upsertRuntimeRollupTx(ctx, tx, *connection, createdAt, opened, closed, rejected, limited, 0); err != nil {
			return err
		}
	} else if eventType == "runtime_connection" && runtimeEvent.Type == domain.RuntimeEventRejected {
		// A rejection can happen before a connection ID or service is known
		// (for example, a public listener with no authenticated Agent session).
		// Keep that signal in a node-level rollup without inventing a current
		// connection row.
		ended := createdAt
		rejected := domain.RuntimeConnection{ID: eventID, Type: domain.RuntimeConnectionSession, NodeID: nodeID, State: domain.RuntimeStateClosed, StartedAt: createdAt, LastActivityAt: createdAt, EndedAt: &ended}
		if err := upsertRuntimeRollupTx(ctx, tx, rejected, createdAt, 0, 0, 1, 0, 0); err != nil {
			return err
		}
	}
	if snapshot != nil {
		if err := upsertRuntimeSnapshotTx(ctx, tx, *snapshot); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyRuntimeChanges(nodeID)
	return nil
}
