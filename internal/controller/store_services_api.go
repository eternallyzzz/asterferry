package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"asterferry/internal/domain"
)

// PutGatewaySpec stores the normalized Gateway behavior specification.
func (s *Repository) PutGatewaySpec(ctx context.Context, spec domain.GatewaySpec, options WriteOptions) error {
	return s.PutNodeSpec(ctx, domain.NewGatewayNodeSpec(spec), options)
}

func (s *Repository) DeleteGatewaySpec(ctx context.Context, nodeID string, options WriteOptions) error {
	return s.DeleteNodeSpec(ctx, nodeID, options)
}

func (s *Repository) GetGatewaySpec(ctx context.Context, nodeID string) (domain.GatewaySpec, error) {
	spec, err := s.GetNodeSpec(ctx, nodeID)
	if err != nil {
		return domain.GatewaySpec{}, err
	}
	if spec.Kind != domain.NodeSpecGateway || spec.Gateway == nil {
		return domain.GatewaySpec{}, sql.ErrNoRows
	}
	value := *spec.Gateway
	value.Revision = spec.Revision
	return value, nil
}

func (s *Repository) PutAgentSpec(ctx context.Context, spec domain.AgentSpec, options WriteOptions) error {
	return s.PutNodeSpec(ctx, domain.NewAgentNodeSpec(spec), options)
}

func (s *Repository) DeleteAgentSpec(ctx context.Context, nodeID string, options WriteOptions) error {
	return s.DeleteNodeSpec(ctx, nodeID, options)
}

func (s *Repository) GetAgentSpec(ctx context.Context, nodeID string) (domain.AgentSpec, error) {
	spec, err := s.GetNodeSpec(ctx, nodeID)
	if err != nil {
		return domain.AgentSpec{}, err
	}
	if spec.Kind != domain.NodeSpecAgent || spec.Agent == nil {
		return domain.AgentSpec{}, sql.ErrNoRows
	}
	value := *spec.Agent
	value.Revision = spec.Revision
	return value, nil
}

func (s *Repository) PutService(ctx context.Context, service domain.Service, options WriteOptions) error {
	if err := service.Validate(); err != nil {
		return err
	}
	spec, specErr := s.GetNodeSpec(ctx, service.AgentID)
	if specErr != nil {
		return specErr
	}
	if spec.Kind != domain.NodeSpecAgent {
		return errors.New("service agent has the wrong node kind")
	}
	return s.putServiceDocument(ctx, service, options)
}

func (s *Repository) GetService(ctx context.Context, id string) (domain.Service, error) {
	return loadServiceNormalized(ctx, s.db, id)
}

func (s *Repository) ListServices(ctx context.Context, agentID string) ([]domain.Service, error) {
	query := `SELECT id FROM services`
	args := []any{}
	if strings.TrimSpace(agentID) != "" {
		query += ` WHERE agent_id=?`
		args = append(args, agentID)
	}
	query += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	result := []domain.Service{}
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
	services, err := loadServicesBatchNormalized(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		service, ok := services[id]
		if !ok {
			return nil, fmt.Errorf("stored service %q disappeared while listing", id)
		}
		result = append(result, service)
	}
	return result, nil
}

func (s *Repository) DeleteService(ctx context.Context, id string, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := struct {
		ID      string `json:"id"`
		IfMatch int64  `json:"if_match"`
	}{ID: id, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	var revision int64
	var agentID string
	if err := tx.QueryRowContext(ctx, `SELECT revision,agent_id FROM services WHERE id=?`, id).Scan(&revision, &agentID); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: "service", Expected: options.IfMatch, Actual: revision}
	}
	if assigned, assignmentErr := serviceHasAssignment(ctx, tx, id); assignmentErr != nil {
		return assignmentErr
	} else if assigned {
		return &domain.ApplyError{Code: "resource_conflict", Path: "service", Message: "assigned service cannot be deleted"}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id=? AND revision=?`, id, revision)
	if err != nil {
		return err
	}
	if err := requireRevisionWrite(ctx, tx, result, "service", revision, `SELECT revision FROM services WHERE id=?`, id); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", "service", id, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, agentID)
}
