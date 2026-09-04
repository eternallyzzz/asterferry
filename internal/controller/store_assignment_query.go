package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func (s *Store) GetAssignment(ctx context.Context, id string) (domain.Assignment, error) {
	var data []byte
	var revision int64
	var indexedID, indexedGateway, indexedAgent string
	var indexedGeneration uint64
	if err := s.db.QueryRowContext(ctx, `SELECT id,gateway_id,agent_id,document_json,revision,generation FROM assignments WHERE id=?`, id).Scan(&indexedID, &indexedGateway, &indexedAgent, &data, &revision, &indexedGeneration); err != nil {
		return domain.Assignment{}, err
	}
	var assignment domain.Assignment
	if err := json.Unmarshal(data, &assignment); err != nil {
		return domain.Assignment{}, err
	}
	if assignment.State == "" {
		assignment.State = domain.AssignmentPending
	}
	if assignment.ID != indexedID || assignment.GatewayID != indexedGateway || assignment.AgentID != indexedAgent || assignment.Generation != indexedGeneration {
		return domain.Assignment{}, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment", Message: "stored assignment metadata does not match its row"}
	}
	assignment.Revision = revision
	if err := assignment.Validate(); err != nil {
		return domain.Assignment{}, fmt.Errorf("stored assignment is invalid: %w", err)
	}
	return assignment, nil
}

func assignmentParticipantsApplied(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT node_id) FROM assignment_acks WHERE assignment_id=? AND generation>=? AND status='applied' AND node_id IN (?,?)`, assignment.ID, assignment.Generation, assignment.GatewayID, assignment.AgentID).Scan(&count)
	return count == 2, err
}

func (s *Store) ListAssignments(ctx context.Context, gatewayID, agentID string) ([]domain.Assignment, error) {
	query := `SELECT id,gateway_id,agent_id,document_json,revision,generation FROM assignments WHERE 1=1`
	args := []any{}
	if gatewayID != "" {
		query += ` AND gateway_id=?`
		args = append(args, gatewayID)
	}
	if agentID != "" {
		query += ` AND agent_id=?`
		args = append(args, agentID)
	}
	query += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Assignment{}
	for rows.Next() {
		var id, indexedGateway, indexedAgent string
		var data []byte
		var revision int64
		var indexedGeneration uint64
		if err := rows.Scan(&id, &indexedGateway, &indexedAgent, &data, &revision, &indexedGeneration); err != nil {
			return nil, err
		}
		var assignment domain.Assignment
		if err := json.Unmarshal(data, &assignment); err != nil {
			return nil, err
		}
		if assignment.State == "" {
			assignment.State = domain.AssignmentPending
		}
		if assignment.ID != id || assignment.GatewayID != indexedGateway || assignment.AgentID != indexedAgent || assignment.Generation != indexedGeneration {
			return nil, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment", Message: "stored assignment metadata does not match its row"}
		}
		assignment.Revision = revision
		if err := assignment.Validate(); err != nil {
			return nil, fmt.Errorf("stored assignment is invalid: %w", err)
		}
		result = append(result, assignment)
	}
	return result, rows.Err()
}
