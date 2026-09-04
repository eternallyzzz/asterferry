package controller

import (
	"context"
	"fmt"

	"asterferry/internal/domain"
)

// loadAgentSpecsBatchNormalized is the set-based Agent projection used by
// list and scheduler views. Child positions are checked per node and value
// kind before the aggregate is validated.
func loadAgentSpecsBatchNormalized(ctx context.Context, q sqlQueryer, agents []domain.Node) (map[string]domain.NodeSpec, error) {
	ids := make([]string, 0, len(agents))
	for _, agent := range agents {
		ids = append(ids, agent.ID)
	}
	result := make(map[string]domain.NodeSpec, len(ids))
	for start := 0; start < len(ids); {
		chunk, args, end := batchIDs(ids, start, gatewayCandidateBatchSize)
		queryArgs := append(args, string(domain.NodeSpecAgent))
		rows, err := q.QueryContext(ctx, `SELECT ns.node_id,ns.kind,ns.revision,ns.updated_at,aspec.limits_max_connections,aspec.limits_max_streams,aspec.limits_max_buffer_bytes,aspec.logging_level,aspec.logging_format,aspec.egress_enabled,aspec.egress_max_connections FROM node_specs ns JOIN agent_specs aspec ON aspec.node_id=ns.node_id WHERE ns.node_id IN (`+questionMarks(len(chunk))+`) AND ns.kind=? ORDER BY ns.node_id`, queryArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var nodeID, kind, updated, loggingLevel, loggingFormat string
			var revision, maxConnections, maxStreams, maxBuffer, egressMax int64
			var egressEnabled int
			if err := rows.Scan(&nodeID, &kind, &revision, &updated, &maxConnections, &maxStreams, &maxBuffer, &loggingLevel, &loggingFormat, &egressEnabled, &egressMax); err != nil {
				_ = rows.Close()
				return nil, err
			}
			parsed, err := parseStoredTime("node_spec.updated_at", updated)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			spec := domain.AgentSpec{NodeID: nodeID, Revision: revision, Limits: domain.AgentLimits{MaxConnections: int(maxConnections), MaxStreams: int(maxStreams), MaxBufferBytes: int(maxBuffer)}, Logging: domain.LoggingPolicy{Level: loggingLevel, Format: loggingFormat}, Egress: domain.EgressPolicy{Enabled: egressEnabled != 0, MaxConnections: int(egressMax)}}
			result[nodeID] = domain.NodeSpec{NodeID: nodeID, Kind: domain.NodeSpecKind(kind), Revision: revision, UpdatedAt: parsed, Agent: &spec}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		labelsByNode, err := loadStringMapsForIDs(ctx, q, "agent_selector_labels", "node_id", chunk)
		if err != nil {
			return nil, err
		}
		for nodeID, labels := range labelsByNode {
			if spec, ok := result[nodeID]; ok && spec.Agent != nil {
				spec.Agent.GatewaySelector.MatchLabels = labels
				result[nodeID] = spec
			}
		}
		if err := loadAgentBatchProxies(ctx, q, args, chunk, result); err != nil {
			return nil, err
		}
		if err := loadAgentBatchRoutes(ctx, q, args, chunk, result); err != nil {
			return nil, err
		}
		if err := loadAgentBatchRouteValues(ctx, q, args, chunk, result); err != nil {
			return nil, err
		}
		if err := loadAgentBatchEgress(ctx, q, args, chunk, result); err != nil {
			return nil, err
		}
		start = end
	}
	for nodeID, spec := range result {
		if spec.Agent == nil {
			delete(result, nodeID)
			continue
		}
		if err := spec.Validate(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func loadAgentBatchProxies(ctx context.Context, q sqlQueryer, args []any, chunk []string, result map[string]domain.NodeSpec) error {
	rows, err := q.QueryContext(ctx, `SELECT node_id,position,id,protocol,bind,route,enabled FROM agent_proxies WHERE node_id IN (`+questionMarks(len(chunk))+`) ORDER BY node_id,position`, args...)
	if err != nil {
		return err
	}
	positions := make(map[string]int)
	for rows.Next() {
		var nodeID, id, protocol, bind, route string
		var position int64
		var enabled int
		if err := rows.Scan(&nodeID, &position, &id, &protocol, &bind, &route, &enabled); err != nil {
			_ = rows.Close()
			return err
		}
		expected := positions[nodeID]
		if err := requireStoredPosition(position, expected, "agent proxy"); err != nil {
			_ = rows.Close()
			return err
		}
		positions[nodeID] = expected + 1
		if spec, ok := result[nodeID]; ok && spec.Agent != nil {
			spec.Agent.Proxies = append(spec.Agent.Proxies, domain.ProxySpec{ID: id, Protocol: protocol, Bind: bind, Route: route, Enabled: enabled != 0})
			result[nodeID] = spec
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}

func loadAgentBatchRoutes(ctx context.Context, q sqlQueryer, args []any, chunk []string, result map[string]domain.NodeSpec) error {
	rows, err := q.QueryContext(ctx, `SELECT node_id,position,name,destination,enabled FROM agent_routes WHERE node_id IN (`+questionMarks(len(chunk))+`) ORDER BY node_id,position`, args...)
	if err != nil {
		return err
	}
	positions := make(map[string]int)
	for rows.Next() {
		var nodeID, name, destination string
		var position int64
		var enabled int
		if err := rows.Scan(&nodeID, &position, &name, &destination, &enabled); err != nil {
			_ = rows.Close()
			return err
		}
		expected := positions[nodeID]
		if err := requireStoredPosition(position, expected, "agent route"); err != nil {
			_ = rows.Close()
			return err
		}
		positions[nodeID] = expected + 1
		if spec, ok := result[nodeID]; ok && spec.Agent != nil {
			spec.Agent.Routes = append(spec.Agent.Routes, domain.RouteRule{Name: name, Destination: destination, Enabled: enabled != 0})
			result[nodeID] = spec
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}

func loadAgentBatchRouteValues(ctx context.Context, q sqlQueryer, args []any, chunk []string, result map[string]domain.NodeSpec) error {
	rows, err := q.QueryContext(ctx, `SELECT node_id,route_position,kind,position,value FROM agent_route_values WHERE node_id IN (`+questionMarks(len(chunk))+`) ORDER BY node_id,route_position,kind,position`, args...)
	if err != nil {
		return err
	}
	positions := make(map[string]int)
	for rows.Next() {
		var nodeID, kind, value string
		var routePosition, position int64
		if err := rows.Scan(&nodeID, &routePosition, &kind, &position, &value); err != nil {
			_ = rows.Close()
			return err
		}
		if err := requireStoredKind(kind, "agent route value", "cidrs", "domains", "geoip"); err != nil {
			_ = rows.Close()
			return err
		}
		if spec, ok := result[nodeID]; ok && spec.Agent != nil {
			if routePosition < 0 || routePosition >= int64(len(spec.Agent.Routes)) {
				_ = rows.Close()
				return fmt.Errorf("stored agent route position is invalid")
			}
			key := fmt.Sprintf("%s:%d:%s", nodeID, routePosition, kind)
			expected := positions[key]
			if err := requireStoredPosition(position, expected, "agent "+kind+" route value"); err != nil {
				_ = rows.Close()
				return err
			}
			positions[key] = expected + 1
			route := &spec.Agent.Routes[routePosition]
			switch kind {
			case "cidrs":
				route.CIDRs = append(route.CIDRs, value)
			case "domains":
				route.Domains = append(route.Domains, value)
			case "geoip":
				route.GeoIP = append(route.GeoIP, value)
			}
			result[nodeID] = spec
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}

func loadAgentBatchEgress(ctx context.Context, q sqlQueryer, args []any, chunk []string, result map[string]domain.NodeSpec) error {
	rows, err := q.QueryContext(ctx, `SELECT node_id,kind,position,value FROM agent_egress_values WHERE node_id IN (`+questionMarks(len(chunk))+`) ORDER BY node_id,kind,position`, args...)
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
		if err := requireStoredKind(kind, "agent egress", "tcp_ports", "udp_ports", "allow_cidrs", "allow_special_cidrs"); err != nil {
			_ = rows.Close()
			return err
		}
		key := nodeID + "\x00" + kind
		expected := positions[key]
		if err := requireStoredPosition(position, expected, "agent "+kind+" egress value"); err != nil {
			_ = rows.Close()
			return err
		}
		positions[key] = expected + 1
		if spec, ok := result[nodeID]; ok && spec.Agent != nil {
			switch kind {
			case "tcp_ports":
				spec.Agent.Egress.TCPPorts = append(spec.Agent.Egress.TCPPorts, value)
			case "udp_ports":
				spec.Agent.Egress.UDPPorts = append(spec.Agent.Egress.UDPPorts, value)
			case "allow_cidrs":
				spec.Agent.Egress.AllowCIDRs = append(spec.Agent.Egress.AllowCIDRs, value)
			case "allow_special_cidrs":
				spec.Agent.Egress.AllowSpecialCIDRs = append(spec.Agent.Egress.AllowSpecialCIDRs, value)
			}
			result[nodeID] = spec
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}
