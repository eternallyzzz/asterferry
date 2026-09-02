package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"asterferry/internal/domain"
)

// GetNodeSpec returns the node's one authoritative behavior document. A
// missing row is intentional: enrollment creates an identity, and the
// operator may choose its behavior later.
func (s *Store) GetNodeSpec(ctx context.Context, nodeID string) (domain.NodeSpec, error) {
	var data []byte
	var kind string
	var revision int64
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT kind,document_json,revision,updated_at FROM node_specs WHERE node_id=?`, nodeID).Scan(&kind, &data, &revision, &updated)
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
	if err := s.db.QueryRowContext(ctx, `SELECT kind FROM node_specs WHERE node_id=?`, node.ID).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	node.SpecKind = domain.NodeSpecKind(kind)
	return nil
}

// PutNodeSpec is the sole behavior write path. Storage and wire lifecycle
// conversions happen around this envelope; no parallel typed spec table is
// maintained.
func (s *Store) PutNodeSpec(ctx context.Context, spec domain.NodeSpec, options WriteOptions) error {
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

func (s *Store) putNodeSpecDocument(ctx context.Context, spec domain.NodeSpec, options WriteOptions) error {
	requestSpec := spec
	if spec.Gateway != nil {
		gateway := *spec.Gateway
		requestSpec.Gateway = &gateway
	}
	if spec.Agent != nil {
		agent := *spec.Agent
		requestSpec.Agent = &agent
	}
	requestSpec.Revision = 0
	requestSpec.UpdatedAt = time.Time{}
	if requestSpec.Gateway != nil {
		requestSpec.Gateway.Obfuscation = obfuscationRequestPolicy(requestSpec.Gateway.Obfuscation)
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
	document, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	now := spec.UpdatedAt.Format(time.RFC3339Nano)
	if isInsert {
		_, err = tx.ExecContext(ctx, `INSERT INTO node_specs(node_id,kind,document_json,revision,updated_at) VALUES(?,?,?,?,?)`, spec.NodeID, string(spec.Kind), document, revision, now)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE node_specs SET kind=?,document_json=?,revision=?,updated_at=? WHERE node_id=? AND revision=?`, string(spec.Kind), document, revision, now, spec.NodeID, revision-1)
		if err == nil {
			err = requireRevisionWrite(ctx, tx, result, "node_spec", revision-1, `SELECT revision FROM node_specs WHERE node_id=?`, spec.NodeID)
		}
	}
	if err != nil {
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

func (s *Store) DeleteNodeSpec(ctx context.Context, nodeID string, options WriteOptions) error {
	// Deleting a behavior publishes an empty desired snapshot. Serialize the
	// read/clear/write sequence with EnsureDesiredSnapshot so a reconnect cannot
	// materialize the retired behavior after this transaction commits.
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

// nodeSpecDependentsTx is the storage invariant behind behavior replacement
// and deletion. A Node can be reconfigured only after its old behavior has no
// live business resources; otherwise the same identity could silently change
// from a Gateway to an Agent while assignments/services still point at it.
func nodeSpecDependentsTx(ctx context.Context, tx *sql.Tx, nodeID, kind string) (int, error) {
	var dependents int
	switch domain.NodeSpecKind(kind) {
	case domain.NodeSpecGateway:
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignments WHERE gateway_id=?`, nodeID).Scan(&dependents); err != nil {
			return 0, err
		}
	case domain.NodeSpecAgent:
		if err := tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM services WHERE agent_id=?) +
			(SELECT COUNT(*) FROM assignments WHERE agent_id=?)`, nodeID, nodeID).Scan(&dependents); err != nil {
			return 0, err
		}
	default:
		return 0, &domain.ApplyError{Code: "invalid_spec_kind", Path: "node_spec.kind", Message: "stored node spec kind is invalid"}
	}
	return dependents, nil
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
		spec.NodeID, spec.Kind, spec.Revision = nodeID, domain.NodeSpecKind(kind), revision
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
