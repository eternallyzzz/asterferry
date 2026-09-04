package controller

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"asterferry/internal/domain"
)

func (s *Repository) putServiceDocument(ctx context.Context, service domain.Service, options WriteOptions) error {
	requestService := service
	requestService.Revision = 0
	requestService.UpdatedAt = time.Time{}
	idempotentRequest := struct {
		Service domain.Service `json:"service"`
		IfMatch int64          `json:"if_match"`
	}{Service: requestService, IfMatch: options.IfMatch}
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, idempotentRequest)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	affectedNodes := []string{service.AgentID}
	participants, err := assignmentParticipantIDsForServiceTx(ctx, tx, service.ID)
	if err != nil {
		return err
	}
	affectedNodes = append(affectedNodes, participants...)
	previous, previousErr := loadServiceNormalized(ctx, tx, service.ID)
	hadPrevious := previousErr == nil
	if previousErr != nil && !errors.Is(previousErr, sql.ErrNoRows) {
		return previousErr
	}
	var revision int64
	if !hadPrevious {
		revision = 1
		if options.IfMatch > 0 {
			return &RevisionConflictError{Resource: "service", Expected: options.IfMatch, Actual: 0}
		}
	} else {
		revision = previous.Revision
		if options.IfMatch <= 0 || options.IfMatch != revision {
			return &RevisionConflictError{Resource: "service", Expected: options.IfMatch, Actual: revision}
		}
		affectedNodes = append(affectedNodes, previous.AgentID)
		if previous.AgentID != service.AgentID || previous.Protocol != service.Protocol || previous.PublicBind != service.PublicBind || previous.PublicPort != service.PublicPort {
			assigned, assignmentErr := serviceHasAssignment(ctx, tx, service.ID)
			if assignmentErr != nil {
				return assignmentErr
			}
			if assigned {
				return &domain.ApplyError{Code: "resource_conflict", Path: "service", Message: "assigned service cannot change agent, protocol, bind, or port"}
			}
		}
		if !selectorsEqual(previous.GatewaySelector, service.GatewaySelector) {
			assignment, assigned, lookupErr := assignmentForService(ctx, tx, service.ID)
			if lookupErr != nil {
				return lookupErr
			}
			if assigned {
				labels, labelErr := loadNodeLabels(ctx, tx, assignment.GatewayID)
				if labelErr != nil {
					return labelErr
				}
				if !service.GatewaySelector.Matches(labels) {
					return &domain.ApplyError{Code: "selector_mismatch", Path: "service.gateway_selector", Message: "assigned service selector does not match its gateway"}
				}
			}
		}
		revision++
	}
	service.Revision = revision
	service.UpdatedAt = nowTime
	if hadPrevious {
		result, updateErr := tx.ExecContext(ctx, `UPDATE services SET agent_id=?,protocol=?,local_target=?,public_bind=?,public_port=?,enabled=?,revision=?,updated_at=? WHERE id=? AND revision=?`, service.AgentID, service.Protocol, service.LocalTarget, normalizeBind(service.PublicBind), service.PublicPort, boolInt(service.Enabled), service.Revision, now, service.ID, revision-1)
		if updateErr != nil {
			return updateErr
		}
		if err := requireRevisionWrite(ctx, tx, result, "service", revision-1, `SELECT revision FROM services WHERE id=?`, service.ID); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO services(id,agent_id,protocol,local_target,public_bind,public_port,enabled,revision,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, service.ID, service.AgentID, service.Protocol, service.LocalTarget, normalizeBind(service.PublicBind), service.PublicPort, boolInt(service.Enabled), service.Revision, now); err != nil {
		return err
	}
	if err := replaceServiceSelectorTx(ctx, tx, service.ID, service.GatewaySelector); err != nil {
		return err
	}
	if hadPrevious && !sameServiceContent(previous, service) {
		if err := bumpAssignmentsForServiceTx(ctx, tx, service.ID); err != nil {
			return err
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "upsert", "service", service.ID, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"id": service.ID, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}
