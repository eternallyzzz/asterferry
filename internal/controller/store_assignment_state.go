package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"asterferry/internal/domain"
)

// UpdateAssignmentState changes only the lifecycle state of an assignment.
// Placement and ordered child rows are left untouched.
func (s *ResourceRepository) UpdateAssignmentState(ctx context.Context, id, state string, options WriteOptions) (domain.Assignment, error) {
	if strings.TrimSpace(id) == "" {
		return domain.Assignment{}, sql.ErrNoRows
	}
	if state != domain.AssignmentPending && state != domain.AssignmentApplied && state != domain.AssignmentDegraded && state != domain.AssignmentDraining {
		return domain.Assignment{}, &domain.ApplyError{Code: "invalid_assignment_state", Path: "state", Message: "assignment state is invalid"}
	}
	if state == domain.AssignmentApplied {
		return domain.Assignment{}, &domain.ApplyError{Code: "state_controller_owned", Path: "state", Message: "assignment state applied is controller-owned"}
	}
	tx, err := s.beginWriteTx(ctx)
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
	assignment, err := loadAssignmentNormalized(ctx, tx, id)
	if err != nil {
		return domain.Assignment{}, err
	}
	if hit {
		if err := s.commitWriteTx(ctx, tx); err != nil {
			return domain.Assignment{}, err
		}
		s.ChangeBus().notifyResourceChanges(assignment.GatewayID, assignment.AgentID)
		return assignment, nil
	}
	if options.IfMatch <= 0 || options.IfMatch != assignment.Revision {
		return domain.Assignment{}, &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: assignment.Revision}
	}
	if assignment.Revision == math.MaxInt64 {
		return domain.Assignment{}, &domain.ApplyError{Code: "invalid_revision", Path: "assignment.revision", Message: "assignment revision is exhausted"}
	}
	assignment.State = state
	assignment.Revision++
	assignment.UpdatedAt = time.Now().UTC()
	result, err := updateAssignmentColumnsTx(ctx, tx, assignment, assignment.Revision-1)
	if err != nil {
		return domain.Assignment{}, err
	}
	if err := requireRevisionWrite(ctx, tx, result, "assignment", assignment.Revision-1, `SELECT revision FROM assignments WHERE id=?`, id); err != nil {
		return domain.Assignment{}, err
	}
	if state == domain.AssignmentDegraded || state == domain.AssignmentDraining {
		if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
			return domain.Assignment{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, id); err != nil {
		return domain.Assignment{}, err
	}
	if err := insertAudit(ctx, tx, options.Actor, "state", "assignment", id, assignment.Revision, map[string]string{"state": state}); err != nil {
		return domain.Assignment{}, err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": assignment.Revision, "state": state}); err != nil {
		return domain.Assignment{}, err
	}
	if err := s.commitWriteTx(ctx, tx); err != nil {
		return domain.Assignment{}, err
	}
	s.ChangeBus().notifySnapshotChanges(assignment.GatewayID, assignment.AgentID)
	s.ChangeBus().notifyResourceChanges(assignment.GatewayID, assignment.AgentID)
	return assignment, nil
}

// applyNodeResult records the lifecycle consequence of a node applying its
// complete desired snapshot.
func (s *ResourceRepository) applyNodeResult(ctx context.Context, nodeID string, generation uint64, applied bool, actor string) ([]domain.Assignment, error) {
	return s.applyNodeResultWithError(ctx, nodeID, generation, applied, "", actor)
}

func (s *ResourceRepository) applyNodeResultWithError(ctx context.Context, nodeID string, generation uint64, applied bool, errorCode, actor string) ([]domain.Assignment, error) {
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
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,gateway_id,agent_id,revision,generation FROM assignments WHERE gateway_id=? OR agent_id=? ORDER BY id`+s.selectForUpdateClause(), nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	type assignmentResultRow struct {
		id                string
		gatewayID         string
		agentID           string
		revision          int64
		indexedGeneration int64
	}
	assignmentRows := make([]assignmentResultRow, 0)
	for rows.Next() {
		var row assignmentResultRow
		if err := rows.Scan(&row.id, &row.gatewayID, &row.agentID, &row.revision, &row.indexedGeneration); err != nil {
			_ = rows.Close()
			return nil, err
		}
		assignmentRows = append(assignmentRows, row)
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
		assignment, err := loadAssignmentNormalized(ctx, tx, row.id)
		if err != nil {
			return nil, fmt.Errorf("load assignment %q: %w", row.id, err)
		}
		if assignment.ID != row.id || assignment.GatewayID != row.gatewayID || assignment.AgentID != row.agentID || int64(assignment.Generation) != row.indexedGeneration || assignment.Generation > generation || assignment.State == domain.AssignmentDraining {
			continue
		}
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
			ready, err := assignmentParticipantsApplied(ctx, tx, assignment)
			if err != nil {
				return nil, err
			}
			if !ready {
				continue
			}
			assignment.State = domain.AssignmentApplied
		} else {
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
		if row.revision == math.MaxInt64 {
			return nil, &domain.ApplyError{Code: "invalid_revision", Path: "assignment.revision", Message: "assignment revision is exhausted"}
		}
		assignment.Revision = row.revision + 1
		assignment.UpdatedAt = now
		result, err := updateAssignmentColumnsTx(ctx, tx, assignment, row.revision)
		if err != nil {
			return nil, err
		}
		if err := requireRevisionWrite(ctx, tx, result, "assignment", row.revision, `SELECT revision FROM assignments WHERE id=?`, assignment.ID); err != nil {
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
	if err := s.commitWriteTx(ctx, tx); err != nil {
		return nil, err
	}
	s.ChangeBus().notifySnapshotChanges(changedNodes...)
	s.ChangeBus().notifyResourceChanges(changedNodes...)
	return changed, nil
}
