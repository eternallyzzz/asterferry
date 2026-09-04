package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"asterferry/internal/domain"
)

// Relational aggregate loaders and writers for services, assignments and
// observed state live here so the common repository codec stays small.
func loadNodeSpecNormalized(ctx context.Context, q sqlQueryer, nodeID string) (domain.NodeSpec, error) {
	var kind string
	var revision int64
	var updated string
	if err := q.QueryRowContext(ctx, `SELECT kind,revision,updated_at FROM node_specs WHERE node_id=?`, nodeID).Scan(&kind, &revision, &updated); err != nil {
		return domain.NodeSpec{}, err
	}
	parsed, err := parseStoredTime("node_spec.updated_at", updated)
	if err != nil {
		return domain.NodeSpec{}, err
	}
	spec := domain.NodeSpec{NodeID: nodeID, Kind: domain.NodeSpecKind(kind), Revision: revision, UpdatedAt: parsed}
	switch spec.Kind {
	case domain.NodeSpecGateway:
		value, err := loadGatewaySpecNormalized(ctx, q, nodeID, revision)
		if err != nil {
			return domain.NodeSpec{}, err
		}
		spec.Gateway = &value
	case domain.NodeSpecAgent:
		value, err := loadAgentSpecNormalized(ctx, q, nodeID, revision)
		if err != nil {
			return domain.NodeSpec{}, err
		}
		spec.Agent = &value
	default:
		return domain.NodeSpec{}, &domain.ApplyError{Code: "invalid_spec_kind", Path: "node_spec.kind", Message: "stored node spec kind is invalid"}
	}
	if err := spec.Validate(); err != nil {
		return domain.NodeSpec{}, fmt.Errorf("stored node spec is invalid: %w", err)
	}
	return spec, nil
}

func writeNodeSpecNormalizedTx(ctx context.Context, tx *sql.Tx, spec domain.NodeSpec) error {
	if spec.Gateway != nil {
		return replaceGatewaySpecTx(ctx, tx, *spec.Gateway)
	}
	if spec.Agent != nil {
		return replaceAgentSpecTx(ctx, tx, *spec.Agent)
	}
	return errors.New("node spec has no typed configuration")
}

func loadServiceNormalized(ctx context.Context, q sqlQueryer, id string) (domain.Service, error) {
	var service domain.Service
	var publicPort, enabled int64
	var revision int64
	var updated string
	var err error
	if err := q.QueryRowContext(ctx, `SELECT id,agent_id,protocol,local_target,public_bind,public_port,enabled,revision,updated_at FROM services WHERE id=?`, id).Scan(&service.ID, &service.AgentID, &service.Protocol, &service.LocalTarget, &service.PublicBind, &publicPort, &enabled, &revision, &updated); err != nil {
		return domain.Service{}, err
	}
	service.PublicPort, err = storedPort(publicPort, "service public port")
	if err != nil {
		return domain.Service{}, err
	}
	service.Enabled = enabled != 0
	service.Revision = revision
	service.UpdatedAt, err = parseStoredTime("service.updated_at", updated)
	if err != nil {
		return domain.Service{}, err
	}
	service.GatewaySelector.MatchLabels, err = loadStringMap(ctx, q, "service_selector_labels", "service_id", id)
	if err != nil {
		return domain.Service{}, err
	}
	if err := service.Validate(); err != nil {
		return domain.Service{}, fmt.Errorf("stored service is invalid: %w", err)
	}
	return service, nil
}

func replaceServiceSelectorTx(ctx context.Context, tx *sql.Tx, serviceID string, selector domain.Selector) error {
	return replaceStringMapTx(ctx, tx, "service_selector_labels", "service_id", serviceID, selector.MatchLabels)
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func loadAssignmentNormalized(ctx context.Context, q sqlQueryer, id string) (domain.Assignment, error) {
	var assignment domain.Assignment
	var generation, revision, maxPadding int64
	var shaping int
	var keyCiphertext, previousKeyCiphertext []byte
	var updated string
	if err := q.QueryRowContext(ctx, `SELECT id,gateway_id,agent_id,generation,state,public_endpoint,obfuscation_mode,obfuscation_key_ciphertext,obfuscation_previous_key_ciphertext,obfuscation_key_id,obfuscation_previous_key_id,obfuscation_max_padding_bytes,obfuscation_handshake_shaping,revision,updated_at FROM assignments WHERE id=?`, id).Scan(&assignment.ID, &assignment.GatewayID, &assignment.AgentID, &generation, &assignment.State, &assignment.PublicEndpoint, &assignment.Obfuscation.Mode, &keyCiphertext, &previousKeyCiphertext, &assignment.Obfuscation.KeyID, &assignment.Obfuscation.PreviousKeyID, &maxPadding, &shaping, &revision, &updated); err != nil {
		return domain.Assignment{}, err
	}
	var err error
	assignment.UpdatedAt, err = parseStoredTime("assignment.updated_at", updated)
	if err != nil {
		return domain.Assignment{}, err
	}
	assignment.Generation, err = storedUint64(generation, "assignment generation")
	if err != nil {
		return domain.Assignment{}, err
	}
	assignment.Revision = revision
	assignment.Obfuscation.KeyCiphertext = append([]byte(nil), keyCiphertext...)
	assignment.Obfuscation.PreviousKeyCiphertext = append([]byte(nil), previousKeyCiphertext...)
	assignment.Obfuscation.MaxPaddingBytes = int(maxPadding)
	assignment.Obfuscation.HandshakeShaping = shaping != 0
	if assignment.State == "" {
		assignment.State = domain.AssignmentPending
	}
	rows, err := q.QueryContext(ctx, `SELECT position,service_id FROM assignment_services WHERE assignment_id=? ORDER BY position`, id)
	if err != nil {
		return domain.Assignment{}, err
	}
	for rows.Next() {
		var position int64
		var serviceID string
		if err := rows.Scan(&position, &serviceID); err != nil {
			_ = rows.Close()
			return domain.Assignment{}, err
		}
		assignment.ServiceIDs = append(assignment.ServiceIDs, serviceID)
	}
	if err := rows.Close(); err != nil {
		return domain.Assignment{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.Assignment{}, err
	}
	rows, err = q.QueryContext(ctx, `SELECT position,service_id,gateway_id,protocol,bind,port FROM assignment_bindings WHERE assignment_id=? ORDER BY position`, id)
	if err != nil {
		return domain.Assignment{}, err
	}
	for rows.Next() {
		var position, port int64
		var binding domain.Binding
		var gatewayID string
		if err := rows.Scan(&position, &binding.ServiceID, &gatewayID, &binding.Protocol, &binding.Bind, &port); err != nil {
			_ = rows.Close()
			return domain.Assignment{}, err
		}
		if gatewayID != assignment.GatewayID {
			_ = rows.Close()
			return domain.Assignment{}, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment.bindings", Message: "stored binding gateway does not match assignment"}
		}
		binding.Port, err = storedUint16(port, "assignment binding port")
		if err != nil {
			_ = rows.Close()
			return domain.Assignment{}, err
		}
		assignment.Bindings = append(assignment.Bindings, binding)
	}
	if err := rows.Close(); err != nil {
		return domain.Assignment{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.Assignment{}, err
	}
	if err := assignment.Validate(); err != nil {
		return domain.Assignment{}, fmt.Errorf("stored assignment is invalid: %w", err)
	}
	return assignment, nil
}

func updateAssignmentColumnsTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment, expectedRevision int64) (sql.Result, error) {
	return tx.ExecContext(ctx, `UPDATE assignments SET gateway_id=?,agent_id=?,generation=?,state=?,public_endpoint=?,obfuscation_mode=?,obfuscation_key_ciphertext=?,obfuscation_previous_key_ciphertext=?,obfuscation_key_id=?,obfuscation_previous_key_id=?,obfuscation_max_padding_bytes=?,obfuscation_handshake_shaping=?,revision=?,updated_at=? WHERE id=? AND revision=?`, assignment.GatewayID, assignment.AgentID, assignment.Generation, assignment.State, assignment.PublicEndpoint, assignment.Obfuscation.Mode, nullableBytes(assignment.Obfuscation.KeyCiphertext), nullableBytes(assignment.Obfuscation.PreviousKeyCiphertext), assignment.Obfuscation.KeyID, assignment.Obfuscation.PreviousKeyID, assignment.Obfuscation.MaxPaddingBytes, boolInt(assignment.Obfuscation.HandshakeShaping), assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano), assignment.ID, expectedRevision)
}

func insertAssignmentColumnsTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) (sql.Result, error) {
	return tx.ExecContext(ctx, `INSERT INTO assignments(id,gateway_id,agent_id,generation,state,public_endpoint,obfuscation_mode,obfuscation_key_ciphertext,obfuscation_previous_key_ciphertext,obfuscation_key_id,obfuscation_previous_key_id,obfuscation_max_padding_bytes,obfuscation_handshake_shaping,revision,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, assignment.ID, assignment.GatewayID, assignment.AgentID, assignment.Generation, assignment.State, assignment.PublicEndpoint, assignment.Obfuscation.Mode, nullableBytes(assignment.Obfuscation.KeyCiphertext), nullableBytes(assignment.Obfuscation.PreviousKeyCiphertext), assignment.Obfuscation.KeyID, assignment.Obfuscation.PreviousKeyID, assignment.Obfuscation.MaxPaddingBytes, boolInt(assignment.Obfuscation.HandshakeShaping), assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano))
}

func replaceAssignmentChildrenTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_services WHERE assignment_id=?`, assignment.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_bindings WHERE assignment_id=?`, assignment.ID); err != nil {
		return err
	}
	for position, serviceID := range assignment.ServiceIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_services(assignment_id,position,service_id) VALUES(?,?,?)`, assignment.ID, position, serviceID); err != nil {
			if isUniqueConstraint(err) {
				return &domain.ApplyError{Code: "resource_conflict", Path: "service_ids", Message: fmt.Sprintf("service %q is already assigned", serviceID)}
			}
			return err
		}
	}
	if assignment.State == domain.AssignmentDegraded || assignment.State == domain.AssignmentDraining {
		return nil
	}
	for position, binding := range assignment.Bindings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_bindings(assignment_id,position,service_id,gateway_id,protocol,bind,port) VALUES(?,?,?,?,?,?,?)`, assignment.ID, position, binding.ServiceID, assignment.GatewayID, binding.Protocol, normalizeBind(binding.Bind), binding.Port); err != nil {
			if isUniqueConstraint(err) {
				return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
			}
			return err
		}
	}
	return nil
}

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
	for rows.Next() {
		var position, streams int64
		var session domain.SessionSummary
		var started string
		if err := rows.Scan(&position, &session.ID, &session.PeerID, &started, &streams); err != nil {
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
	for rows.Next() {
		var position, port int64
		var listener domain.ListenerState
		var ready int
		if err := rows.Scan(&position, &listener.Protocol, &listener.Bind, &port, &ready); err != nil {
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
