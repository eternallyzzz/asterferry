package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"asterferry/internal/domain"
)

// Observed state is a resource-side aggregate because it participates in
// desired-generation validation and scheduler health decisions. Runtime
// telemetry rows are not part of this codec.
func loadObservedStateNormalized(ctx context.Context, q sqlQueryer, nodeID string) (domain.ObservedState, string, error) {
	var state domain.ObservedState
	var generation int64
	var protocolVersion int
	var healthy, degraded int
	var lastCode, lastPath, lastMessage, observedAt, updatedAt string
	var metrics [13]int64
	var geoipUp int
	if err := q.QueryRowContext(ctx, `SELECT node_id,generation,protocol_version,healthy,degraded,last_error_code,last_error_path,last_error_message,observed_at,updated_at,active_streams,active_sessions,active_egress,udp_oversize_drops,geoip_up,active_connections,active_flows,runtime_bytes_in_total,runtime_bytes_out_total,runtime_opened_total,runtime_closed_total,runtime_rejected_total,runtime_rate_limited_total,runtime_telemetry_dropped_total FROM observed_states WHERE node_id=?`, nodeID).Scan(&state.NodeID, &generation, &protocolVersion, &healthy, &degraded, &lastCode, &lastPath, &lastMessage, &observedAt, &updatedAt, &metrics[0], &metrics[1], &metrics[2], &metrics[3], &geoipUp, &metrics[4], &metrics[5], &metrics[6], &metrics[7], &metrics[8], &metrics[9], &metrics[10], &metrics[11], &metrics[12]); err != nil {
		return domain.ObservedState{}, "", err
	}
	var err error
	if protocolVersion <= 0 {
		return domain.ObservedState{}, "", errors.New("stored observed protocol version is invalid")
	}
	state.SchemaVersion = uint32(protocolVersion)
	state.AppliedGeneration, err = storedUint64(generation, "observed generation")
	if err != nil {
		return domain.ObservedState{}, "", err
	}
	state.Healthy = healthy != 0
	state.Degraded = degraded != 0
	state.ObservedAt, err = parseStoredTime("observed.observed_at", observedAt)
	if err != nil {
		return domain.ObservedState{}, "", err
	}
	if lastCode != "" {
		state.LastError = &domain.ApplyError{Code: lastCode, Path: lastPath, Message: lastMessage}
	}
	metricValues := make([]uint64, len(metrics))
	for index, value := range metrics {
		metricValues[index], err = storedUint64(value, fmt.Sprintf("observed metric %d", index))
		if err != nil {
			return domain.ObservedState{}, "", err
		}
	}
	state.Metrics = domain.RuntimeMetrics{ActiveStreams: metricValues[0], ActiveSessions: metricValues[1], ActiveEgress: metricValues[2], UDPOversizeDrops: metricValues[3], GeoIPUp: geoipUp != 0, ActiveConnections: metricValues[4], ActiveFlows: metricValues[5], RuntimeBytesInTotal: metricValues[6], RuntimeBytesOutTotal: metricValues[7], RuntimeOpenedTotal: metricValues[8], RuntimeClosedTotal: metricValues[9], RuntimeRejectedTotal: metricValues[10], RuntimeRateLimitedTotal: metricValues[11], RuntimeTelemetryDroppedTotal: metricValues[12]}
	rows, err := q.QueryContext(ctx, `SELECT position,id,peer_id,started_at,streams FROM observed_sessions WHERE node_id=? ORDER BY position`, nodeID)
	if err != nil {
		return domain.ObservedState{}, "", err
	}
	for expected := 0; rows.Next(); expected++ {
		var position, streams int64
		var session domain.SessionSummary
		var started string
		if err := rows.Scan(&position, &session.ID, &session.PeerID, &started, &streams); err != nil {
			_ = rows.Close()
			return domain.ObservedState{}, "", err
		}
		if err := requireStoredPosition(position, expected, "observed session"); err != nil {
			_ = rows.Close()
			return domain.ObservedState{}, "", err
		}
		session.StartedAt, err = parseStoredTime("observed.session.started_at", started)
		if err != nil {
			_ = rows.Close()
			return domain.ObservedState{}, "", err
		}
		if streams < 0 {
			_ = rows.Close()
			return domain.ObservedState{}, "", errors.New("stored observed session stream count is negative")
		}
		session.Streams = int(streams)
		state.Sessions = append(state.Sessions, session)
	}
	if err := rows.Close(); err != nil {
		return domain.ObservedState{}, "", err
	}
	if err := rows.Err(); err != nil {
		return domain.ObservedState{}, "", err
	}
	rows, err = q.QueryContext(ctx, `SELECT position,protocol,bind,port,ready FROM observed_listeners WHERE node_id=? ORDER BY position`, nodeID)
	if err != nil {
		return domain.ObservedState{}, "", err
	}
	for expected := 0; rows.Next(); expected++ {
		var position, port int64
		var listener domain.ListenerState
		var ready int
		if err := rows.Scan(&position, &listener.Protocol, &listener.Bind, &port, &ready); err != nil {
			_ = rows.Close()
			return domain.ObservedState{}, "", err
		}
		if err := requireStoredPosition(position, expected, "observed listener"); err != nil {
			_ = rows.Close()
			return domain.ObservedState{}, "", err
		}
		listener.Port, err = storedUint16(port, "observed listener port")
		if err != nil {
			_ = rows.Close()
			return domain.ObservedState{}, "", err
		}
		listener.Ready = ready != 0
		state.Listeners = append(state.Listeners, listener)
	}
	if err := rows.Close(); err != nil {
		return domain.ObservedState{}, "", err
	}
	if err := rows.Err(); err != nil {
		return domain.ObservedState{}, "", err
	}
	if err := state.Validate(); err != nil {
		return domain.ObservedState{}, "", err
	}
	return state, updatedAt, nil
}

func uint64SQL(value uint64, field string) (int64, error) {
	if value > uint64(^uint64(0)>>1) {
		return 0, fmt.Errorf("%s exceeds database integer range", field)
	}
	return int64(value), nil
}

func writeObservedStateTx(ctx context.Context, tx *sql.Tx, state domain.ObservedState, updatedAt time.Time) error {
	if err := state.Validate(); err != nil {
		return err
	}
	observedAt := state.ObservedAt
	if observedAt.IsZero() {
		observedAt = updatedAt
	}
	metrics := []uint64{state.Metrics.ActiveStreams, state.Metrics.ActiveSessions, state.Metrics.ActiveEgress, state.Metrics.UDPOversizeDrops, state.Metrics.ActiveConnections, state.Metrics.ActiveFlows, state.Metrics.RuntimeBytesInTotal, state.Metrics.RuntimeBytesOutTotal, state.Metrics.RuntimeOpenedTotal, state.Metrics.RuntimeClosedTotal, state.Metrics.RuntimeRejectedTotal, state.Metrics.RuntimeRateLimitedTotal, state.Metrics.RuntimeTelemetryDroppedTotal}
	metricSQL := make([]any, 0, len(metrics))
	for index, value := range metrics {
		converted, err := uint64SQL(value, fmt.Sprintf("metrics[%d]", index))
		if err != nil {
			return err
		}
		metricSQL = append(metricSQL, converted)
	}
	lastCode, lastPath, lastMessage := "", "", ""
	if state.LastError != nil {
		lastCode, lastPath, lastMessage = state.LastError.Code, state.LastError.Path, state.LastError.Message
	}
	args := []any{state.NodeID, state.AppliedGeneration, state.SchemaVersion, boolInt(state.Healthy), boolInt(state.Degraded), lastCode, lastPath, lastMessage, observedAt.Format(time.RFC3339Nano), updatedAt.Format(time.RFC3339Nano), metricSQL[0], metricSQL[1], metricSQL[2], metricSQL[3], boolInt(state.Metrics.GeoIPUp), metricSQL[4], metricSQL[5], metricSQL[6], metricSQL[7], metricSQL[8], metricSQL[9], metricSQL[10], metricSQL[11], metricSQL[12]}
	if _, err := tx.ExecContext(ctx, `INSERT INTO observed_states(node_id,generation,protocol_version,healthy,degraded,last_error_code,last_error_path,last_error_message,observed_at,updated_at,active_streams,active_sessions,active_egress,udp_oversize_drops,geoip_up,active_connections,active_flows,runtime_bytes_in_total,runtime_bytes_out_total,runtime_opened_total,runtime_closed_total,runtime_rejected_total,runtime_rate_limited_total,runtime_telemetry_dropped_total) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET generation=excluded.generation,protocol_version=excluded.protocol_version,healthy=excluded.healthy,degraded=excluded.degraded,last_error_code=excluded.last_error_code,last_error_path=excluded.last_error_path,last_error_message=excluded.last_error_message,observed_at=excluded.observed_at,updated_at=excluded.updated_at,active_streams=excluded.active_streams,active_sessions=excluded.active_sessions,active_egress=excluded.active_egress,udp_oversize_drops=excluded.udp_oversize_drops,geoip_up=excluded.geoip_up,active_connections=excluded.active_connections,active_flows=excluded.active_flows,runtime_bytes_in_total=excluded.runtime_bytes_in_total,runtime_bytes_out_total=excluded.runtime_bytes_out_total,runtime_opened_total=excluded.runtime_opened_total,runtime_closed_total=excluded.runtime_closed_total,runtime_rejected_total=excluded.runtime_rejected_total,runtime_rate_limited_total=excluded.runtime_rate_limited_total,runtime_telemetry_dropped_total=excluded.runtime_telemetry_dropped_total`, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM observed_sessions WHERE node_id=?`, state.NodeID); err != nil {
		return err
	}
	for position, session := range state.Sessions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO observed_sessions(node_id,position,id,peer_id,started_at,streams) VALUES(?,?,?,?,?,?)`, state.NodeID, position, session.ID, session.PeerID, session.StartedAt.Format(time.RFC3339Nano), session.Streams); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM observed_listeners WHERE node_id=?`, state.NodeID); err != nil {
		return err
	}
	for position, listener := range state.Listeners {
		if _, err := tx.ExecContext(ctx, `INSERT INTO observed_listeners(node_id,position,protocol,bind,port,ready) VALUES(?,?,?,?,?,?)`, state.NodeID, position, listener.Protocol, normalizeBind(listener.Bind), listener.Port, boolInt(listener.Ready)); err != nil {
			return err
		}
	}
	return nil
}
