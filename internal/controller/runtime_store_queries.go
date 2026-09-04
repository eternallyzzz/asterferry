package controller

import (
	"asterferry/internal/domain"
	"context"
	"encoding/json"
	"strings"
	"time"
)

func (s *RuntimeRepository) ListRuntimeConnections(ctx context.Context, filter RuntimeConnectionFilter) ([]domain.RuntimeConnection, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT node_id,id,type,state,peer_node_id,gateway_id,agent_id,assignment_id,service_id,protocol,source_ip,source_port,target,parent_session_id,started_at,last_activity_at,ended_at,close_reason,bytes_in,bytes_out,rate_in,rate_out,limit_json FROM runtime_connections WHERE 1=1`
	args := make([]any, 0, 10)
	for _, field := range []struct{ column, value string }{
		{"node_id", filter.NodeID}, {"state", filter.State}, {"type", filter.Type}, {"source_ip", filter.SourceIP},
		{"peer_node_id", filter.PeerNodeID}, {"gateway_id", filter.GatewayID}, {"agent_id", filter.AgentID},
		{"assignment_id", filter.AssignmentID}, {"service_id", filter.ServiceID}, {"protocol", filter.Protocol},
	} {
		if strings.TrimSpace(field.value) != "" {
			query += ` AND ` + field.column + `=?`
			args = append(args, strings.TrimSpace(field.value))
		}
	}
	query += ` ORDER BY CASE state WHEN 'active' THEN 0 WHEN 'unknown' THEN 1 ELSE 2 END,last_activity_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.RuntimeConnection, 0)
	for rows.Next() {
		connection, err := scanRuntimeConnection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, connection)
	}
	return result, rows.Err()
}

// MarkRuntimeConnectionsUnknown prevents an abandoned control stream from
// leaving rows that look active forever. A reconnect immediately repairs the
// rows with its next runtime snapshot; closed rows are preserved unchanged.
func (s *RuntimeRepository) MarkRuntimeConnectionsUnknown(ctx context.Context, nodeID string, at time.Time) error {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE runtime_connections SET state='unknown',updated_at=? WHERE node_id=? AND state='active'`, at.UTC().Format(time.RFC3339Nano), nodeID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if err := s.commitWriteTx(ctx, tx); err != nil {
		return err
	}
	if changed > 0 {
		s.ChangeBus().notifyRuntimeChanges(nodeID)
	}
	return nil
}

func (s *RuntimeRepository) MarkAllRuntimeConnectionsUnknown(ctx context.Context, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE runtime_connections SET state='unknown',updated_at=? WHERE state='active'`, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if err := s.commitWriteTx(ctx, tx); err != nil {
		return err
	}
	if changed > 0 {
		s.ChangeBus().notifyRuntimeChanges("")
	}
	return nil
}

func (s *RuntimeRepository) GetRuntimeConnection(ctx context.Context, nodeID, connectionID string) (domain.RuntimeConnection, error) {
	return scanRuntimeConnection(s.db.QueryRowContext(ctx, `SELECT node_id,id,type,state,peer_node_id,gateway_id,agent_id,assignment_id,service_id,protocol,source_ip,source_port,target,parent_session_id,started_at,last_activity_at,ended_at,close_reason,bytes_in,bytes_out,rate_in,rate_out,limit_json FROM runtime_connections WHERE node_id=? AND id=?`, nodeID, connectionID))
}

func (s *RuntimeRepository) ListRuntimeEvents(ctx context.Context, nodeID string, limit int) ([]RuntimeEventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id,event_id,node_id,connection_id,event_type,payload_json,created_at FROM runtime_events`
	args := []any{}
	if strings.TrimSpace(nodeID) != "" {
		query += ` WHERE node_id=?`
		args = append(args, strings.TrimSpace(nodeID))
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RuntimeEventRecord, 0)
	for rows.Next() {
		var value RuntimeEventRecord
		var payload []byte
		var created string
		if err := rows.Scan(&value.ID, &value.EventID, &value.NodeID, &value.ConnectionID, &value.Type, &payload, &created); err != nil {
			return nil, err
		}
		value.Payload = append(json.RawMessage(nil), payload...)
		var err error
		value.CreatedAt, err = parseStoredTime("runtime_event.created_at", created)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *RuntimeRepository) ListRuntimeTraffic(ctx context.Context, nodeID string, limit int) ([]RuntimeTrafficRollup, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	query := `SELECT bucket_start,node_id,gateway_id,agent_id,assignment_id,service_id,protocol,bytes_in,bytes_out,opened,closed,rejected,rate_limited,active_max FROM runtime_traffic_rollups`
	args := []any{}
	if strings.TrimSpace(nodeID) != "" {
		query += ` WHERE node_id=?`
		args = append(args, strings.TrimSpace(nodeID))
	}
	query += ` ORDER BY bucket_start DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RuntimeTrafficRollup, 0)
	for rows.Next() {
		var value RuntimeTrafficRollup
		var bucket string
		var bytesIn, bytesOut, opened, closed, rejected, limited, active int64
		if err := rows.Scan(&bucket, &value.NodeID, &value.GatewayID, &value.AgentID, &value.AssignmentID, &value.ServiceID, &value.Protocol, &bytesIn, &bytesOut, &opened, &closed, &rejected, &limited, &active); err != nil {
			return nil, err
		}
		var err error
		value.BucketStart, err = parseStoredTime("runtime_rollup.bucket_start", bucket)
		if err != nil {
			return nil, err
		}
		value.BytesIn, err = nonNegativeUint64(bytesIn)
		if err != nil {
			return nil, err
		}
		value.BytesOut, err = nonNegativeUint64(bytesOut)
		if err != nil {
			return nil, err
		}
		value.Opened, err = nonNegativeUint64(opened)
		if err != nil {
			return nil, err
		}
		value.Closed, err = nonNegativeUint64(closed)
		if err != nil {
			return nil, err
		}
		value.Rejected, err = nonNegativeUint64(rejected)
		if err != nil {
			return nil, err
		}
		value.RateLimited, err = nonNegativeUint64(limited)
		if err != nil {
			return nil, err
		}
		value.ActiveMax, err = nonNegativeUint64(active)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
