package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

func assignmentParticipantIDsTx(ctx context.Context, tx *sql.Tx, nodeID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT gateway_id,agent_id FROM assignments WHERE gateway_id=? OR agent_id=?`, nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var gatewayID, agentID string
		if err := rows.Scan(&gatewayID, &agentID); err != nil {
			return nil, err
		}
		result = append(result, gatewayID, agentID)
	}
	return result, rows.Err()
}

func assignmentParticipantIDsForServiceTx(ctx context.Context, tx *sql.Tx, serviceID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT assignments.gateway_id,assignments.agent_id FROM assignments JOIN assignment_services ON assignment_services.assignment_id=assignments.id WHERE assignment_services.service_id=?`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var gatewayID, agentID string
		if err := rows.Scan(&gatewayID, &agentID); err != nil {
			return nil, err
		}
		result = append(result, gatewayID, agentID)
	}
	return result, rows.Err()
}

// bumpAssignmentsForServiceTx invalidates the shared placement generation for
// every assignment that consumes a changed Service. It updates only the
// assignment lifecycle columns; the ordered relationship rows remain intact.
func bumpAssignmentsForServiceTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT assignments.id,assignments.revision,assignments.generation FROM assignments JOIN assignment_services ON assignment_services.assignment_id=assignments.id WHERE assignment_services.service_id=? ORDER BY assignments.id`, serviceID)
	if err != nil {
		return err
	}
	type assignmentRow struct {
		id         string
		revision   int64
		generation uint64
	}
	assigned := make([]assignmentRow, 0)
	for rows.Next() {
		var row assignmentRow
		var generation int64
		if err := rows.Scan(&row.id, &row.revision, &generation); err != nil {
			_ = rows.Close()
			return err
		}
		row.generation, err = storedUint64(generation, "assignment generation")
		if err != nil {
			_ = rows.Close()
			return err
		}
		assigned = append(assigned, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range assigned {
		assignment, err := loadAssignmentNormalized(ctx, tx, row.id)
		if err != nil {
			return fmt.Errorf("load assignment %q: %w", row.id, err)
		}
		contains := false
		for _, candidate := range assignment.ServiceIDs {
			if candidate == serviceID {
				contains = true
				break
			}
		}
		if !contains {
			continue
		}
		if assignment.ID != row.id || assignment.Generation != row.generation {
			return &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment", Message: "stored assignment metadata does not match its row"}
		}
		if assignment.Generation == math.MaxUint64 {
			return &domain.ApplyError{Code: "invalid_generation", Path: "assignment.generation", Message: "assignment generation is exhausted"}
		}
		if row.revision == math.MaxInt64 {
			return &domain.ApplyError{Code: "invalid_revision", Path: "assignment.revision", Message: "assignment revision is exhausted"}
		}
		assignment.Generation++
		if assignment.State != domain.AssignmentDegraded && assignment.State != domain.AssignmentDraining {
			assignment.State = domain.AssignmentPending
		}
		assignment.Revision = row.revision + 1
		assignment.UpdatedAt = time.Now().UTC()
		result, err := updateAssignmentColumnsTx(ctx, tx, assignment, row.revision)
		if err != nil {
			return err
		}
		if err := requireRevisionWrite(ctx, tx, result, "assignment", row.revision, `SELECT revision FROM assignments WHERE id=?`, row.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, row.id); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, "system", "derived_service", "assignment", row.id, assignment.Revision, map[string]string{"service_id": serviceID, "generation": fmt.Sprint(assignment.Generation)}); err != nil {
			return err
		}
	}
	return nil
}

func serviceHasAssignment(ctx context.Context, tx *sql.Tx, serviceID string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignment_services WHERE service_id=?`, serviceID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func assignmentForService(ctx context.Context, tx *sql.Tx, serviceID string) (domain.Assignment, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT assignments.id FROM assignments JOIN assignment_services ON assignment_services.assignment_id=assignments.id WHERE assignment_services.service_id=?`, serviceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Assignment{}, false, nil
	}
	if err != nil {
		return domain.Assignment{}, false, err
	}
	assignment, err := loadAssignmentNormalized(ctx, tx, id)
	if err != nil {
		return domain.Assignment{}, false, err
	}
	return assignment, true, nil
}
