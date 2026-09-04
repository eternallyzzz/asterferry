package controller

import (
	"context"
	"database/sql"
	"fmt"

	"asterferry/internal/domain"
)

// GatewaySpec is a normalized aggregate: the scalar row is accompanied by
// ordered endpoint/listener/range/value tables. Loading and replacing the
// complete aggregate stays in this file so its table-count and ordering
// invariants are reviewed together.
func loadGatewaySpecNormalized(ctx context.Context, q sqlQueryer, nodeID string, revision int64) (domain.GatewaySpec, error) {
	var spec domain.GatewaySpec
	spec.NodeID = nodeID
	spec.Revision = revision
	var values [15]int64
	var alpn, mode, keyID, previousKeyID string
	var keyCiphertext, previousKeyCiphertext []byte
	var handshakeShaping, egressEnabled int
	err := q.QueryRowContext(ctx, `SELECT capacity_max_agents,capacity_max_connections,capacity_max_services,transport_alpn,transport_max_streams,transport_max_frame_bytes,transport_max_datagram_bytes,transport_handshake_timeout_seconds,transport_idle_timeout_seconds,obfuscation_mode,obfuscation_key_ciphertext,obfuscation_previous_key_ciphertext,obfuscation_key_id,obfuscation_previous_key_id,obfuscation_max_padding_bytes,obfuscation_handshake_shaping,egress_enabled,egress_max_connections FROM gateway_specs WHERE node_id=?`, nodeID).Scan(
		&values[0], &values[1], &values[2], &alpn, &values[3], &values[4], &values[5], &values[6], &values[7], &mode, &keyCiphertext, &previousKeyCiphertext, &keyID, &previousKeyID, &values[8], &handshakeShaping, &egressEnabled, &values[9])
	if err != nil {
		return domain.GatewaySpec{}, err
	}
	spec.Capacity = domain.Capacity{MaxAgents: int(values[0]), MaxConnections: int(values[1]), MaxServices: int(values[2])}
	spec.Transport = domain.TransportPolicy{ALPN: alpn, MaxStreams: int(values[3]), MaxFrameBytes: int(values[4]), MaxDatagramBytes: int(values[5]), HandshakeTimeoutSeconds: int(values[6]), IdleTimeoutSeconds: int(values[7])}
	spec.Obfuscation = domain.ObfuscationPolicy{Mode: mode, KeyCiphertext: append([]byte(nil), keyCiphertext...), PreviousKeyCiphertext: append([]byte(nil), previousKeyCiphertext...), KeyID: keyID, PreviousKeyID: previousKeyID, MaxPaddingBytes: int(values[8]), HandshakeShaping: handshakeShaping != 0}
	spec.Egress.Enabled = egressEnabled != 0
	spec.Egress.MaxConnections = int(values[9])

	rows, err := q.QueryContext(ctx, `SELECT position,endpoint FROM gateway_endpoints WHERE node_id=? ORDER BY position`, nodeID)
	if err != nil {
		return domain.GatewaySpec{}, err
	}
	for expected := 0; rows.Next(); expected++ {
		var position int64
		var endpoint string
		if err := rows.Scan(&position, &endpoint); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		if err := requireStoredPosition(position, expected, "gateway endpoint"); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		spec.PublicEndpoints = append(spec.PublicEndpoints, endpoint)
	}
	if err := rows.Close(); err != nil {
		return domain.GatewaySpec{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.GatewaySpec{}, err
	}
	spec.Labels, err = loadStringMap(ctx, q, "gateway_labels", "node_id", nodeID)
	if err != nil {
		return domain.GatewaySpec{}, err
	}

	rows, err = q.QueryContext(ctx, `SELECT position,protocol,bind,port,enabled FROM gateway_listeners WHERE node_id=? ORDER BY position`, nodeID)
	if err != nil {
		return domain.GatewaySpec{}, err
	}
	for expected := 0; rows.Next(); expected++ {
		var position, port int64
		var protocol, bind string
		var enabled int
		if err := rows.Scan(&position, &protocol, &bind, &port, &enabled); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		if err := requireStoredPosition(position, expected, "gateway listener"); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		listenerPort, err := storedUint16(port, "gateway listener port")
		if err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		spec.Listeners = append(spec.Listeners, domain.Listener{Protocol: protocol, Bind: bind, Port: listenerPort, Enabled: enabled != 0})
	}
	if err := rows.Close(); err != nil {
		return domain.GatewaySpec{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.GatewaySpec{}, err
	}

	rows, err = q.QueryContext(ctx, `SELECT protocol,position,min_port,max_port FROM gateway_port_ranges WHERE node_id=? ORDER BY protocol,position`, nodeID)
	if err != nil {
		return domain.GatewaySpec{}, err
	}
	rangePositions := make(map[string]int)
	for rows.Next() {
		var protocol string
		var position, minPort, maxPort int64
		if err := rows.Scan(&protocol, &position, &minPort, &maxPort); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		if protocol != domain.ProtocolTCP && protocol != domain.ProtocolUDP {
			_ = rows.Close()
			return domain.GatewaySpec{}, fmt.Errorf("stored gateway port range protocol %q is invalid", protocol)
		}
		expected := rangePositions[protocol]
		if err := requireStoredPosition(position, expected, "gateway "+protocol+" port range"); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		rangePositions[protocol] = expected + 1
		minValue, err := storedUint16(minPort, "gateway port range minimum")
		if err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		maxValue, err := storedUint16(maxPort, "gateway port range maximum")
		if err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		rangeValue := domain.PortRange{Min: minValue, Max: maxValue}
		if protocol == domain.ProtocolUDP {
			spec.PortPool.UDP = append(spec.PortPool.UDP, rangeValue)
		} else {
			spec.PortPool.TCP = append(spec.PortPool.TCP, rangeValue)
		}
	}
	if err := rows.Close(); err != nil {
		return domain.GatewaySpec{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.GatewaySpec{}, err
	}

	rows, err = q.QueryContext(ctx, `SELECT kind,position,value FROM gateway_egress_values WHERE node_id=? ORDER BY kind,position`, nodeID)
	if err != nil {
		return domain.GatewaySpec{}, err
	}
	egressPositions := make(map[string]int)
	for rows.Next() {
		var kind, value string
		var position int64
		if err := rows.Scan(&kind, &position, &value); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		if err := requireStoredKind(kind, "gateway egress", "tcp_ports", "udp_ports", "allow_cidrs", "allow_special_cidrs"); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		expected := egressPositions[kind]
		if err := requireStoredPosition(position, expected, "gateway "+kind+" egress value"); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
		egressPositions[kind] = expected + 1
		switch kind {
		case "tcp_ports":
			spec.Egress.TCPPorts = append(spec.Egress.TCPPorts, value)
		case "udp_ports":
			spec.Egress.UDPPorts = append(spec.Egress.UDPPorts, value)
		case "allow_cidrs":
			spec.Egress.AllowCIDRs = append(spec.Egress.AllowCIDRs, value)
		case "allow_special_cidrs":
			spec.Egress.AllowSpecialCIDRs = append(spec.Egress.AllowSpecialCIDRs, value)
		}
	}
	if err := rows.Close(); err != nil {
		return domain.GatewaySpec{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.GatewaySpec{}, err
	}
	if err := spec.Validate(); err != nil {
		return domain.GatewaySpec{}, fmt.Errorf("stored gateway spec is invalid: %w", err)
	}
	return spec, nil
}

func replaceGatewaySpecTx(ctx context.Context, tx *sql.Tx, spec domain.GatewaySpec) error {
	for _, table := range []string{"gateway_endpoints", "gateway_labels", "gateway_listeners", "gateway_port_ranges", "gateway_egress_values"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE node_id=?`, spec.NodeID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_specs(node_id,capacity_max_agents,capacity_max_connections,capacity_max_services,transport_alpn,transport_max_streams,transport_max_frame_bytes,transport_max_datagram_bytes,transport_handshake_timeout_seconds,transport_idle_timeout_seconds,obfuscation_mode,obfuscation_key_ciphertext,obfuscation_previous_key_ciphertext,obfuscation_key_id,obfuscation_previous_key_id,obfuscation_max_padding_bytes,obfuscation_handshake_shaping,egress_enabled,egress_max_connections) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET capacity_max_agents=excluded.capacity_max_agents,capacity_max_connections=excluded.capacity_max_connections,capacity_max_services=excluded.capacity_max_services,transport_alpn=excluded.transport_alpn,transport_max_streams=excluded.transport_max_streams,transport_max_frame_bytes=excluded.transport_max_frame_bytes,transport_max_datagram_bytes=excluded.transport_max_datagram_bytes,transport_handshake_timeout_seconds=excluded.transport_handshake_timeout_seconds,transport_idle_timeout_seconds=excluded.transport_idle_timeout_seconds,obfuscation_mode=excluded.obfuscation_mode,obfuscation_key_ciphertext=excluded.obfuscation_key_ciphertext,obfuscation_previous_key_ciphertext=excluded.obfuscation_previous_key_ciphertext,obfuscation_key_id=excluded.obfuscation_key_id,obfuscation_previous_key_id=excluded.obfuscation_previous_key_id,obfuscation_max_padding_bytes=excluded.obfuscation_max_padding_bytes,obfuscation_handshake_shaping=excluded.obfuscation_handshake_shaping,egress_enabled=excluded.egress_enabled,egress_max_connections=excluded.egress_max_connections`,
		spec.NodeID, spec.Capacity.MaxAgents, spec.Capacity.MaxConnections, spec.Capacity.MaxServices, spec.Transport.ALPN, spec.Transport.MaxStreams, spec.Transport.MaxFrameBytes, spec.Transport.MaxDatagramBytes, spec.Transport.HandshakeTimeoutSeconds, spec.Transport.IdleTimeoutSeconds, spec.Obfuscation.Mode, nullableBytes(spec.Obfuscation.KeyCiphertext), nullableBytes(spec.Obfuscation.PreviousKeyCiphertext), spec.Obfuscation.KeyID, spec.Obfuscation.PreviousKeyID, spec.Obfuscation.MaxPaddingBytes, boolInt(spec.Obfuscation.HandshakeShaping), boolInt(spec.Egress.Enabled), spec.Egress.MaxConnections); err != nil {
		return err
	}
	for position, endpoint := range spec.PublicEndpoints {
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_endpoints(node_id,position,endpoint) VALUES(?,?,?)`, spec.NodeID, position, endpoint); err != nil {
			return err
		}
	}
	if err := replaceStringMapTx(ctx, tx, "gateway_labels", "node_id", spec.NodeID, spec.Labels); err != nil {
		return err
	}
	for position, listener := range spec.Listeners {
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_listeners(node_id,position,protocol,bind,port,enabled) VALUES(?,?,?,?,?,?)`, spec.NodeID, position, listener.Protocol, normalizeBind(listener.Bind), listener.Port, boolInt(listener.Enabled)); err != nil {
			return err
		}
	}
	for _, protocol := range []string{domain.ProtocolTCP, domain.ProtocolUDP} {
		ranges := spec.PortPool.TCP
		if protocol == domain.ProtocolUDP {
			ranges = spec.PortPool.UDP
		}
		for position, portRange := range ranges {
			if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_port_ranges(node_id,protocol,position,min_port,max_port) VALUES(?,?,?,?,?)`, spec.NodeID, protocol, position, portRange.Min, portRange.Max); err != nil {
				return err
			}
		}
	}
	for _, valuesByKind := range []struct {
		kind   string
		values []string
	}{
		{kind: "tcp_ports", values: spec.Egress.TCPPorts},
		{kind: "udp_ports", values: spec.Egress.UDPPorts},
		{kind: "allow_cidrs", values: spec.Egress.AllowCIDRs},
		{kind: "allow_special_cidrs", values: spec.Egress.AllowSpecialCIDRs},
	} {
		for position, value := range valuesByKind.values {
			if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_egress_values(node_id,kind,position,value) VALUES(?,?,?,?)`, spec.NodeID, valuesByKind.kind, position, value); err != nil {
				return err
			}
		}
	}
	return nil
}
