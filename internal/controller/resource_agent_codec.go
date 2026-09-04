package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"asterferry/internal/domain"
)

// AgentSpec owns the proxy, route, route-value, selector, and egress child
// tables for an agent aggregate. The loader validates every position because
// positions are the durable representation of user-visible list order.
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
	for expected := 0; rows.Next(); expected++ {
		var position int64
		var proxy domain.ProxySpec
		var enabled int
		if err := rows.Scan(&position, &proxy.ID, &proxy.Protocol, &proxy.Bind, &proxy.Route, &enabled); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
		}
		if err := requireStoredPosition(position, expected, "agent proxy"); err != nil {
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
	for expected := 0; rows.Next(); expected++ {
		var position int64
		var route domain.RouteRule
		var enabled int
		if err := rows.Scan(&position, &route.Name, &route.Destination, &enabled); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
		}
		if err := requireStoredPosition(position, expected, "agent route"); err != nil {
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
	valuePositions := make(map[string]int)
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
		if err := requireStoredKind(kind, "agent route value", "cidrs", "domains", "geoip"); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
		}
		key := fmt.Sprintf("%d:%s", routePosition, kind)
		expected := valuePositions[key]
		if err := requireStoredPosition(position, expected, "agent "+kind+" route value"); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
		}
		valuePositions[key] = expected + 1
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
	egressPositions := make(map[string]int)
	for rows.Next() {
		var kind, value string
		var position int64
		if err := rows.Scan(&kind, &position, &value); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
		}
		if err := requireStoredKind(kind, "agent egress", "tcp_ports", "udp_ports", "allow_cidrs", "allow_special_cidrs"); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
		}
		expected := egressPositions[kind]
		if err := requireStoredPosition(position, expected, "agent "+kind+" egress value"); err != nil {
			_ = rows.Close()
			return domain.AgentSpec{}, err
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
		for _, valuesByKind := range []struct {
			kind   string
			values []string
		}{
			{kind: "cidrs", values: route.CIDRs},
			{kind: "domains", values: route.Domains},
			{kind: "geoip", values: route.GeoIP},
		} {
			for valuePosition, value := range valuesByKind.values {
				if _, err := tx.ExecContext(ctx, `INSERT INTO agent_route_values(node_id,route_position,kind,position,value) VALUES(?,?,?,?,?)`, spec.NodeID, position, valuesByKind.kind, valuePosition, value); err != nil {
					return err
				}
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
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_egress_values(node_id,kind,position,value) VALUES(?,?,?,?)`, spec.NodeID, valuesByKind.kind, position, value); err != nil {
				return err
			}
		}
	}
	return nil
}
