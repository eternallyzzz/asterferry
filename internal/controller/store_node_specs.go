package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"asterferry/internal/domain"
)

// GetNodeSpec returns the node's typed behavior aggregate. The node_specs row
// holds only its discriminator and repository metadata; configuration fields
// live in the kind-specific relational tables.
func (s *ResourceRepository) GetNodeSpec(ctx context.Context, nodeID string) (domain.NodeSpec, error) {
	return loadNodeSpecNormalized(ctx, s.db, nodeID)
}

func (s *ResourceRepository) decorateNodeSpecKind(ctx context.Context, node *domain.Node) error {
	if node == nil {
		return nil
	}
	var kind string
	if err := s.db.QueryRowContext(ctx, `SELECT kind FROM node_specs WHERE node_id=?`, node.ID).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	node.SpecKind = domain.NodeSpecKind(kind)
	return nil
}

func (s *ResourceRepository) PutNodeSpec(ctx context.Context, spec domain.NodeSpec, options WriteOptions) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if spec.Kind == domain.NodeSpecGateway {
		if err := s.protectObfuscationPolicy(&spec.Gateway.Obfuscation); err != nil {
			return err
		}
	}
	return s.putNodeSpecDocument(ctx, spec, options)
}

// putNodeSpecDocument is retained as the application-level write name while
// the physical representation is now a normalized aggregate.
func (s *ResourceRepository) putNodeSpecDocument(ctx context.Context, spec domain.NodeSpec, options WriteOptions) error {
	requestSpec := spec
	requestSpec.Revision = 0
	requestSpec.UpdatedAt = time.Time{}
	if requestSpec.Gateway != nil {
		value := *requestSpec.Gateway
		value.Obfuscation = obfuscationRequestPolicy(value.Obfuscation)
		requestSpec.Gateway = &value
	}
	idempotentRequest := struct {
		Spec    domain.NodeSpec `json:"spec"`
		IfMatch int64           `json:"if_match"`
	}{Spec: requestSpec, IfMatch: options.IfMatch}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, idempotentRequest); err != nil {
		return err
	} else if hit {
		return nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id=?`, spec.NodeID).Scan(&exists); err != nil {
		return err
	}
	if spec.Kind == domain.NodeSpecGateway {
		if err := validateGatewaySpecTx(ctx, tx, *spec.Gateway); err != nil {
			return err
		}
	} else if err := validateAgentSpecTx(ctx, tx, *spec.Agent); err != nil {
		return err
	}
	var revision int64
	var currentKind string
	err = tx.QueryRowContext(ctx, `SELECT revision FROM node_specs WHERE node_id=?`, spec.NodeID).Scan(&revision)
	isInsert := errors.Is(err, sql.ErrNoRows)
	if isInsert {
		revision = 1
		if options.IfMatch > 0 {
			return &RevisionConflictError{Resource: "node_spec", Expected: options.IfMatch, Actual: 0}
		}
	} else if err == nil {
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM node_specs WHERE node_id=?`, spec.NodeID).Scan(&currentKind); err != nil {
			return err
		}
		if currentKind != string(spec.Kind) {
			dependents, err := nodeSpecDependentsTx(ctx, tx, spec.NodeID, currentKind)
			if err != nil {
				return err
			}
			if dependents > 0 {
				return &domain.ApplyError{Code: "resource_conflict", Path: "kind", Message: "node behavior cannot change while services or assignments depend on it"}
			}
		}
		if options.IfMatch <= 0 || options.IfMatch != revision {
			return &RevisionConflictError{Resource: "node_spec", Expected: options.IfMatch, Actual: revision}
		}
		revision++
	} else {
		return err
	}
	spec.Revision = revision
	spec.UpdatedAt = time.Now().UTC()
	if spec.Gateway != nil {
		spec.Gateway.Revision = revision
	}
	if spec.Agent != nil {
		spec.Agent.Revision = revision
	}
	now := spec.UpdatedAt.Format(time.RFC3339Nano)
	if isInsert {
		_, err = tx.ExecContext(ctx, `INSERT INTO node_specs(node_id,kind,revision,updated_at) VALUES(?,?,?,?)`, spec.NodeID, string(spec.Kind), revision, now)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE node_specs SET kind=?,revision=?,updated_at=? WHERE node_id=? AND revision=?`, string(spec.Kind), revision, now, spec.NodeID, revision-1)
		if err == nil {
			err = requireRevisionWrite(ctx, tx, result, "node_spec", revision-1, `SELECT revision FROM node_specs WHERE node_id=?`, spec.NodeID)
		}
	}
	if err != nil {
		return err
	}
	if err := writeNodeSpecNormalizedTx(ctx, tx, spec); err != nil {
		return err
	}
	affectedNodes := []string{spec.NodeID}
	if spec.Kind == domain.NodeSpecGateway {
		participants, err := assignmentParticipantIDsTx(ctx, tx, spec.NodeID)
		if err != nil {
			return err
		}
		affectedNodes = append(affectedNodes, participants...)
		if err := updateAssignmentEndpointsTx(ctx, tx, *spec.Gateway); err != nil {
			return err
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "upsert", "node_spec", spec.NodeID, revision, map[string]string{"kind": string(spec.Kind)}); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"node_id": spec.NodeID, "revision": revision}); err != nil {
		return err
	}
	if spec.Kind == domain.NodeSpecGateway {
		return s.commitAndNotifyPendingServices(tx, affectedNodes...)
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

func (s *ResourceRepository) DeleteNodeSpec(ctx context.Context, nodeID string, options WriteOptions) error {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := struct {
		NodeID string `json:"node_id"`
	}{NodeID: nodeID}
	if hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request); err != nil {
		return err
	} else if hit {
		return tx.Commit()
	}
	var revision int64
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind,revision FROM node_specs WHERE node_id=?`, nodeID).Scan(&kind, &revision); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: "node_spec", Expected: options.IfMatch, Actual: revision}
	}
	dependents, err := nodeSpecDependentsTx(ctx, tx, nodeID, kind)
	if err != nil {
		return err
	}
	if dependents > 0 {
		return &domain.ApplyError{Code: "resource_conflict", Path: "node_spec", Message: "node behavior has dependent services or assignments"}
	}
	participants, err := assignmentParticipantIDsTx(ctx, tx, nodeID)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM node_specs WHERE node_id=? AND revision=?`, nodeID, revision)
	if err != nil {
		return err
	}
	if err := requireRevisionWrite(ctx, tx, result, "node_spec", revision, `SELECT revision FROM node_specs WHERE node_id=?`, nodeID); err != nil {
		return err
	}
	if err := clearDesiredSnapshotTx(ctx, tx, nodeID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", "node_spec", nodeID, revision, map[string]string{"kind": kind}); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"node_id": nodeID, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, append(participants, nodeID)...)
}

func nodeSpecDependentsTx(ctx context.Context, tx *sql.Tx, nodeID, kind string) (int, error) {
	var dependents int
	switch domain.NodeSpecKind(kind) {
	case domain.NodeSpecGateway:
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignments WHERE gateway_id=?`, nodeID).Scan(&dependents); err != nil {
			return 0, err
		}
	case domain.NodeSpecAgent:
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM services WHERE agent_id=?) + (SELECT COUNT(*) FROM assignments WHERE agent_id=?)`, nodeID, nodeID).Scan(&dependents); err != nil {
			return 0, err
		}
	default:
		return 0, &domain.ApplyError{Code: "invalid_spec_kind", Path: "node_spec.kind", Message: "stored node spec kind is invalid"}
	}
	return dependents, nil
}

func (s *ResourceRepository) ListNodeSpecs(ctx context.Context) ([]domain.NodeSpec, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id FROM node_specs ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			return nil, err
		}
		ids = append(ids, nodeID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	specs, err := loadNodeSpecsBatchNormalized(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	result := make([]domain.NodeSpec, 0, len(ids))
	for _, nodeID := range ids {
		spec, ok := specs[nodeID]
		if !ok {
			return nil, fmt.Errorf("stored node spec %q disappeared while listing", nodeID)
		}
		result = append(result, spec)
	}
	return result, nil
}
