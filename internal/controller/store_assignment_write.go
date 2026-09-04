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

func (s *Store) PutAssignment(ctx context.Context, assignment domain.Assignment, options WriteOptions) error {
	// API callers may omit the lifecycle field on create. Persist the safe
	// default explicitly so a newly scheduled placement cannot be admitted by
	// a node before both peers acknowledge its generation.
	if assignment.State == "" {
		assignment.State = domain.AssignmentPending
	}
	// `applied` is an observed/controller-owned state.  Allowing a REST caller
	// (or a retrying placement writer) to set it directly would bypass the
	// two-sided acknowledgement barrier and could expose a listener before the
	// Gateway and Agent have both installed the same generation.  The control
	// stream is the only path that may promote an assignment after recording
	// participant acknowledgements.
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
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, idempotentRequest)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	// Node identity and lifecycle checks are authoritative only inside the
	// assignment transaction. A preflight GetNode can race a kind/enable
	// change and allow a placement to commit against a different identity.
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
	var gatewayLabelsJSON []byte
	if err := tx.QueryRowContext(ctx, `SELECT labels_json FROM nodes WHERE id=?`, assignment.GatewayID).Scan(&gatewayLabelsJSON); err != nil {
		return err
	}
	var gatewayLabels map[string]string
	if len(gatewayLabelsJSON) > 0 {
		if err := json.Unmarshal(gatewayLabelsJSON, &gatewayLabels); err != nil {
			return fmt.Errorf("decode gateway labels: %w", err)
		}
	}
	var gatewaySpec domain.GatewaySpec
	var gatewaySpecDocument []byte
	if err := tx.QueryRowContext(ctx, `SELECT document_json FROM node_specs WHERE node_id=? AND kind=?`, assignment.GatewayID, string(domain.NodeSpecGateway)).Scan(&gatewaySpecDocument); err == nil {
		var envelope domain.NodeSpec
		if err := json.Unmarshal(gatewaySpecDocument, &envelope); err != nil {
			return fmt.Errorf("decode gateway spec: %w", err)
		}
		if envelope.Gateway == nil {
			return errors.New("decode gateway spec: typed gateway document is missing")
		}
		gatewaySpec = *envelope.Gateway
		// The public endpoint is a derived part of the assignment and must be
		// one of the currently advertised endpoints.  Without this check an
		// operator could persist a syntactically valid but unreachable endpoint
		// and the Agent would repeatedly dial a value the Gateway never owns.
		if assignment.PublicEndpoint != "" {
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
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var revision int64
	var previous domain.Assignment
	var previousDocument []byte
	hadPrevious := false
	err = tx.QueryRowContext(ctx, `SELECT revision,document_json FROM assignments WHERE id=?`, assignment.ID).Scan(&revision, &previousDocument)
	if err == nil {
		hadPrevious = true
		if decodeErr := json.Unmarshal(previousDocument, &previous); decodeErr != nil {
			return fmt.Errorf("decode existing assignment: %w", decodeErr)
		}
		if assignment.Generation < previous.Generation {
			return &RevisionConflictError{Resource: "assignment_generation", Expected: uint64ToRevision(previous.Generation), Actual: uint64ToRevision(assignment.Generation)}
		}
	}
	// A degraded/draining placement is a relinquished claim during failover.
	// Collect those rows now, but defer deleting them until every validation
	// below has passed so a rejected replacement leaves the old assignment
	// untouched. Active/pending placements remain hard conflicts.
	superseded, supersedeErr := supersededAssignments(ctx, tx, assignment.ServiceIDs, assignment.ID)
	if supersedeErr != nil {
		return supersedeErr
	}
	serviceDocuments := make(map[string]domain.Service, len(assignment.ServiceIDs))
	for _, serviceID := range assignment.ServiceIDs {
		var serviceDocument []byte
		if err := tx.QueryRowContext(ctx, `SELECT document_json FROM services WHERE id=?`, serviceID).Scan(&serviceDocument); err != nil {
			return fmt.Errorf("assignment service %q: %w", serviceID, err)
		}
		var service domain.Service
		if err := json.Unmarshal(serviceDocument, &service); err != nil {
			return fmt.Errorf("assignment service %q: %w", serviceID, err)
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
		serviceProtocol := service.Protocol
		if serviceProtocol != binding.Protocol {
			return &domain.ApplyError{Code: "protocol_mismatch", Path: "bindings", Message: fmt.Sprintf("binding protocol for service %q does not match the service", binding.ServiceID)}
		}
		if normalizeBind(service.PublicBind) != normalizeBind(binding.Bind) {
			return &domain.ApplyError{Code: "bind_mismatch", Path: "bindings", Message: fmt.Sprintf("binding address for service %q does not match the service public bind", binding.ServiceID)}
		}
		if service.PublicPort != 0 && service.PublicPort != binding.Port {
			return &domain.ApplyError{Code: "port_mismatch", Path: "bindings", Message: fmt.Sprintf("binding port for service %q does not match the service public port", binding.ServiceID)}
		}
		if len(gatewaySpecDocument) > 0 && !portInPool(gatewaySpec.PortPool, binding.Protocol, binding.Port) {
			return &domain.ApplyError{Code: "port_outside_pool", Path: "bindings", Message: fmt.Sprintf("binding port %d is outside gateway %q %s port pool", binding.Port, assignment.GatewayID, binding.Protocol)}
		}
		for _, listener := range gatewaySpec.Listeners {
			if bindingKey(listener.Protocol, listener.Bind, listener.Port) == bindingKey(binding.Protocol, binding.Bind, binding.Port) {
				return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
			}
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		revision = 1
		if options.IfMatch > 0 {
			return &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: 0}
		}
		err = nil
	} else if err == nil {
		if options.IfMatch <= 0 || options.IfMatch != revision {
			return &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: revision}
		}
		revision++
	}
	if err != nil {
		return err
	}
	for _, old := range superseded {
		for _, oldServiceID := range old.ServiceIDs {
			if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id=?`, oldServiceID); deleteErr != nil {
				return deleteErr
			}
		}
		if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM assignments WHERE id=?`, old.ID); deleteErr != nil {
			return deleteErr
		}
		if auditErr := insertAudit(ctx, tx, options.Actor, "supersede", "assignment", old.ID, old.Revision, map[string]string{"replacement": assignment.ID}); auditErr != nil {
			return auditErr
		}
	}
	// Revisions and timestamps are repository-owned metadata. Store the
	// canonical values in the document as well as in their indexed columns so
	// a snapshot built from the row is self-consistent.
	nowTime := time.Now().UTC()
	assignment.Revision = revision
	assignment.UpdatedAt = nowTime
	b, err := json.Marshal(assignment)
	if err != nil {
		return err
	}
	if hadPrevious {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE assignments SET gateway_id=?,agent_id=?,document_json=?,generation=?,revision=?,updated_at=? WHERE id=? AND revision=?`, assignment.GatewayID, assignment.AgentID, b, assignment.Generation, revision, nowTime.Format(time.RFC3339Nano), assignment.ID, revision-1)
		if err == nil {
			err = requireRevisionWrite(ctx, tx, result, "assignment", revision-1, `SELECT revision FROM assignments WHERE id=?`, assignment.ID)
		}
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO assignments(id,gateway_id,agent_id,document_json,generation,revision,updated_at) VALUES(?,?,?,?,?,?,?)`, assignment.ID, assignment.GatewayID, assignment.AgentID, b, assignment.Generation, revision, nowTime.Format(time.RFC3339Nano))
	}
	if err != nil {
		return err
	}
	// A placement write invalidates every participant acknowledgement, even
	// when the caller keeps the same generation. The next complete snapshot
	// application must prove that both endpoints have accepted the document;
	// retaining an acknowledgement from an older binding or endpoint would
	// otherwise open a partially applied assignment.
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, assignment.ID); err != nil {
		return err
	}
	for _, serviceID := range assignment.ServiceIDs {
		var serviceAgent string
		if err := tx.QueryRowContext(ctx, `SELECT agent_id FROM services WHERE id=?`, serviceID).Scan(&serviceAgent); err != nil {
			return fmt.Errorf("assignment service %q: %w", serviceID, err)
		}
		if serviceAgent != assignment.AgentID {
			return fmt.Errorf("assignment service %q belongs to another agent", serviceID)
		}
	}
	if err := replaceAssignmentServicesTx(ctx, tx, assignment); err != nil {
		return err
	}
	serviceSet := make(map[string]struct{}, len(assignment.ServiceIDs))
	for _, serviceID := range assignment.ServiceIDs {
		serviceSet[serviceID] = struct{}{}
	}
	if hadPrevious {
		if previous.GatewayID != assignment.GatewayID || previous.AgentID != assignment.AgentID {
			for _, oldServiceID := range previous.ServiceIDs {
				if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id=?`, oldServiceID); deleteErr != nil {
					return deleteErr
				}
			}
		}
		for _, oldServiceID := range previous.ServiceIDs {
			if _, keep := serviceSet[oldServiceID]; !keep {
				if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id=?`, oldServiceID); deleteErr != nil {
					return deleteErr
				}
			}
		}
	}
	// Degraded and draining placements have relinquished their public
	// listeners. Keep the binding metadata in the assignment document for
	// audit/failover diagnostics, but release any rows left by the previous
	// generation instead of re-inserting them into the occupancy index.
	if assignment.State == domain.AssignmentDegraded || assignment.State == domain.AssignmentDraining {
		if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
			return err
		}
	} else {
		for _, binding := range assignment.Bindings {
			if binding.Port == 0 || binding.ServiceID == "" {
				return errors.New("assignment binding requires service id and port")
			}
			if _, ok := serviceSet[binding.ServiceID]; !ok {
				return fmt.Errorf("assignment binding references unknown service %q", binding.ServiceID)
			}
			_, bindingErr := tx.ExecContext(ctx, `INSERT INTO service_bindings(service_id,gateway_id,protocol,bind,port) VALUES(?,?,?,?,?) ON CONFLICT(service_id) DO UPDATE SET gateway_id=excluded.gateway_id,protocol=excluded.protocol,bind=excluded.bind,port=excluded.port`, binding.ServiceID, assignment.GatewayID, binding.Protocol, normalizeBind(binding.Bind), binding.Port)
			if bindingErr != nil {
				if isUniqueConstraint(bindingErr) {
					return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
				}
				return bindingErr
			}
		}
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

func replaceAssignmentServicesTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_services WHERE assignment_id=?`, assignment.ID); err != nil {
		return err
	}
	for _, serviceID := range assignment.ServiceIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_services(assignment_id,service_id) VALUES(?,?)`, assignment.ID, serviceID); err != nil {
			if isUniqueConstraint(err) {
				return &domain.ApplyError{Code: "resource_conflict", Path: "service_ids", Message: fmt.Sprintf("service %q is already assigned", serviceID)}
			}
			return err
		}
	}
	return nil
}

// deleteAssignmentBindingsTx releases a placement's public-port occupancy
// rows while retaining the binding metadata in the assignment document.  A
// degraded/draining assignment is fail-closed and must not block a healthy
// replacement from claiming the same (gateway, protocol, bind, port) tuple.
func deleteAssignmentBindingsTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) error {
	for _, serviceID := range assignment.ServiceIDs {
		if strings.TrimSpace(serviceID) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id=?`, serviceID); err != nil {
			return err
		}
	}
	return nil
}

// supersededAssignments returns older placements that have already entered a
// degraded/draining lifecycle state and claim one of the services in a new
// assignment.  A failover is committed as one transaction: these rows and
// their bindings are removed only after the replacement has passed all
// placement validation.  Healthy or pending claims remain hard conflicts.
func supersededAssignments(ctx context.Context, tx *sql.Tx, serviceIDs []string, assignmentID string) ([]domain.Assignment, error) {
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(serviceIDs)), ",")
	args := make([]any, 0, len(serviceIDs)+1)
	args = append(args, assignmentID)
	for _, serviceID := range serviceIDs {
		args = append(args, serviceID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.document_json FROM assignments a JOIN assignment_services assignment_service ON assignment_service.assignment_id=a.id WHERE a.id<>? AND assignment_service.service_id IN (`+placeholders+`) GROUP BY a.id ORDER BY a.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]struct{})
	result := make([]domain.Assignment, 0)
	for rows.Next() {
		var id string
		var document []byte
		if err := rows.Scan(&id, &document); err != nil {
			return nil, err
		}
		var candidate domain.Assignment
		if err := json.Unmarshal(document, &candidate); err != nil {
			return nil, fmt.Errorf("decode assignment %q: %w", id, err)
		}
		candidate.ID = id
		if candidate.State != domain.AssignmentDegraded && candidate.State != domain.AssignmentDraining {
			return nil, &domain.ApplyError{Code: "resource_conflict", Path: "service_ids", Message: fmt.Sprintf("service is already assigned by %q", candidate.ID)}
		}
		if _, exists := seen[candidate.ID]; exists {
			continue
		}
		seen[candidate.ID] = struct{}{}
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) DeleteAssignment(ctx context.Context, id string, options WriteOptions) error {
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
	var gatewayID, agentID string
	var document []byte
	if err := tx.QueryRowContext(ctx, `SELECT revision,gateway_id,agent_id,document_json FROM assignments WHERE id=?`, id).Scan(&revision, &gatewayID, &agentID, &document); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: "assignment", Expected: options.IfMatch, Actual: revision}
	}
	var assignment domain.Assignment
	if err := json.Unmarshal(document, &assignment); err != nil {
		return err
	}
	for _, serviceID := range assignment.ServiceIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id=?`, serviceID); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE id=? AND revision=?`, id, revision)
	if err != nil {
		return err
	}
	if err := requireRevisionWrite(ctx, tx, result, "assignment", revision, `SELECT revision FROM assignments WHERE id=?`, id); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", "assignment", id, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, gatewayID, agentID)
}
