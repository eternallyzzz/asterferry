package controller

import (
	"context"
	"fmt"

	"asterferry/internal/domain"
)

func loadNodeSpecsBatchNormalized(ctx context.Context, q sqlQueryer, ids []string) (map[string]domain.NodeSpec, error) {
	type metadata struct {
		kind domain.NodeSpecKind
	}
	metadatas := make(map[string]metadata, len(ids))
	gatewayNodes := make([]domain.Node, 0)
	agentNodes := make([]domain.Node, 0)
	for start := 0; start < len(ids); {
		chunk, args, end := batchIDs(ids, start, gatewayCandidateBatchSize)
		rows, err := q.QueryContext(ctx, `SELECT node_id,kind,revision,updated_at FROM node_specs WHERE node_id IN (`+questionMarks(len(chunk))+`) ORDER BY node_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var nodeID, kind, updated string
			var revision int64
			if err := rows.Scan(&nodeID, &kind, &revision, &updated); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if _, err := parseStoredTime("node_spec.updated_at", updated); err != nil {
				_ = rows.Close()
				return nil, err
			}
			specKind := domain.NodeSpecKind(kind)
			metadatas[nodeID] = metadata{kind: specKind}
			switch specKind {
			case domain.NodeSpecGateway:
				gatewayNodes = append(gatewayNodes, domain.Node{ID: nodeID})
			case domain.NodeSpecAgent:
				agentNodes = append(agentNodes, domain.Node{ID: nodeID})
			default:
				_ = rows.Close()
				return nil, &domain.ApplyError{Code: "invalid_spec_kind", Path: "node_spec.kind", Message: "stored node spec kind is invalid"}
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		start = end
	}
	gateways, err := loadGatewaySpecsBatchNormalized(ctx, q, gatewayNodes)
	if err != nil {
		return nil, err
	}
	agents, err := loadAgentSpecsBatchNormalized(ctx, q, agentNodes)
	if err != nil {
		return nil, err
	}
	result := make(map[string]domain.NodeSpec, len(metadatas))
	for _, id := range ids {
		meta, ok := metadatas[id]
		if !ok {
			return nil, fmt.Errorf("stored node spec %q disappeared while listing", id)
		}
		var spec domain.NodeSpec
		switch meta.kind {
		case domain.NodeSpecGateway:
			spec, ok = gateways[id]
		case domain.NodeSpecAgent:
			spec, ok = agents[id]
		}
		if !ok {
			return nil, fmt.Errorf("stored %s spec %q is missing", meta.kind, id)
		}
		result[id] = spec
	}
	return result, nil
}

func loadServicesBatchNormalized(ctx context.Context, q sqlQueryer, ids []string) (map[string]domain.Service, error) {
	result := make(map[string]domain.Service, len(ids))
	for start := 0; start < len(ids); {
		chunk, args, end := batchIDs(ids, start, gatewayCandidateBatchSize)
		rows, err := q.QueryContext(ctx, `SELECT id,agent_id,protocol,local_target,public_bind,public_port,enabled,revision,updated_at FROM services WHERE id IN (`+questionMarks(len(chunk))+`) ORDER BY id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var service domain.Service
			var publicPort, enabled int64
			var updated string
			if err := rows.Scan(&service.ID, &service.AgentID, &service.Protocol, &service.LocalTarget, &service.PublicBind, &publicPort, &enabled, &service.Revision, &updated); err != nil {
				_ = rows.Close()
				return nil, err
			}
			service.PublicPort, err = storedPort(publicPort, "service public port")
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			service.Enabled = enabled != 0
			service.UpdatedAt, err = parseStoredTime("service.updated_at", updated)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			result[service.ID] = service
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		selectors, err := loadStringMapsForIDs(ctx, q, "service_selector_labels", "service_id", chunk)
		if err != nil {
			return nil, err
		}
		for _, id := range chunk {
			service, ok := result[id]
			if !ok {
				continue
			}
			service.GatewaySelector.MatchLabels = selectors[id]
			if service.GatewaySelector.MatchLabels == nil {
				service.GatewaySelector.MatchLabels = map[string]string{}
			}
			if err := service.Validate(); err != nil {
				return nil, fmt.Errorf("stored service is invalid: %w", err)
			}
			result[id] = service
		}
		start = end
	}
	return result, nil
}
