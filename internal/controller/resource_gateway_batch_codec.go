package controller

import (
	"context"
	"fmt"

	"asterferry/internal/domain"
)

// loadGatewaySpecsBatchNormalized keeps the projection path set-based. It is
// the scheduler-facing counterpart of loadGatewaySpecNormalized and preserves
// the same ordered-child invariants while loading a bounded chunk at a time.
func loadGatewaySpecsBatchNormalized(ctx context.Context, q sqlQueryer, gateways []domain.Node) (map[string]domain.NodeSpec, error) {
	ids := make([]string, 0, len(gateways))
	for _, gateway := range gateways {
		ids = append(ids, gateway.ID)
	}
	result := make(map[string]domain.NodeSpec, len(ids))
	for start := 0; start < len(ids); {
		chunk, args, end := batchIDs(ids, start, gatewayCandidateBatchSize)
		queryArgs := append(args, string(domain.NodeSpecGateway))
		rows, err := q.QueryContext(ctx, `SELECT ns.node_id,ns.kind,ns.revision,ns.updated_at,gs.capacity_max_agents,gs.capacity_max_connections,gs.capacity_max_services,gs.transport_alpn,gs.transport_max_streams,gs.transport_max_frame_bytes,gs.transport_max_datagram_bytes,gs.transport_handshake_timeout_seconds,gs.transport_idle_timeout_seconds,gs.obfuscation_mode,gs.obfuscation_key_ciphertext,gs.obfuscation_previous_key_ciphertext,gs.obfuscation_key_id,gs.obfuscation_previous_key_id,gs.obfuscation_max_padding_bytes,gs.obfuscation_handshake_shaping,gs.egress_enabled,gs.egress_max_connections FROM node_specs ns JOIN gateway_specs gs ON gs.node_id=ns.node_id WHERE ns.node_id IN (`+questionMarks(len(chunk))+`) AND ns.kind=? ORDER BY ns.node_id`, queryArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var nodeID, kind, updated, alpn, mode, keyID, previousKeyID string
			var revision int64
			var capacityAgents, capacityConnections, capacityServices int64
			var maxStreams, maxFrame, maxDatagram, handshakeTimeout, idleTimeout int64
			var maxPadding, egressMax int64
			var keyCiphertext, previousKeyCiphertext []byte
			var shaping, egressEnabled int
			if err := rows.Scan(&nodeID, &kind, &revision, &updated, &capacityAgents, &capacityConnections, &capacityServices, &alpn, &maxStreams, &maxFrame, &maxDatagram, &handshakeTimeout, &idleTimeout, &mode, &keyCiphertext, &previousKeyCiphertext, &keyID, &previousKeyID, &maxPadding, &shaping, &egressEnabled, &egressMax); err != nil {
				_ = rows.Close()
				return nil, err
			}
			parsed, err := parseStoredTime("node_spec.updated_at", updated)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			spec := domain.GatewaySpec{NodeID: nodeID, Revision: revision, Capacity: domain.Capacity{MaxAgents: int(capacityAgents), MaxConnections: int(capacityConnections), MaxServices: int(capacityServices)}, Transport: domain.TransportPolicy{ALPN: alpn, MaxStreams: int(maxStreams), MaxFrameBytes: int(maxFrame), MaxDatagramBytes: int(maxDatagram), HandshakeTimeoutSeconds: int(handshakeTimeout), IdleTimeoutSeconds: int(idleTimeout)}, Obfuscation: domain.ObfuscationPolicy{Mode: mode, KeyCiphertext: append([]byte(nil), keyCiphertext...), PreviousKeyCiphertext: append([]byte(nil), previousKeyCiphertext...), KeyID: keyID, PreviousKeyID: previousKeyID, MaxPaddingBytes: int(maxPadding), HandshakeShaping: shaping != 0}, Egress: domain.EgressPolicy{Enabled: egressEnabled != 0, MaxConnections: int(egressMax)}}
			result[nodeID] = domain.NodeSpec{NodeID: nodeID, Kind: domain.NodeSpecKind(kind), Revision: revision, UpdatedAt: parsed, Gateway: &spec}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		labelsByNode, err := loadStringMapsForIDs(ctx, q, "gateway_labels", "node_id", chunk)
		if err != nil {
			return nil, err
		}
		for nodeID, labels := range labelsByNode {
			if spec, ok := result[nodeID]; ok && spec.Gateway != nil {
				spec.Gateway.Labels = labels
				result[nodeID] = spec
			}
		}
		if err := loadGatewayBatchEndpoints(ctx, q, args, chunk, result); err != nil {
			return nil, err
		}
		if err := loadGatewayBatchListeners(ctx, q, args, chunk, result); err != nil {
			return nil, err
		}
		if err := loadGatewayBatchRanges(ctx, q, args, chunk, result); err != nil {
			return nil, err
		}
		if err := loadGatewayBatchEgress(ctx, q, args, chunk, result); err != nil {
			return nil, err
		}
		start = end
	}
	for nodeID, spec := range result {
		if spec.Gateway == nil {
			delete(result, nodeID)
			continue
		}
		if err := spec.Validate(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func loadGatewayBatchEndpoints(ctx context.Context, q sqlQueryer, args []any, chunk []string, result map[string]domain.NodeSpec) error {
	rows, err := q.QueryContext(ctx, `SELECT node_id,position,endpoint FROM gateway_endpoints WHERE node_id IN (`+questionMarks(len(chunk))+`) ORDER BY node_id,position`, args...)
	if err != nil {
		return err
	}
	positions := make(map[string]int)
	for rows.Next() {
		var nodeID, endpoint string
		var position int64
		if err := rows.Scan(&nodeID, &position, &endpoint); err != nil {
			_ = rows.Close()
			return err
		}
		expected := positions[nodeID]
		if err := requireStoredPosition(position, expected, "gateway endpoint"); err != nil {
			_ = rows.Close()
			return err
		}
		positions[nodeID] = expected + 1
		if spec, ok := result[nodeID]; ok && spec.Gateway != nil {
			spec.Gateway.PublicEndpoints = append(spec.Gateway.PublicEndpoints, endpoint)
			result[nodeID] = spec
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}

func loadGatewayBatchListeners(ctx context.Context, q sqlQueryer, args []any, chunk []string, result map[string]domain.NodeSpec) error {
	rows, err := q.QueryContext(ctx, `SELECT node_id,position,protocol,bind,port,enabled FROM gateway_listeners WHERE node_id IN (`+questionMarks(len(chunk))+`) ORDER BY node_id,position`, args...)
	if err != nil {
		return err
	}
	positions := make(map[string]int)
	for rows.Next() {
		var nodeID, protocol, bind string
		var position, port int64
		var enabled int
		if err := rows.Scan(&nodeID, &position, &protocol, &bind, &port, &enabled); err != nil {
			_ = rows.Close()
			return err
		}
		expected := positions[nodeID]
		if err := requireStoredPosition(position, expected, "gateway listener"); err != nil {
			_ = rows.Close()
			return err
		}
		positions[nodeID] = expected + 1
		listenerPort, err := storedUint16(port, "gateway listener port")
		if err != nil {
			_ = rows.Close()
			return err
		}
		if spec, ok := result[nodeID]; ok && spec.Gateway != nil {
			spec.Gateway.Listeners = append(spec.Gateway.Listeners, domain.Listener{Protocol: protocol, Bind: bind, Port: listenerPort, Enabled: enabled != 0})
			result[nodeID] = spec
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}

func loadGatewayBatchRanges(ctx context.Context, q sqlQueryer, args []any, chunk []string, result map[string]domain.NodeSpec) error {
	rows, err := q.QueryContext(ctx, `SELECT node_id,protocol,position,min_port,max_port FROM gateway_port_ranges WHERE node_id IN (`+questionMarks(len(chunk))+`) ORDER BY node_id,protocol,position`, args...)
	if err != nil {
		return err
	}
	positions := make(map[string]int)
	for rows.Next() {
		var nodeID, protocol string
		var position, minPort, maxPort int64
		if err := rows.Scan(&nodeID, &protocol, &position, &minPort, &maxPort); err != nil {
			_ = rows.Close()
			return err
		}
		if protocol != domain.ProtocolTCP && protocol != domain.ProtocolUDP {
			_ = rows.Close()
			return fmt.Errorf("stored gateway port range protocol %q is invalid", protocol)
		}
		key := nodeID + "\x00" + protocol
		expected := positions[key]
		if err := requireStoredPosition(position, expected, "gateway "+protocol+" port range"); err != nil {
			_ = rows.Close()
			return err
		}
		positions[key] = expected + 1
		minValue, err := storedUint16(minPort, "gateway port range minimum")
		if err != nil {
			_ = rows.Close()
			return err
		}
		maxValue, err := storedUint16(maxPort, "gateway port range maximum")
		if err != nil {
			_ = rows.Close()
			return err
		}
		if spec, ok := result[nodeID]; ok && spec.Gateway != nil {
			rangeValue := domain.PortRange{Min: minValue, Max: maxValue}
			if protocol == domain.ProtocolUDP {
				spec.Gateway.PortPool.UDP = append(spec.Gateway.PortPool.UDP, rangeValue)
			} else {
				spec.Gateway.PortPool.TCP = append(spec.Gateway.PortPool.TCP, rangeValue)
			}
			result[nodeID] = spec
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}

func loadGatewayBatchEgress(ctx context.Context, q sqlQueryer, args []any, chunk []string, result map[string]domain.NodeSpec) error {
	rows, err := q.QueryContext(ctx, `SELECT node_id,kind,position,value FROM gateway_egress_values WHERE node_id IN (`+questionMarks(len(chunk))+`) ORDER BY node_id,kind,position`, args...)
	if err != nil {
		return err
	}
	positions := make(map[string]int)
	for rows.Next() {
		var nodeID, kind, value string
		var position int64
		if err := rows.Scan(&nodeID, &kind, &position, &value); err != nil {
			_ = rows.Close()
			return err
		}
		if err := requireStoredKind(kind, "gateway egress", "tcp_ports", "udp_ports", "allow_cidrs", "allow_special_cidrs"); err != nil {
			_ = rows.Close()
			return err
		}
		key := nodeID + "\x00" + kind
		expected := positions[key]
		if err := requireStoredPosition(position, expected, "gateway "+kind+" egress value"); err != nil {
			_ = rows.Close()
			return err
		}
		positions[key] = expected + 1
		if spec, ok := result[nodeID]; ok && spec.Gateway != nil {
			switch kind {
			case "tcp_ports":
				spec.Gateway.Egress.TCPPorts = append(spec.Gateway.Egress.TCPPorts, value)
			case "udp_ports":
				spec.Gateway.Egress.UDPPorts = append(spec.Gateway.Egress.UDPPorts, value)
			case "allow_cidrs":
				spec.Gateway.Egress.AllowCIDRs = append(spec.Gateway.Egress.AllowCIDRs, value)
			case "allow_special_cidrs":
				spec.Gateway.Egress.AllowSpecialCIDRs = append(spec.Gateway.Egress.AllowSpecialCIDRs, value)
			}
			result[nodeID] = spec
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}
