package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// UpdateAssignmentState changes only the lifecycle state of an assignment.
// It is used by the health reconciler and runtime actions; the assignment
// placement and its public bindings remain untouched. The revision and audit
// event are updated in the same transaction so an operator cannot overwrite
// a concurrent placement change with a stale health result.
func (s *Store) UpdateAssignmentState(ctx context.Context, id, state string, options WriteOptions) (domain.Assignment, error) {
	if strings.TrimSpace(id) == "" {
		return domain.Assignment{}, sql.ErrNoRows
	}
	if state != domain.AssignmentPending && state != domain.AssignmentApplied && state != domain.AssignmentDegraded && state != domain.AssignmentDraining {
		return domain.Assignment{}, &domain.ApplyError{Code: "invalid_assignment_state", Path: "state", Message: "assignment state is invalid"}
	}
	// `applied` is not an operator-selected lifecycle transition.  It is the
	// result of the two-sided acknowledgement barrier in applyNodeResult: both
	// the Gateway and Agent must have applied the same assignment generation
	// before the public listener is admitted.  Keeping this guard on the
	// generic state-update path prevents internal callers (or a future API
	// endpoint) from accidentally opening a placement with one-sided state.
	if state == domain.AssignmentApplied {
		return domain.Assignment{}, &domain.ApplyError{Code: "state_controller_owned", Path: "state", Message: "assignment state applied is controller-owned"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Assignment{}, err
	}
	defer tx.Rollback()
	request := struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		IfMatch int64  `json:"if_match"`
	}{ID: id, State: state, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return domain.Assignment{}, err
	}
	if hit {
		var assignment domain.Assignment
		var document []byte
		if err := tx.QueryRowContext(ctx, `SELECT document_json FROM assignments WHERE id=?`, id).Scan(&document); err != nil {
			return domain.Assignment{}, err
		}
		if err := json.Unmarshal(document, &assignment); err != nil {
			return domain.Assignment{}, err
		}
		return assignment, s.commitAndNotifyResources(tx, assignment.GatewayID, assignment.AgentID)
	}
	var revision int64
	var document []byte
	if err := tx.QueryRowContext(ctx, `SELECT revision,document_json FROM assignments WHERE id=?`, id).Scan(&revision, &document); err != nil {
		return domain.Assignment{}, err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return domain.Assignment{}, &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: revision}
	}
	var assignment domain.Assignment
	if err := json.Unmarshal(document, &assignment); err != nil {
		return domain.Assignment{}, err
	}
	assignment.State = state
	assignment.Revision = revision + 1
	assignment.UpdatedAt = time.Now().UTC()
	document, err = json.Marshal(assignment)
	if err != nil {
		return domain.Assignment{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, document, assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano), id, revision)
	if err != nil {
		return domain.Assignment{}, err
	}
	if err := requireRevisionWrite(ctx, tx, result, "assignment", revision, `SELECT revision FROM assignments WHERE id=?`, id); err != nil {
		return domain.Assignment{}, err
	}
	if state == domain.AssignmentDegraded || state == domain.AssignmentDraining {
		if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
			return domain.Assignment{}, err
		}
	}
	// A manually requested lifecycle transition supersedes any participant
	// acknowledgements recorded for the previous state. They are only valid
	// after both nodes apply the next complete desired snapshot.
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, id); err != nil {
		return domain.Assignment{}, err
	}
	if err := insertAudit(ctx, tx, options.Actor, "state", "assignment", id, assignment.Revision, map[string]string{"state": state}); err != nil {
		return domain.Assignment{}, err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": assignment.Revision, "state": state}); err != nil {
		return domain.Assignment{}, err
	}
	return assignment, s.commitAndNotifyResources(tx, assignment.GatewayID, assignment.AgentID)
}

// applyNodeResult records the lifecycle consequence of a node applying its
// complete desired snapshot. The control-stream path uses
// applyNodeResultWithError to retain stable rejection error codes; this
// boolean helper remains useful to package-level tests and simple callers.
//
//lint:ignore U1000 package tests exercise the boolean compatibility helper.
func (s *Store) applyNodeResult(ctx context.Context, nodeID string, generation uint64, applied bool, actor string) ([]domain.Assignment, error) {
	return s.applyNodeResultWithError(ctx, nodeID, generation, applied, "", actor)
}

// applyNodeResultWithError records the lifecycle consequence of a node
// applying its complete desired snapshot. Assignment state is controller-
// owned, so a node can only move assignments that it participates in and only
// when the result generation is at least the assignment generation carried by
// that snapshot. The returned assignments let the caller refresh both
// node-scoped snapshots after the transaction commits. The control-stream form
// also preserves the stable rejection code in assignment_acks for diagnostics
// and auditing.
func (s *Store) applyNodeResultWithError(ctx context.Context, nodeID string, generation uint64, applied bool, errorCode, actor string) ([]domain.Assignment, error) {
	if strings.TrimSpace(nodeID) == "" || generation == 0 {
		return nil, errors.New("node result identity and generation are required")
	}
	if generation > math.MaxInt64 {
		return nil, &domain.ApplyError{Code: "invalid_generation", Path: "generation", Message: "node result generation exceeds repository limit"}
	}
	targetState := domain.AssignmentDegraded
	if applied {
		targetState = domain.AssignmentApplied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// The assignment row is the serialization point for the two participant
	// ACKs. PostgreSQL transactions otherwise can both count the other ACK as
	// absent under READ COMMITTED and commit the assignment in pending state.
	// SQLite gets the empty suffix because its single writer already provides
	// the equivalent serialization.
	rows, err := tx.QueryContext(ctx, `SELECT id,gateway_id,agent_id,document_json,revision,generation FROM assignments WHERE gateway_id=? OR agent_id=? ORDER BY id`+s.selectForUpdateClause(), nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	type assignmentResultRow struct {
		assignment        domain.Assignment
		revision          int64
		indexedGeneration uint64
	}
	assignmentRows := make([]assignmentResultRow, 0)
	for rows.Next() {
		var assignment domain.Assignment
		var document []byte
		var revision int64
		var assignmentGeneration uint64
		if err := rows.Scan(&assignment.ID, &assignment.GatewayID, &assignment.AgentID, &document, &revision, &assignmentGeneration); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(document, &assignment); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("decode assignment %q: %w", assignment.ID, err)
		}
		assignmentRows = append(assignmentRows, assignmentResultRow{assignment: assignment, revision: revision, indexedGeneration: assignmentGeneration})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	changed := make([]domain.Assignment, 0)
	now := time.Now().UTC()
	for _, row := range assignmentRows {
		assignment := row.assignment
		revision := row.revision
		assignmentGeneration := row.indexedGeneration
		// The indexed generation is authoritative for this comparison; the
		// document is checked as well so a corrupted row cannot be advanced by a
		// control result that happens to name the same assignment.
		if assignment.Generation != assignmentGeneration || assignment.Generation > generation || assignment.State == domain.AssignmentDraining {
			continue
		}
		// Persist an acknowledgement for this participant before evaluating the
		// shared lifecycle state. A placement is only opened after both the
		// Gateway and Agent have applied the assignment generation; one node
		// succeeding must never make the other node accept streams prematurely.
		ackStatus := "rejected"
		if applied {
			ackStatus = "applied"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_acks(assignment_id,node_id,generation,status,error_code,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(assignment_id,node_id) DO UPDATE SET generation=excluded.generation,status=excluded.status,error_code=excluded.error_code,updated_at=excluded.updated_at`, assignment.ID, nodeID, assignment.Generation, ackStatus, strings.TrimSpace(errorCode), now.Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if applied {
			if assignment.State == domain.AssignmentDegraded {
				if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
					return nil, err
				}
				continue
			}
			if assignment.State == domain.AssignmentApplied {
				continue
			}
			ready, readyErr := assignmentParticipantsApplied(ctx, tx, assignment)
			if readyErr != nil {
				return nil, readyErr
			}
			if !ready {
				continue
			}
			assignment.State = domain.AssignmentApplied
		} else {
			// A rejected apply is fail-closed. Keep a pending placement out of
			// the data path and mark an already-applied placement degraded until a
			// new scheduling pass provides a complete healthy generation.
			if assignment.State == domain.AssignmentDegraded {
				if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
					return nil, err
				}
				continue
			}
			assignment.State = domain.AssignmentDegraded
			if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
				return nil, err
			}
		}
		assignment.Revision = revision + 1
		assignment.UpdatedAt = now
		updated, marshalErr := json.Marshal(assignment)
		if marshalErr != nil {
			return nil, marshalErr
		}
		result, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Revision, now.Format(time.RFC3339Nano), assignment.ID, revision)
		if err != nil {
			return nil, err
		}
		if err := requireRevisionWrite(ctx, tx, result, "assignment", revision, `SELECT revision FROM assignments WHERE id=?`, assignment.ID); err != nil {
			return nil, err
		}
		if err := insertAudit(ctx, tx, actor, "state", "assignment", assignment.ID, assignment.Revision, map[string]string{"state": targetState, "node_id": nodeID, "generation": fmt.Sprint(generation)}); err != nil {
			return nil, err
		}
		changed = append(changed, assignment)
	}
	changedNodes := make([]string, 0, len(changed)*2)
	for _, assignment := range changed {
		changedNodes = append(changedNodes, assignment.GatewayID, assignment.AgentID)
	}
	if err := s.commitAndNotifyResources(tx, changedNodes...); err != nil {
		return nil, err
	}
	return changed, nil
}
