package controller

import (
	"context"
	"database/sql"
	"fmt"

	"asterferry/internal/domain"
)

func (s *Repository) GetAssignment(ctx context.Context, id string) (domain.Assignment, error) {
	return loadAssignmentNormalized(ctx, s.db, id)
}

func assignmentParticipantsApplied(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT node_id) FROM assignment_acks WHERE assignment_id=? AND generation>=? AND status='applied' AND node_id IN (?,?)`, assignment.ID, assignment.Generation, assignment.GatewayID, assignment.AgentID).Scan(&count)
	return count == 2, err
}

func (s *Repository) ListAssignments(ctx context.Context, gatewayID, agentID string) ([]domain.Assignment, error) {
	query := `SELECT id FROM assignments WHERE 1=1`
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
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	loaded, err := loadAssignmentsByIDsNormalized(ctx, s.db, ids, "id")
	if err != nil {
		return nil, err
	}
	byID := make(map[string]domain.Assignment, len(loaded))
	for _, assignment := range loaded {
		byID[assignment.ID] = assignment
	}
	result := make([]domain.Assignment, 0, len(ids))
	for _, id := range ids {
		assignment, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("stored assignment %q disappeared while listing", id)
		}
		result = append(result, assignment)
	}
	return result, nil
}
