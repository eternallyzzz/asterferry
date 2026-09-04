package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"asterferry/internal/domain"
)

// NodeSpec is the small aggregate dispatcher. The concrete Gateway and Agent
// codecs are intentionally separate because each owns a different family of
// normalized child tables.
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
