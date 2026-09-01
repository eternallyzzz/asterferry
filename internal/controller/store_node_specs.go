package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"asterferry/internal/domain"
)

// GetNodeSpec returns the behavior document for a node. A missing row is
// intentional: freshly enrolled nodes are valid identities and wait here
// until an operator configures their first behavior.
func (s *Store) GetNodeSpec(ctx context.Context, nodeID string) (domain.NodeSpec, error) {
	var data []byte
	var kind string
	var revision int64
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT kind,document_json,revision,updated_at FROM node_specs WHERE node_id=?`, nodeID).Scan(&kind, &data, &revision, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		// Read old rows while a v7 deployment is still carrying the compatibility
		// tables. This also makes an interrupted/manual migration fail safe.
		if spec, legacyErr := s.getLegacyNodeSpec(ctx, nodeID); legacyErr == nil {
			return spec, nil
		} else if !errors.Is(legacyErr, sql.ErrNoRows) {
			return domain.NodeSpec{}, legacyErr
		}
		return domain.NodeSpec{}, err
	}
	if err != nil {
		return domain.NodeSpec{}, err
	}
	var spec domain.NodeSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return domain.NodeSpec{}, fmt.Errorf("decode node spec %q: %w", nodeID, err)
	}
	spec.NodeID = nodeID
	spec.Kind = domain.NodeSpecKind(kind)
	spec.Revision = revision
	if spec.UpdatedAt, err = parseStoredTime("node_spec.updated_at", updated); err != nil {
		return domain.NodeSpec{}, err
	}
	if err := spec.Validate(); err != nil {
		return domain.NodeSpec{}, fmt.Errorf("stored node spec is invalid: %w", err)
	}
	return spec, nil
}

func (s *Store) decorateNodeSpecKind(ctx context.Context, node *domain.Node) error {
	if node == nil {
		return nil
	}
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM node_specs WHERE node_id=?`, node.ID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		// Compatibility databases may have only the old typed table populated.
		if node.Role == domain.RoleGateway {
			node.SpecKind = domain.NodeSpecGateway
		} else if node.Role == domain.RoleAgent {
			node.SpecKind = domain.NodeSpecAgent
		}
		return nil
	}
	if err != nil {
		return err
	}
	node.SpecKind = domain.NodeSpecKind(kind)
	return nil
}

func (s *Store) getLegacyNodeSpec(ctx context.Context, nodeID string) (domain.NodeSpec, error) {
	if spec, err := s.GetGatewaySpec(ctx, nodeID); err == nil {
		return domain.NewGatewayNodeSpec(spec), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.NodeSpec{}, err
	}
	spec, err := s.GetAgentSpec(ctx, nodeID)
	if err != nil {
		return domain.NodeSpec{}, err
	}
	return domain.NewAgentNodeSpec(spec), nil
}

// PutNodeSpec is the only new public write path for node behavior. The
// compatibility role column is updated in the same logical operation so the
// existing scheduler and data-plane code keep working while the database
// rolls forward. Switching kinds is deliberately refused once the node owns
// services, assignments, or gateway bindings.
func (s *Store) PutNodeSpec(ctx context.Context, spec domain.NodeSpec, options WriteOptions) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	node, err := s.GetNode(ctx, spec.NodeID)
	if err != nil {
		return err
	}
	if node.Role != "" && node.Role != spec.RuntimeKind() {
		var dependents int
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM assignments WHERE gateway_id=? OR agent_id=?) + (SELECT COUNT(*) FROM services WHERE agent_id=?) + (SELECT COUNT(*) FROM service_bindings WHERE gateway_id=?)`, spec.NodeID, spec.NodeID, spec.NodeID, spec.NodeID).Scan(&dependents); err != nil {
			return err
		}
		if dependents > 0 {
			return &domain.ApplyError{Code: "resource_conflict", Path: "kind", Message: "node behavior cannot be changed while it has dependent services, assignments, or bindings"}
		}
	}
	// The typed writer owns validation, revision allocation, assignment
	// derivation, audit, idempotency, and the compatibility role update. In
	// particular, do not update nodes.role before entering that transaction:
	// an invalid spec must not leave a generic identity looking configured.
	// Behavior changes are an explicit delete-then-create operation for now;
	// this keeps the old typed tables and the unified envelope atomic without
	// inventing a second, subtly different write path.
	if node.Role != "" && node.Role != spec.RuntimeKind() {
		return &domain.ApplyError{Code: "resource_conflict", Path: "kind", Message: "delete the existing node behavior before switching its kind"}
	}
	if spec.Kind == domain.NodeSpecGateway {
		return s.PutGatewaySpec(ctx, *spec.Gateway, options)
	}
	return s.PutAgentSpec(ctx, *spec.Agent, options)
}

func (s *Store) DeleteNodeSpec(ctx context.Context, nodeID string, options WriteOptions) error {
	spec, err := s.GetNodeSpec(ctx, nodeID)
	if err != nil {
		return err
	}
	if spec.Kind == domain.NodeSpecGateway {
		return s.DeleteGatewaySpec(ctx, nodeID, options)
	} else {
		return s.DeleteAgentSpec(ctx, nodeID, options)
	}
}

func (s *Store) ListNodeSpecs(ctx context.Context) ([]domain.NodeSpec, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id,kind,document_json,revision,updated_at FROM node_specs ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.NodeSpec, 0)
	for rows.Next() {
		var nodeID, kind, updated string
		var data []byte
		var revision int64
		if err := rows.Scan(&nodeID, &kind, &data, &revision, &updated); err != nil {
			return nil, err
		}
		var spec domain.NodeSpec
		if err := json.Unmarshal(data, &spec); err != nil {
			return nil, err
		}
		spec.NodeID = nodeID
		spec.Kind = domain.NodeSpecKind(kind)
		spec.Revision = revision
		spec.UpdatedAt, err = parseStoredTime("node_spec.updated_at", updated)
		if err != nil {
			return nil, err
		}
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		result = append(result, spec)
	}
	return result, rows.Err()
}
