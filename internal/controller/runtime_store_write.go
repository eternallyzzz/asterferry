package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

func upsertRuntimeConnectionTx(ctx context.Context, tx *sql.Tx, connection domain.RuntimeConnection, updatedAt time.Time) error {
	if err := connection.Validate(); err != nil {
		return err
	}
	var limitJSON []byte
	var err error
	if connection.Limit != nil {
		limitJSON, err = json.Marshal(connection.Limit)
		if err != nil {
			return err
		}
	}
	var ended any
	if connection.EndedAt != nil {
		ended = connection.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runtime_connections(node_id,id,type,state,peer_node_id,gateway_id,agent_id,assignment_id,service_id,protocol,source_ip,source_port,target,parent_session_id,started_at,last_activity_at,ended_at,close_reason,bytes_in,bytes_out,rate_in,rate_out,limit_json,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(node_id,id) DO UPDATE SET type=excluded.type,state=excluded.state,peer_node_id=excluded.peer_node_id,gateway_id=excluded.gateway_id,agent_id=excluded.agent_id,assignment_id=excluded.assignment_id,service_id=excluded.service_id,protocol=excluded.protocol,source_ip=excluded.source_ip,source_port=excluded.source_port,target=excluded.target,parent_session_id=excluded.parent_session_id,started_at=excluded.started_at,last_activity_at=excluded.last_activity_at,ended_at=excluded.ended_at,close_reason=excluded.close_reason,bytes_in=excluded.bytes_in,bytes_out=excluded.bytes_out,rate_in=excluded.rate_in,rate_out=excluded.rate_out,limit_json=excluded.limit_json,updated_at=excluded.updated_at`,
		connection.NodeID, connection.ID, connection.Type, connection.State, connection.PeerNodeID, connection.GatewayID, connection.AgentID, connection.AssignmentID, connection.ServiceID, connection.Protocol, connection.SourceIP, connection.SourcePort, connection.Target, connection.ParentSessionID, connection.StartedAt.UTC().Format(time.RFC3339Nano), connection.LastActivityAt.UTC().Format(time.RFC3339Nano), ended, connection.CloseReason, uint64ToDatabaseInt(connection.BytesIn), uint64ToDatabaseInt(connection.BytesOut), connection.RateIn, connection.RateOut, limitJSON, updatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func upsertRuntimeSnapshotTx(ctx context.Context, tx *sql.Tx, snapshot domain.RuntimeSnapshot) error {
	now := snapshot.ObservedAt.UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE runtime_connections SET state='unknown',updated_at=? WHERE node_id=? AND state='active'`, now.Format(time.RFC3339Nano), snapshot.NodeID); err != nil {
		return err
	}
	byGroup := make(map[string]struct {
		connection domain.RuntimeConnection
		count      uint64
	})
	for _, connection := range snapshot.Connections {
		if err := upsertRuntimeConnectionTx(ctx, tx, connection, now); err != nil {
			return err
		}
		key := connection.AssignmentID + "\x00" + connection.ServiceID + "\x00" + connection.Protocol
		group := byGroup[key]
		group.connection = connection
		group.count++
		byGroup[key] = group
	}
	for _, group := range byGroup {
		rollupConnection := group.connection
		rollupConnection.BytesIn, rollupConnection.BytesOut = 0, 0
		if err := upsertRuntimeRollupTx(ctx, tx, rollupConnection, now, 0, 0, 0, 0, group.count); err != nil {
			return err
		}
	}
	return nil
}

func upsertRuntimeRollupTx(ctx context.Context, tx *sql.Tx, txConnection domain.RuntimeConnection, at time.Time, opened, closed, rejected, limited, activeMax uint64) error {
	bucket := at.UTC().Truncate(time.Minute).Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `INSERT INTO runtime_traffic_rollups(bucket_start,node_id,gateway_id,agent_id,assignment_id,service_id,protocol,bytes_in,bytes_out,opened,closed,rejected,rate_limited,active_max) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(bucket_start,node_id,assignment_id,service_id,protocol) DO UPDATE SET gateway_id=excluded.gateway_id,agent_id=excluded.agent_id,bytes_in=runtime_traffic_rollups.bytes_in+excluded.bytes_in,bytes_out=runtime_traffic_rollups.bytes_out+excluded.bytes_out,opened=runtime_traffic_rollups.opened+excluded.opened,closed=runtime_traffic_rollups.closed+excluded.closed,rejected=runtime_traffic_rollups.rejected+excluded.rejected,rate_limited=runtime_traffic_rollups.rate_limited+excluded.rate_limited,active_max=CASE WHEN excluded.active_max>runtime_traffic_rollups.active_max THEN excluded.active_max ELSE runtime_traffic_rollups.active_max END`, bucket, txConnection.NodeID, txConnection.GatewayID, txConnection.AgentID, txConnection.AssignmentID, txConnection.ServiceID, txConnection.Protocol, uint64ToDatabaseInt(txConnection.BytesIn), uint64ToDatabaseInt(txConnection.BytesOut), uint64ToDatabaseInt(opened), uint64ToDatabaseInt(closed), uint64ToDatabaseInt(rejected), uint64ToDatabaseInt(limited), uint64ToDatabaseInt(activeMax))
	return err
}

func scanRuntimeConnection(row scanner) (domain.RuntimeConnection, error) {
	var connection domain.RuntimeConnection
	var sourcePort int64
	var started, last string
	var ended sql.NullString
	var bytesIn, bytesOut int64
	var limitJSON []byte
	if err := row.Scan(&connection.NodeID, &connection.ID, &connection.Type, &connection.State, &connection.PeerNodeID, &connection.GatewayID, &connection.AgentID, &connection.AssignmentID, &connection.ServiceID, &connection.Protocol, &connection.SourceIP, &sourcePort, &connection.Target, &connection.ParentSessionID, &started, &last, &ended, &connection.CloseReason, &bytesIn, &bytesOut, &connection.RateIn, &connection.RateOut, &limitJSON); err != nil {
		return domain.RuntimeConnection{}, err
	}
	if sourcePort < 0 || sourcePort > math.MaxUint16 {
		return domain.RuntimeConnection{}, errors.New("stored runtime source port is invalid")
	}
	connection.SourcePort = uint16(sourcePort)
	var err error
	connection.StartedAt, err = parseStoredTime("runtime_connection.started_at", started)
	if err != nil {
		return domain.RuntimeConnection{}, err
	}
	connection.LastActivityAt, err = parseStoredTime("runtime_connection.last_activity_at", last)
	if err != nil {
		return domain.RuntimeConnection{}, err
	}
	if ended.Valid && ended.String != "" {
		value, parseErr := parseStoredTime("runtime_connection.ended_at", ended.String)
		if parseErr != nil {
			return domain.RuntimeConnection{}, parseErr
		}
		connection.EndedAt = &value
	}
	connection.BytesIn, err = nonNegativeUint64(bytesIn)
	if err != nil {
		return domain.RuntimeConnection{}, err
	}
	connection.BytesOut, err = nonNegativeUint64(bytesOut)
	if err != nil {
		return domain.RuntimeConnection{}, err
	}
	if len(limitJSON) > 0 {
		var value domain.RuntimeRateLimit
		if err := json.Unmarshal(limitJSON, &value); err != nil {
			return domain.RuntimeConnection{}, err
		}
		connection.Limit = &value
	}
	if err := connection.Validate(); err != nil {
		return domain.RuntimeConnection{}, fmt.Errorf("stored runtime connection is invalid: %w", err)
	}
	return connection, nil
}

func nonNegativeUint64(value int64) (uint64, error) {
	if value < 0 {
		return 0, errors.New("stored runtime counter is negative")
	}
	return uint64(value), nil
}

func uint64ToDatabaseInt(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func (s *RuntimeRepository) PruneRuntimeHistory(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-runtimeRetention).Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_events WHERE created_at < ?`, cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_traffic_rollups WHERE bucket_start < ?`, cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_connections WHERE ended_at IS NOT NULL AND ended_at < ?`, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}
