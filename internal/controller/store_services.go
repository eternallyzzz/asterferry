package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *Store) putServiceDocument(ctx context.Context, service domain.Service, options WriteOptions) error {
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
	var revision int64
	var previousDocument []byte
	var previous domain.Service
	hadPrevious := false
	err = tx.QueryRowContext(ctx, `SELECT revision,document_json FROM services WHERE id=?`, service.ID).Scan(&revision, &previousDocument)
	if errors.Is(err, sql.ErrNoRows) {
		revision = 1
		if options.IfMatch > 0 {
			return &RevisionConflictError{Resource: "service", Expected: options.IfMatch, Actual: 0}
		}
		service.Revision = revision
		service.UpdatedAt = nowTime
		b, marshalErr := json.Marshal(service)
		if marshalErr != nil {
			return marshalErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO services(id,agent_id,document_json,revision,updated_at) VALUES(?,?,?,?,?)`, service.ID, service.AgentID, b, revision, now)
	} else if err == nil {
		hadPrevious = true
		if options.IfMatch <= 0 || options.IfMatch != revision {
			return &RevisionConflictError{Resource: "service", Expected: options.IfMatch, Actual: revision}
		}
		if decodeErr := json.Unmarshal(previousDocument, &previous); decodeErr != nil {
			return fmt.Errorf("decode existing service: %w", decodeErr)
		}
		// An unassigned service may move between Agents. The old Agent's
		// node-scoped snapshot must be invalidated too, otherwise targeted
		// notifications leave the old node serving a stale service document.
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
				var labelsJSON []byte
				if err := tx.QueryRowContext(ctx, `SELECT labels_json FROM nodes WHERE id=?`, assignment.GatewayID).Scan(&labelsJSON); err != nil {
					return err
				}
				var labels map[string]string
				if len(labelsJSON) > 0 {
					if err := json.Unmarshal(labelsJSON, &labels); err != nil {
						return err
					}
				}
				if !service.GatewaySelector.Matches(labels) {
					return &domain.ApplyError{Code: "selector_mismatch", Path: "service.gateway_selector", Message: "assigned service selector does not match its gateway"}
				}
			}
		}
		revision++
		service.Revision = revision
		service.UpdatedAt = nowTime
		b, marshalErr := json.Marshal(service)
		if marshalErr != nil {
			return marshalErr
		}
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE services SET agent_id=?,document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, service.AgentID, b, revision, now, service.ID, revision-1)
		if err == nil {
			err = requireRevisionWrite(ctx, tx, result, "service", revision-1, `SELECT revision FROM services WHERE id=?`, service.ID)
		}
	}
	if err != nil {
		return err
	}
	if hadPrevious && !sameServiceContent(previous, service) {
		// A service document is consumed by both ends of its assignment. Mark
		// the placement pending and advance the shared assignment generation in
		// the same transaction as the service write; otherwise one node could
		// start using a new local target while its peer still authorizes the old
		// target under an applied assignment.
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
