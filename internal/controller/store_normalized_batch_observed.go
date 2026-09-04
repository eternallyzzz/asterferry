package controller

import (
	"context"
	"fmt"
	"time"

	"asterferry/internal/domain"
)

func loadObservedBatchNormalized(ctx context.Context, q sqlQueryer, gateways []domain.Node) (map[string]domain.ObservedState, error) {
	nodeIDs := make([]string, 0, len(gateways))
	for _, gateway := range gateways {
		nodeIDs = append(nodeIDs, gateway.ID)
	}
	result := make(map[string]domain.ObservedState, len(nodeIDs))
	for start := 0; start < len(nodeIDs); {
		chunk, args, end := batchIDs(nodeIDs, start, gatewayCandidateBatchSize)
		rows, err := q.QueryContext(ctx, `SELECT node_id,generation,protocol_version,healthy,degraded,last_error_code,last_error_path,last_error_message,observed_at,updated_at,active_streams,active_sessions,active_egress,udp_oversize_drops,geoip_up,active_connections,active_flows,runtime_bytes_in_total,runtime_bytes_out_total,runtime_opened_total,runtime_closed_total,runtime_rejected_total,runtime_rate_limited_total,runtime_telemetry_dropped_total FROM observed_states WHERE node_id IN (`+questionMarks(len(chunk))+`) ORDER BY node_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var state domain.ObservedState
			var generation int64
			var protocolVersion, healthy, degraded, geoipUp int
			var lastCode, lastPath, lastMessage, observedAt, updatedAt string
			var metrics [13]int64
			if err := rows.Scan(&state.NodeID, &generation, &protocolVersion, &healthy, &degraded, &lastCode, &lastPath, &lastMessage, &observedAt, &updatedAt, &metrics[0], &metrics[1], &metrics[2], &metrics[3], &geoipUp, &metrics[4], &metrics[5], &metrics[6], &metrics[7], &metrics[8], &metrics[9], &metrics[10], &metrics[11], &metrics[12]); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if protocolVersion <= 0 {
				_ = rows.Close()
				return nil, fmt.Errorf("stored observed protocol version is invalid")
			}
			state.SchemaVersion = uint32(protocolVersion)
			state.AppliedGeneration, err = storedUint64(generation, "observed generation")
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			state.Healthy = healthy != 0
			state.Degraded = degraded != 0
			state.ObservedAt, err = parseStoredTime("observed.observed_at", observedAt)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			if lastCode != "" {
				state.LastError = &domain.ApplyError{Code: lastCode, Path: lastPath, Message: lastMessage}
			}
			metricValues := make([]uint64, len(metrics))
			for index, value := range metrics {
				metricValues[index], err = storedUint64(value, fmt.Sprintf("observed metric %d", index))
				if err != nil {
					_ = rows.Close()
					return nil, err
				}
			}
			state.Metrics = domain.RuntimeMetrics{ActiveStreams: metricValues[0], ActiveSessions: metricValues[1], ActiveEgress: metricValues[2], UDPOversizeDrops: metricValues[3], GeoIPUp: geoipUp != 0, ActiveConnections: metricValues[4], ActiveFlows: metricValues[5], RuntimeBytesInTotal: metricValues[6], RuntimeBytesOutTotal: metricValues[7], RuntimeOpenedTotal: metricValues[8], RuntimeClosedTotal: metricValues[9], RuntimeRejectedTotal: metricValues[10], RuntimeRateLimitedTotal: metricValues[11], RuntimeTelemetryDroppedTotal: metricValues[12]}
			if _, err := parseStoredTime("observed.updated_at", updatedAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			result[state.NodeID] = state
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		idsWithStates := make([]string, 0, len(result))
		for _, id := range chunk {
			if _, ok := result[id]; ok {
				idsWithStates = append(idsWithStates, id)
			}
		}
		if len(idsWithStates) > 0 {
			childArgs := make([]any, len(idsWithStates))
			for index, id := range idsWithStates {
				childArgs[index] = id
			}
			rows, err = q.QueryContext(ctx, `SELECT node_id,position,id,peer_id,started_at,streams FROM observed_sessions WHERE node_id IN (`+questionMarks(len(idsWithStates))+`) ORDER BY node_id,position`, childArgs...)
			if err != nil {
				return nil, err
			}
			sessionPositions := make(map[string]int)
			for rows.Next() {
				var nodeID, started string
				var position, streams int64
				var session domain.SessionSummary
				if err := rows.Scan(&nodeID, &position, &session.ID, &session.PeerID, &started, &streams); err != nil {
					_ = rows.Close()
					return nil, err
				}
				expected := sessionPositions[nodeID]
				if err := requireStoredPosition(position, expected, "observed session"); err != nil {
					_ = rows.Close()
					return nil, err
				}
				sessionPositions[nodeID] = expected + 1
				session.StartedAt, err = parseStoredTime("observed.session.started_at", started)
				if err != nil {
					_ = rows.Close()
					return nil, err
				}
				if streams < 0 {
					_ = rows.Close()
					return nil, fmt.Errorf("stored observed session stream count is negative")
				}
				session.Streams = int(streams)
				state := result[nodeID]
				state.Sessions = append(state.Sessions, session)
				result[nodeID] = state
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
			rows, err = q.QueryContext(ctx, `SELECT node_id,position,protocol,bind,port,ready FROM observed_listeners WHERE node_id IN (`+questionMarks(len(idsWithStates))+`) ORDER BY node_id,position`, childArgs...)
			if err != nil {
				return nil, err
			}
			listenerPositions := make(map[string]int)
			for rows.Next() {
				var nodeID, protocol, bind string
				var position, port int64
				var ready int
				if err := rows.Scan(&nodeID, &position, &protocol, &bind, &port, &ready); err != nil {
					_ = rows.Close()
					return nil, err
				}
				expected := listenerPositions[nodeID]
				if err := requireStoredPosition(position, expected, "observed listener"); err != nil {
					_ = rows.Close()
					return nil, err
				}
				listenerPositions[nodeID] = expected + 1
				listenerPort, err := storedUint16(port, "observed listener port")
				if err != nil {
					_ = rows.Close()
					return nil, err
				}
				state := result[nodeID]
				state.Listeners = append(state.Listeners, domain.ListenerState{Protocol: protocol, Bind: bind, Port: listenerPort, Ready: ready != 0})
				result[nodeID] = state
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
		for _, id := range idsWithStates {
			state := result[id]
			if err := state.Validate(); err != nil {
				return nil, err
			}
			if !state.ObservedAt.IsZero() && state.ObservedAt.After(time.Now().UTC().Add(5*time.Minute)) {
				return nil, &domain.ApplyError{Code: "invalid_observed_state", Path: "observed_at", Message: "observed timestamp is too far in the future"}
			}
		}
		start = end
	}
	return result, nil
}
