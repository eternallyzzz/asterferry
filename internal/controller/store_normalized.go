package controller

// This file is the relational aggregate codec for the Controller repository.
// The domain documents remain the API and wire representation, but business
// fields are stored in typed columns and ordered child tables. JSON is kept
// only for explicitly opaque payloads such as snapshots, audit attributes and
// one-time bootstrap intent.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"asterferry/internal/domain"
)

type sqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func storedUint64(value int64, field string) (uint64, error) {
	if value < 0 {
		return 0, fmt.Errorf("stored %s is negative", field)
	}
	return uint64(value), nil
}

func storedUint16(value int64, field string) (uint16, error) {
	if value <= 0 || value > 65535 {
		return 0, fmt.Errorf("stored %s is outside the uint16 range", field)
	}
	return uint16(value), nil
}

func storedPort(value int64, field string) (uint16, error) {
	if value < 0 || value > 65535 {
		return 0, fmt.Errorf("stored %s is outside the uint16 range", field)
	}
	return uint16(value), nil
}

func loadNodeLabels(ctx context.Context, q sqlQueryer, nodeID string) (map[string]string, error) {
	return loadStringMap(ctx, q, "node_labels", "node_id", nodeID)
}

func loadNodeLabelsForIDs(ctx context.Context, q sqlQueryer, nodeIDs []string) (map[string]map[string]string, error) {
	result := make(map[string]map[string]string, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return result, nil
	}
	for start := 0; start < len(nodeIDs); start += gatewayCandidateBatchSize {
		end := start + gatewayCandidateBatchSize
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		args := make([]any, 0, end-start)
		for _, nodeID := range nodeIDs[start:end] {
			args = append(args, nodeID)
		}
		rows, err := q.QueryContext(ctx, `SELECT node_id,key,value FROM node_labels WHERE node_id IN (`+questionMarks(end-start)+`) ORDER BY node_id,key`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var nodeID, key, value string
			if err := rows.Scan(&nodeID, &key, &value); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if result[nodeID] == nil {
				result[nodeID] = make(map[string]string)
			}
			result[nodeID][key] = value
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func loadStringMapsForIDs(ctx context.Context, q sqlQueryer, table, ownerColumn string, ids []string) (map[string]map[string]string, error) {
	result := make(map[string]map[string]string, len(ids))
	for start := 0; start < len(ids); start += gatewayCandidateBatchSize {
		end := start + gatewayCandidateBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		rows, err := q.QueryContext(ctx, `SELECT `+ownerColumn+`,key,value FROM `+table+` WHERE `+ownerColumn+` IN (`+questionMarks(end-start)+`) ORDER BY `+ownerColumn+`,key`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var owner, key, value string
			if err := rows.Scan(&owner, &key, &value); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if result[owner] == nil {
				result[owner] = make(map[string]string)
			}
			result[owner][key] = value
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func loadNodeIdentity(ctx context.Context, q sqlQueryer, nodeID string) (domain.Node, error) {
	var node domain.Node
	var enabled int
	var created, updated string
	if err := q.QueryRowContext(ctx, `SELECT id,name,enabled,certificate_state,certificate_serial,revision,created_at,updated_at FROM nodes WHERE id=?`, nodeID).Scan(&node.ID, &node.Name, &enabled, &node.CertificateState, &node.CertificateSerial, &node.Revision, &created, &updated); err != nil {
		return domain.Node{}, err
	}
	var err error
	node.Labels, err = loadNodeLabels(ctx, q, node.ID)
	if err != nil {
		return domain.Node{}, err
	}
	node.Enabled = enabled != 0
	node.CreatedAt, err = parseStoredTime("node.created_at", created)
	if err != nil {
		return domain.Node{}, err
	}
	node.UpdatedAt, err = parseStoredTime("node.updated_at", updated)
	if err != nil {
		return domain.Node{}, err
	}
	if err := node.Validate(); err != nil {
		return domain.Node{}, fmt.Errorf("stored node is invalid: %w", err)
	}
	return node, nil
}

func insertNodeLabelsTx(ctx context.Context, tx *sql.Tx, nodeID string, labels map[string]string) error {
	return replaceStringMapTx(ctx, tx, "node_labels", "node_id", nodeID, labels)
}

func loadStringMap(ctx context.Context, q sqlQueryer, table, ownerColumn, owner string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT key,value FROM `+table+` WHERE `+ownerColumn+`=? ORDER BY key`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, rows.Err()
}

func replaceStringMapTx(ctx context.Context, tx *sql.Tx, table, ownerColumn, owner string, values map[string]string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+ownerColumn+`=?`, owner); err != nil {
		return err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(`+ownerColumn+`,key,value) VALUES(?,?,?)`, owner, key, values[key]); err != nil {
			return err
		}
	}
	return nil
}

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
	for rows.Next() {
		var position int64
		var endpoint string
		if err := rows.Scan(&position, &endpoint); err != nil {
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
	for rows.Next() {
		var position, port int64
		var protocol, bind string
		var enabled int
		if err := rows.Scan(&position, &protocol, &bind, &port, &enabled); err != nil {
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
	for rows.Next() {
		var protocol string
		var position, minPort, maxPort int64
		if err := rows.Scan(&protocol, &position, &minPort, &maxPort); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
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
	for rows.Next() {
		var kind, value string
		var position int64
		if err := rows.Scan(&kind, &position, &value); err != nil {
			_ = rows.Close()
			return domain.GatewaySpec{}, err
		}
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_endpoints WHERE node_id=?`, spec.NodeID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_labels WHERE node_id=?`, spec.NodeID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_listeners WHERE node_id=?`, spec.NodeID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_port_ranges WHERE node_id=?`, spec.NodeID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_egress_values WHERE node_id=?`, spec.NodeID); err != nil {
		return err
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
	for protocol, ranges := range map[string][]domain.PortRange{domain.ProtocolTCP: spec.PortPool.TCP, domain.ProtocolUDP: spec.PortPool.UDP} {
		for position, portRange := range ranges {
			if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_port_ranges(node_id,protocol,position,min_port,max_port) VALUES(?,?,?,?,?)`, spec.NodeID, protocol, position, portRange.Min, portRange.Max); err != nil {
				return err
			}
		}
	}
	for kind, values := range map[string][]string{"tcp_ports": spec.Egress.TCPPorts, "udp_ports": spec.Egress.UDPPorts, "allow_cidrs": spec.Egress.AllowCIDRs, "allow_special_cidrs": spec.Egress.AllowSpecialCIDRs} {
		for position, value := range values {
			if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_egress_values(node_id,kind,position,value) VALUES(?,?,?,?)`, spec.NodeID, kind, position, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadAgentSpecNormalized(ctx context.Context, q sqlQueryer, nodeID string, revision int64) (domain.AgentSpec, error) {
	var spec domain.AgentSpec
	spec.NodeID = nodeID
	spec.Revision = revision
	var maxConnections, maxStreams, maxBuffer, egressMax int64
	var loggingLevel, loggingFormat string
	var egressEnabled int
	if err := q.QueryRowContext(ctx, `SELECT limits_max_connections,limits_max_streams,limits_max_buffer_bytes,logging_level,logging_format,egress_enabled,egress_max_connections FROM agent_specs WHERE node_id=?`, nodeID).Scan(&maxConnections, &maxStreams, &maxBuffer, &loggingLevel, &loggingFormat, &egressEnabled, &egressMax); err != nil {
		return domain.AgentSpec{}, err
	}
	spec.Limits = domain.AgentLimits{MaxConnections: int(maxConnections), MaxStreams: int(maxStreams), MaxBufferBytes: int(maxBuffer)}
	spec.Logging = domain.LoggingPolicy{Level: loggingLevel, Format: loggingFormat}
	spec.Egress.Enabled = egressEnabled != 0
	spec.Egress.MaxConnections = int(egressMax)
	var err error
	spec.GatewaySelector.MatchLabels, err = loadStringMap(ctx, q, "agent_selector_labels", "node_id", nodeID)
	if err != nil {
		return domain.AgentSpec{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT position,id,protocol,bind,route,enabled FROM agent_proxies WHERE node_id=? ORDER BY position`, nodeID)
	if err != nil {
		return domain.AgentSpec{}, err
	}
	for rows.Next() {
		var position int64
		var proxy domain.ProxySpec
		var enabled int
		if err := rows.Scan(&position, &proxy.ID, &proxy.Protocol, &proxy.Bind, &proxy.Route, &enabled); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
		}
		proxy.Enabled = enabled != 0
		spec.Proxies = append(spec.Proxies, proxy)
	}
	if err := rows.Close(); err != nil {
		return domain.AgentSpec{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.AgentSpec{}, err
	}
	rows, err = q.QueryContext(ctx, `SELECT position,name,destination,enabled FROM agent_routes WHERE node_id=? ORDER BY position`, nodeID)
	if err != nil {
		return domain.AgentSpec{}, err
	}
	for rows.Next() {
		var position int64
		var route domain.RouteRule
		var enabled int
		if err := rows.Scan(&position, &route.Name, &route.Destination, &enabled); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
		}
		route.Enabled = enabled != 0
		spec.Routes = append(spec.Routes, route)
	}
	if err := rows.Close(); err != nil {
		return domain.AgentSpec{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.AgentSpec{}, err
	}
	rows, err = q.QueryContext(ctx, `SELECT route_position,kind,position,value FROM agent_route_values WHERE node_id=? ORDER BY route_position,kind,position`, nodeID)
	if err != nil {
		return domain.AgentSpec{}, err
	}
	for rows.Next() {
		var routePosition, position int64
		var kind, value string
		if err := rows.Scan(&routePosition, &kind, &position, &value); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
		}
		if routePosition < 0 || routePosition >= int64(len(spec.Routes)) {
			_ = rows.Close()
			return domain.AgentSpec{}, errors.New("stored agent route position is invalid")
		}
		route := &spec.Routes[routePosition]
		switch kind {
		case "cidrs":
			route.CIDRs = append(route.CIDRs, value)
		case "domains":
			route.Domains = append(route.Domains, value)
		case "geoip":
			route.GeoIP = append(route.GeoIP, value)
		}
	}
	if err := rows.Close(); err != nil {
		return domain.AgentSpec{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.AgentSpec{}, err
	}
	rows, err = q.QueryContext(ctx, `SELECT kind,position,value FROM agent_egress_values WHERE node_id=? ORDER BY kind,position`, nodeID)
	if err != nil {
		return domain.AgentSpec{}, err
	}
	for rows.Next() {
		var kind, value string
		var position int64
		if err := rows.Scan(&kind, &position, &value); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
		}
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
		return domain.AgentSpec{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.AgentSpec{}, err
	}
	if err := spec.Validate(); err != nil {
		return domain.AgentSpec{}, fmt.Errorf("stored agent spec is invalid: %w", err)
	}
	return spec, nil
}

func replaceAgentSpecTx(ctx context.Context, tx *sql.Tx, spec domain.AgentSpec) error {
	for _, table := range []string{"agent_selector_labels", "agent_proxies", "agent_routes", "agent_route_values", "agent_egress_values"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE node_id=?`, spec.NodeID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_specs(node_id,limits_max_connections,limits_max_streams,limits_max_buffer_bytes,logging_level,logging_format,egress_enabled,egress_max_connections) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET limits_max_connections=excluded.limits_max_connections,limits_max_streams=excluded.limits_max_streams,limits_max_buffer_bytes=excluded.limits_max_buffer_bytes,logging_level=excluded.logging_level,logging_format=excluded.logging_format,egress_enabled=excluded.egress_enabled,egress_max_connections=excluded.egress_max_connections`, spec.NodeID, spec.Limits.MaxConnections, spec.Limits.MaxStreams, spec.Limits.MaxBufferBytes, spec.Logging.Level, spec.Logging.Format, boolInt(spec.Egress.Enabled), spec.Egress.MaxConnections); err != nil {
		return err
	}
	if err := replaceStringMapTx(ctx, tx, "agent_selector_labels", "node_id", spec.NodeID, spec.GatewaySelector.MatchLabels); err != nil {
		return err
	}
	for position, proxy := range spec.Proxies {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_proxies(node_id,position,id,protocol,bind,route,enabled) VALUES(?,?,?,?,?,?,?)`, spec.NodeID, position, proxy.ID, proxy.Protocol, proxy.Bind, proxy.Route, boolInt(proxy.Enabled)); err != nil {
			return err
		}
	}
	for position, route := range spec.Routes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_routes(node_id,position,name,destination,enabled) VALUES(?,?,?,?,?)`, spec.NodeID, position, route.Name, route.Destination, boolInt(route.Enabled)); err != nil {
			return err
		}
		for kind, values := range map[string][]string{"cidrs": route.CIDRs, "domains": route.Domains, "geoip": route.GeoIP} {
			for valuePosition, value := range values {
				if _, err := tx.ExecContext(ctx, `INSERT INTO agent_route_values(node_id,route_position,kind,position,value) VALUES(?,?,?,?,?)`, spec.NodeID, position, kind, valuePosition, value); err != nil {
					return err
				}
			}
		}
	}
	for kind, values := range map[string][]string{"tcp_ports": spec.Egress.TCPPorts, "udp_ports": spec.Egress.UDPPorts, "allow_cidrs": spec.Egress.AllowCIDRs, "allow_special_cidrs": spec.Egress.AllowSpecialCIDRs} {
		for position, value := range values {
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_egress_values(node_id,kind,position,value) VALUES(?,?,?,?)`, spec.NodeID, kind, position, value); err != nil {
				return err
			}
		}
	}
	return nil
}
