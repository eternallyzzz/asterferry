package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"asterferry/internal/domain"
)

func (s *Repository) PutAssignment(ctx context.Context, assignment domain.Assignment, options WriteOptions) error {
	if assignment.State == "" {
		assignment.State = domain.AssignmentPending
	}
	if assignment.State == domain.AssignmentApplied {
		return &domain.ApplyError{Code: "state_controller_owned", Path: "state", Message: "assignment state applied is controller-owned"}
	}
	if err := assignment.Validate(); err != nil {
		return err
	}
	if err := s.protectObfuscationPolicy(&assignment.Obfuscation); err != nil {
		return err
	}
	if assignment.Generation > math.MaxInt64 {
		return &domain.ApplyError{Code: "invalid_generation", Path: "generation", Message: "assignment generation exceeds repository limit"}
	}
	requestAssignment := assignment
	requestAssignment.Revision = 0
	requestAssignment.UpdatedAt = time.Time{}
	requestAssignment.Obfuscation = obfuscationRequestPolicy(requestAssignment.Obfuscation)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	idempotentRequest := struct {
		Assignment domain.Assignment `json:"assignment"`
		IfMatch    int64             `json:"if_match"`
	}{Assignment: requestAssignment, IfMatch: options.IfMatch}
	if hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, idempotentRequest); err != nil {
		return err
	} else if hit {
		return nil
	}

	var gatewayKind, agentKind string
	var gatewayEnabled, agentEnabled int
	if err := tx.QueryRowContext(ctx, `SELECT ns.kind,n.enabled FROM nodes n JOIN node_specs ns ON ns.node_id=n.id WHERE n.id=?`, assignment.GatewayID).Scan(&gatewayKind, &gatewayEnabled); err != nil {
		return err
	}
	if gatewayKind != string(domain.NodeSpecGateway) {
		return errors.New("assignment gateway has the wrong kind")
	}
	if gatewayEnabled == 0 {
		return &domain.ApplyError{Code: "node_disabled", Path: "gateway_id", Message: "assignment gateway is disabled"}
	}
	if err := tx.QueryRowContext(ctx, `SELECT ns.kind,n.enabled FROM nodes n JOIN node_specs ns ON ns.node_id=n.id WHERE n.id=?`, assignment.AgentID).Scan(&agentKind, &agentEnabled); err != nil {
		return err
	}
	if agentKind != string(domain.NodeSpecAgent) {
		return errors.New("assignment agent has the wrong kind")
	}
	if agentEnabled == 0 {
		return &domain.ApplyError{Code: "node_disabled", Path: "agent_id", Message: "assignment agent is disabled"}
	}
	gatewayLabels, err := loadNodeLabels(ctx, tx, assignment.GatewayID)
	if err != nil {
		return err
	}
	gatewaySpec, gatewaySpecErr := loadGatewaySpecNormalized(ctx, tx, assignment.GatewayID, 0)
	if gatewaySpecErr != nil && !errors.Is(gatewaySpecErr, sql.ErrNoRows) {
		return gatewaySpecErr
	}
	if gatewaySpecErr == nil && assignment.PublicEndpoint != "" {
		endpointKnown := false
		for _, endpoint := range gatewaySpec.PublicEndpoints {
			if endpoint == assignment.PublicEndpoint {
				endpointKnown = true
				break
			}
		}
		if !endpointKnown {
			return &domain.ApplyError{Code: "endpoint_mismatch", Path: "public_endpoint", Message: fmt.Sprintf("assignment endpoint %q is not advertised by gateway %q", assignment.PublicEndpoint, assignment.GatewayID)}
		}
	}

	previous, previousErr := loadAssignmentNormalized(ctx, tx, assignment.ID)
	hadPrevious := previousErr == nil
	if previousErr != nil && !errors.Is(previousErr, sql.ErrNoRows) {
		return previousErr
	}
	if hadPrevious && assignment.Generation < previous.Generation {
		return &RevisionConflictError{Resource: "assignment_generation", Expected: uint64ToRevision(previous.Generation), Actual: uint64ToRevision(assignment.Generation)}
	}
	superseded, err := supersededAssignments(ctx, tx, assignment.ServiceIDs, assignment.ID)
	if err != nil {
		return err
	}
	serviceDocuments := make(map[string]domain.Service, len(assignment.ServiceIDs))
	for _, serviceID := range assignment.ServiceIDs {
		service, serviceErr := loadServiceNormalized(ctx, tx, serviceID)
		if serviceErr != nil {
			return fmt.Errorf("assignment service %q: %w", serviceID, serviceErr)
		}
		if service.AgentID != assignment.AgentID {
			return fmt.Errorf("assignment service %q belongs to another agent", serviceID)
		}
		if !service.GatewaySelector.Matches(gatewayLabels) {
			return &domain.ApplyError{Code: "selector_mismatch", Path: "service.gateway_selector", Message: fmt.Sprintf("gateway %q does not match service %q selector", assignment.GatewayID, serviceID)}
		}
		serviceDocuments[serviceID] = service
	}
	for _, binding := range assignment.Bindings {
		service, ok := serviceDocuments[binding.ServiceID]
		if !ok {
			return fmt.Errorf("assignment binding service %q is not in service_ids", binding.ServiceID)
		}
		if service.Protocol != binding.Protocol {
			return &domain.ApplyError{Code: "protocol_mismatch", Path: "bindings", Message: fmt.Sprintf("binding protocol for service %q does not match the service", binding.ServiceID)}
		}
		if normalizeBind(service.PublicBind) != normalizeBind(binding.Bind) {
			return &domain.ApplyError{Code: "bind_mismatch", Path: "bindings", Message: fmt.Sprintf("binding address for service %q does not match the service public bind", binding.ServiceID)}
		}
		if service.PublicPort != 0 && service.PublicPort != binding.Port {
			return &domain.ApplyError{Code: "port_mismatch", Path: "bindings", Message: fmt.Sprintf("binding port for service %q does not match the service public port", binding.ServiceID)}
		}
		if gatewaySpecErr == nil && !portInPool(gatewaySpec.PortPool, binding.Protocol, binding.Port) {
			return &domain.ApplyError{Code: "port_outside_pool", Path: "bindings", Message: fmt.Sprintf("binding port %d is outside gateway %q %s port pool", binding.Port, assignment.GatewayID, binding.Protocol)}
		}
		if gatewaySpecErr == nil {
			for _, listener := range gatewaySpec.Listeners {
				if bindingKey(listener.Protocol, listener.Bind, listener.Port) == bindingKey(binding.Protocol, binding.Bind, binding.Port) {
					return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
				}
			}
		}
	}

	var revision int64
	if !hadPrevious {
		revision = 1
		if options.IfMatch > 0 {
			return &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: 0}
		}
	} else {
		revision = previous.Revision
		if options.IfMatch <= 0 || options.IfMatch != revision {
			return &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: revision}
		}
		revision++
	}
	for _, old := range superseded {
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE id=?`, old.ID); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, options.Actor, "supersede", "assignment", old.ID, old.Revision, map[string]string{"replacement": assignment.ID}); err != nil {
			return err
		}
	}
	nowTime := time.Now().UTC()
	assignment.Revision = revision
	assignment.UpdatedAt = nowTime
	if hadPrevious {
		result, err := updateAssignmentColumnsTx(ctx, tx, assignment, revision-1)
		if err != nil {
			return err
		}
		if err := requireRevisionWrite(ctx, tx, result, "assignment", revision-1, `SELECT revision FROM assignments WHERE id=?`, assignment.ID); err != nil {
			return err
		}
	} else if _, err := insertAssignmentColumnsTx(ctx, tx, assignment); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, assignment.ID); err != nil {
		return err
	}
	if err := replaceAssignmentChildrenTx(ctx, tx, assignment); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "upsert", "assignment", assignment.ID, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"id": assignment.ID, "revision": revision}); err != nil {
		return err
	}
	affectedNodes := []string{assignment.GatewayID, assignment.AgentID}
	if hadPrevious {
		affectedNodes = append(affectedNodes, previous.GatewayID, previous.AgentID)
	}
	for _, old := range superseded {
		affectedNodes = append(affectedNodes, old.GatewayID, old.AgentID)
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

// deleteAssignmentBindingsTx releases a placement's public-port occupancy.
// Binding metadata is part of the normalized assignment child aggregate, so
// the assignment row remains the only source of lifecycle diagnostics.
func deleteAssignmentBindingsTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM assignment_bindings WHERE assignment_id=?`, assignment.ID)
	return err
}

func supersededAssignments(ctx context.Context, tx *sql.Tx, serviceIDs []string, assignmentID string) ([]domain.Assignment, error) {
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	placeholders := questionMarks(len(serviceIDs))
	args := make([]any, 0, len(serviceIDs)+1)
	args = append(args, assignmentID)
	for _, serviceID := range serviceIDs {
		args = append(args, serviceID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT a.id FROM assignments a JOIN assignment_services assignment_service ON assignment_service.assignment_id=a.id WHERE a.id<>? AND assignment_service.service_id IN (`+placeholders+`) ORDER BY a.id`, args...)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]domain.Assignment, 0, len(ids))
	for _, id := range ids {
		candidate, err := loadAssignmentNormalized(ctx, tx, id)
		if err != nil {
			return nil, fmt.Errorf("load assignment %q: %w", id, err)
		}
		if candidate.State != domain.AssignmentDegraded && candidate.State != domain.AssignmentDraining {
			return nil, &domain.ApplyError{Code: "resource_conflict", Path: "service_ids", Message: fmt.Sprintf("service is already assigned by %q", candidate.ID)}
		}
		result = append(result, candidate)
	}
	return result, nil
}

func (s *Repository) DeleteAssignment(ctx context.Context, id string, options WriteOptions) error {
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
	assignment, err := loadAssignmentNormalized(ctx, tx, id)
	if err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != assignment.Revision {
		return &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: assignment.Revision}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE id=? AND revision=?`, id, assignment.Revision)
	if err != nil {
		return err
	}
	if err := requireRevisionWrite(ctx, tx, result, "assignment", assignment.Revision, `SELECT revision FROM assignments WHERE id=?`, id); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", "assignment", id, assignment.Revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": assignment.Revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, assignment.GatewayID, assignment.AgentID)
}
