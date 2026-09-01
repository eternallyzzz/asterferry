package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"asterferry/internal/domain"
)

// ApplySnapshot commits a complete desired state and its derived resources in
// one transaction. A failed resource or audit insert rolls the whole write
// back, so nodes never receive a partially updated generation.
func (s *Store) ApplySnapshot(ctx context.Context, snapshot domain.DesiredSnapshot, options WriteOptions) error {
	for index := range snapshot.Assignments {
		if snapshot.Assignments[index].State == "" {
			snapshot.Assignments[index].State = domain.AssignmentPending
		}
		// AssignmentApplied is an observed/controller-owned state.  A complete
		// snapshot write is allowed to publish pending, degraded, or draining
		// placement data, but it must never be used as a shortcut to open a
		// listener without the two-sided Gateway/Agent acknowledgement barrier.
		if snapshot.Assignments[index].State == domain.AssignmentApplied {
			return &domain.ApplyError{Code: "state_controller_owned", Path: fmt.Sprintf("assignments[%d].state", index), Message: "assignment state applied is controller-owned"}
		}
	}
	if snapshot.Gateway != nil {
		if err := s.protectObfuscationPolicy(&snapshot.Gateway.Obfuscation); err != nil {
			return err
		}
	}
	for index := range snapshot.Assignments {
		if err := s.protectObfuscationPolicy(&snapshot.Assignments[index].Obfuscation); err != nil {
			return fmt.Errorf("assignment %q obfuscation: %w", snapshot.Assignments[index].ID, err)
		}
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if snapshot.Generation > math.MaxInt64 {
		return &domain.ApplyError{Code: "invalid_generation", Path: "generation", Message: "generation exceeds repository limit"}
	}
	withChecksum, err := snapshot.WithChecksum()
	if err != nil {
		return err
	}
	if snapshot.Checksum != "" && !strings.EqualFold(snapshot.Checksum, withChecksum.Checksum) {
		return &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "snapshot checksum does not match its content"}
	}
	snapshot = withChecksum
	document, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if len(document) > maxSnapshotDocument {
		return &domain.ApplyError{Code: "message_too_large", Message: "desired snapshot exceeds repository limit"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	requestSnapshot := snapshotForIdempotency(snapshot)
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, requestSnapshot)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	var currentGeneration uint64
	var current []byte
	err = tx.QueryRowContext(ctx, `SELECT generation,document_json FROM desired_snapshots WHERE node_id=?`, snapshot.NodeID).Scan(&currentGeneration, &current)
	if err == nil && snapshot.Generation <= currentGeneration {
		expected := currentGeneration
		if expected < math.MaxUint64 {
			expected++
		}
		return &RevisionConflictError{Resource: "desired_snapshot", Expected: uint64ToRevision(expected), Actual: uint64ToRevision(snapshot.Generation)}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if snapshot.Gateway != nil {
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM nodes WHERE id=?`, snapshot.Gateway.NodeID).Scan(&role); err != nil {
			return err
		}
		if role != domain.RoleGateway {
			return errors.New("snapshot gateway node has the wrong role")
		}
	}
	if snapshot.Agent != nil {
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM nodes WHERE id=?`, snapshot.Agent.NodeID).Scan(&role); err != nil {
			return err
		}
		if role != domain.RoleAgent {
			return errors.New("snapshot agent node has the wrong role")
		}
	}
	// A gateway snapshot is complete for that gateway, so replacing its
	// binding rows first releases ports removed from the desired generation.
	// An agent snapshot is scoped to the agent and only releases bindings for
	// the services it owns.
	if snapshot.Gateway != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE gateway_id=?`, snapshot.NodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE gateway_id=?`, snapshot.NodeID); err != nil {
			return err
		}
	} else {
		// Agent snapshots are complete for the agent. Remove all of its old
		// bindings, assignments and service documents before inserting the new
		// generation; the surrounding transaction restores them if validation
		// or any later insert fails.
		if _, err := tx.ExecContext(ctx, `DELETE FROM service_bindings WHERE service_id IN (SELECT id FROM services WHERE agent_id=?)`, snapshot.NodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE agent_id=?`, snapshot.NodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE agent_id=?`, snapshot.NodeID); err != nil {
			return err
		}
	}
	// A node-scoped snapshot can be applied independently while another node
	// is offline. Re-check the global service/assignment invariants after the
	// scoped cleanup so a partial apply cannot silently steal a service or a
	// public binding from a different assignment.
	for _, service := range snapshot.Services {
		var existingAgent string
		if err := tx.QueryRowContext(ctx, `SELECT agent_id FROM services WHERE id=?`, service.ID).Scan(&existingAgent); err == nil {
			if existingAgent != service.AgentID {
				return &domain.ApplyError{Code: "resource_conflict", Path: "services", Message: fmt.Sprintf("service %q belongs to another agent", service.ID)}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	for _, assignment := range snapshot.Assignments {
		// Assignment IDs are stable identities. A node-scoped snapshot may
		// replace rows owned by that node, but it must never overwrite an
		// assignment that belongs to a different Gateway/Agent pair. The
		// regular PutAssignment path intentionally permits an atomic failover;
		// snapshots, however, are complete documents and an owner mismatch is
		// always a stale or corrupted Controller view.
		var existingGateway, existingAgent string
		if err := tx.QueryRowContext(ctx, `SELECT gateway_id,agent_id FROM assignments WHERE id=?`, assignment.ID).Scan(&existingGateway, &existingAgent); err == nil {
			if existingGateway != assignment.GatewayID || existingAgent != assignment.AgentID {
				return &domain.ApplyError{Code: "resource_conflict", Path: "assignments", Message: fmt.Sprintf("assignment %q belongs to another gateway or agent", assignment.ID)}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, serviceID := range assignment.ServiceIDs {
			assigned, err := serviceAssignedElsewhere(ctx, tx, serviceID, assignment.ID)
			if err != nil {
				return err
			}
			if assigned {
				return &domain.ApplyError{Code: "resource_conflict", Path: "assignments", Message: fmt.Sprintf("service %q is already assigned", serviceID)}
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO desired_snapshots(node_id,generation,checksum,document_json,created_at) VALUES(?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET generation=excluded.generation,checksum=excluded.checksum,document_json=excluded.document_json,created_at=excluded.created_at`, snapshot.NodeID, snapshot.Generation, snapshot.Checksum, document, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if snapshot.Gateway != nil {
		value, err := json.Marshal(snapshot.Gateway)
		if err != nil {
			return fmt.Errorf("encode gateway snapshot: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_specs(node_id,document_json,revision,updated_at) VALUES(?,?,1,?) ON CONFLICT(node_id) DO UPDATE SET document_json=excluded.document_json,revision=gateway_specs.revision+1,updated_at=excluded.updated_at`, snapshot.Gateway.NodeID, value, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if snapshot.Agent != nil {
		value, err := json.Marshal(snapshot.Agent)
		if err != nil {
			return fmt.Errorf("encode agent snapshot: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_specs(node_id,document_json,revision,updated_at) VALUES(?,?,1,?) ON CONFLICT(node_id) DO UPDATE SET document_json=excluded.document_json,revision=agent_specs.revision+1,updated_at=excluded.updated_at`, snapshot.Agent.NodeID, value, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if snapshot.Agent != nil {
		// Agent snapshots are authoritative for the services owned by that
		// Agent, so replace their documents in the same transaction.
		for _, service := range snapshot.Services {
			value, err := json.Marshal(service)
			if err != nil {
				return fmt.Errorf("encode service %q: %w", service.ID, err)
			}
			var serviceRole string
			if err := tx.QueryRowContext(ctx, `SELECT role FROM nodes WHERE id=?`, service.AgentID).Scan(&serviceRole); err != nil {
				return fmt.Errorf("service %q agent: %w", service.ID, err)
			}
			if serviceRole != domain.RoleAgent {
				return fmt.Errorf("service %q agent has the wrong role", service.ID)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO services(id,agent_id,document_json,revision,updated_at) VALUES(?,?,?,1,?) ON CONFLICT(id) DO UPDATE SET agent_id=excluded.agent_id,document_json=excluded.document_json,revision=services.revision+1,updated_at=excluded.updated_at`, service.ID, service.AgentID, value, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
	} else {
		// A Gateway snapshot only references service documents owned by Agents.
		// Never let a stale Gateway stream overwrite those authoritative rows;
		// require the referenced content to match what is already stored.
		for _, service := range snapshot.Services {
			var existingDocument []byte
			if err := tx.QueryRowContext(ctx, `SELECT document_json FROM services WHERE id=?`, service.ID).Scan(&existingDocument); err != nil {
				return fmt.Errorf("gateway snapshot service %q: %w", service.ID, err)
			}
			var existing domain.Service
			if err := json.Unmarshal(existingDocument, &existing); err != nil {
				return fmt.Errorf("decode gateway snapshot service %q: %w", service.ID, err)
			}
			if !sameServiceContent(existing, service) {
				return &domain.ApplyError{Code: "resource_conflict", Path: "services", Message: fmt.Sprintf("gateway snapshot service %q is not current", service.ID)}
			}
		}
	}
	for _, assignment := range snapshot.Assignments {
		if err := assignment.Validate(); err != nil {
			return err
		}
		var gatewayRole, agentRole string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM nodes WHERE id=?`, assignment.GatewayID).Scan(&gatewayRole); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT role FROM nodes WHERE id=?`, assignment.AgentID).Scan(&agentRole); err != nil {
			return err
		}
		if gatewayRole != domain.RoleGateway || agentRole != domain.RoleAgent {
			return errors.New("assignment node roles are invalid")
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
		var gatewayPortPool domain.PortPool
		var gatewaySpecDocument []byte
		var gatewaySpec domain.GatewaySpec
		haveGatewaySpec := false
		if snapshot.Gateway != nil && snapshot.Gateway.NodeID == assignment.GatewayID {
			gatewayPortPool = snapshot.Gateway.PortPool
			gatewaySpec = *snapshot.Gateway
			haveGatewaySpec = true
		} else if err := tx.QueryRowContext(ctx, `SELECT document_json FROM gateway_specs WHERE node_id=?`, assignment.GatewayID).Scan(&gatewaySpecDocument); err == nil {
			if err := json.Unmarshal(gatewaySpecDocument, &gatewaySpec); err != nil {
				return fmt.Errorf("decode gateway spec: %w", err)
			}
			gatewayPortPool = gatewaySpec.PortPool
			haveGatewaySpec = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		serviceSet := make(map[string]struct{}, len(assignment.ServiceIDs))
		for _, serviceID := range assignment.ServiceIDs {
			var serviceAgent string
			var serviceDocument []byte
			if err := tx.QueryRowContext(ctx, `SELECT agent_id,document_json FROM services WHERE id=?`, serviceID).Scan(&serviceAgent, &serviceDocument); err != nil {
				return fmt.Errorf("assignment service %q: %w", serviceID, err)
			}
			if serviceAgent != assignment.AgentID {
				return fmt.Errorf("assignment service %q belongs to another agent", serviceID)
			}
			var service domain.Service
			if err := json.Unmarshal(serviceDocument, &service); err != nil {
				return fmt.Errorf("assignment service %q: %w", serviceID, err)
			}
			if !service.GatewaySelector.Matches(gatewayLabels) {
				return &domain.ApplyError{Code: "selector_mismatch", Path: "assignments", Message: fmt.Sprintf("gateway %q does not match service %q selector", assignment.GatewayID, serviceID)}
			}
			serviceSet[serviceID] = struct{}{}
		}
		value, err := json.Marshal(assignment)
		if err != nil {
			return fmt.Errorf("encode assignment %q: %w", assignment.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignments(id,gateway_id,agent_id,document_json,generation,revision,updated_at) VALUES(?,?,?,?,?,1,?) ON CONFLICT(id) DO UPDATE SET gateway_id=excluded.gateway_id,agent_id=excluded.agent_id,document_json=excluded.document_json,generation=excluded.generation,revision=assignments.revision+1,updated_at=excluded.updated_at`, assignment.ID, assignment.GatewayID, assignment.AgentID, value, assignment.Generation, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if err := replaceAssignmentServicesTx(ctx, tx, assignment); err != nil {
			return err
		}
		// Degraded and draining placements have relinquished their public
		// listeners. Keep binding metadata in the assignment document for
		// diagnostics/failover, but do not reinsert it into the occupancy index.
		if assignment.State != domain.AssignmentDegraded && assignment.State != domain.AssignmentDraining {
			for _, binding := range assignment.Bindings {
				if _, ok := serviceSet[binding.ServiceID]; !ok {
					return fmt.Errorf("assignment binding references unknown service %q", binding.ServiceID)
				}
				if (snapshot.Gateway != nil || len(gatewaySpecDocument) > 0) && !portInPool(gatewayPortPool, binding.Protocol, binding.Port) {
					return &domain.ApplyError{Code: "port_outside_pool", Path: "assignments", Message: fmt.Sprintf("binding port %d is outside gateway %q %s port pool", binding.Port, assignment.GatewayID, binding.Protocol)}
				}
				if haveGatewaySpec {
					for _, listener := range gatewaySpec.Listeners {
						if bindingKey(listener.Protocol, listener.Bind, listener.Port) == bindingKey(binding.Protocol, binding.Bind, binding.Port) {
							return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
						}
					}
				}
				if _, bindingErr := tx.ExecContext(ctx, `INSERT INTO service_bindings(service_id,gateway_id,protocol,bind,port) VALUES(?,?,?,?,?) ON CONFLICT(service_id) DO UPDATE SET gateway_id=excluded.gateway_id,protocol=excluded.protocol,bind=excluded.bind,port=excluded.port`, binding.ServiceID, assignment.GatewayID, binding.Protocol, normalizeBind(binding.Bind), binding.Port); bindingErr != nil {
					if isSQLiteUniqueConstraint(bindingErr) {
						return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
					}
					return bindingErr
				}
			}
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "apply", "desired_snapshot", snapshot.NodeID, int64(snapshot.Generation), map[string]string{"checksum": snapshot.Checksum}); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, requestSnapshot, map[string]any{"node_id": snapshot.NodeID, "generation": snapshot.Generation, "checksum": snapshot.Checksum}); err != nil {
		return err
	}
	affectedNodes := []string{snapshot.NodeID}
	for _, assignment := range snapshot.Assignments {
		affectedNodes = append(affectedNodes, assignment.GatewayID, assignment.AgentID)
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

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
	// assignment transaction. A preflight GetNode can race a role/enable
	// change and allow a placement to commit against a different identity.
	var gatewayRole, agentRole string
	var gatewayEnabled, agentEnabled int
	if err := tx.QueryRowContext(ctx, `SELECT role,enabled FROM nodes WHERE id=?`, assignment.GatewayID).Scan(&gatewayRole, &gatewayEnabled); err != nil {
		return err
	}
	if gatewayRole != domain.RoleGateway {
		return errors.New("assignment gateway has the wrong role")
	}
	if gatewayEnabled == 0 {
		return &domain.ApplyError{Code: "node_disabled", Path: "gateway_id", Message: "assignment gateway is disabled"}
	}
	if err := tx.QueryRowContext(ctx, `SELECT role,enabled FROM nodes WHERE id=?`, assignment.AgentID).Scan(&agentRole, &agentEnabled); err != nil {
		return err
	}
	if agentRole != domain.RoleAgent {
		return errors.New("assignment agent has the wrong role")
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
	if err := tx.QueryRowContext(ctx, `SELECT document_json FROM gateway_specs WHERE node_id=?`, assignment.GatewayID).Scan(&gatewaySpecDocument); err == nil {
		if err := json.Unmarshal(gatewaySpecDocument, &gatewaySpec); err != nil {
			return fmt.Errorf("decode gateway spec: %w", err)
		}
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
		_, err = tx.ExecContext(ctx, `UPDATE assignments SET gateway_id=?,agent_id=?,document_json=?,generation=?,revision=?,updated_at=? WHERE id=? AND revision=?`, assignment.GatewayID, assignment.AgentID, b, assignment.Generation, revision, nowTime.Format(time.RFC3339Nano), assignment.ID, revision-1)
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
				if isSQLiteUniqueConstraint(bindingErr) {
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

func serviceAssignedElsewhere(ctx context.Context, tx *sql.Tx, serviceID, assignmentID string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignment_services WHERE service_id=? AND assignment_id<>?`, serviceID, assignmentID).Scan(&count)
	return count > 0, err
}

func replaceAssignmentServicesTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_services WHERE assignment_id=?`, assignment.ID); err != nil {
		return err
	}
	for _, serviceID := range assignment.ServiceIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_services(assignment_id,service_id) VALUES(?,?)`, assignment.ID, serviceID); err != nil {
			if isSQLiteUniqueConstraint(err) {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE id=? AND revision=?`, id, revision); err != nil {
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
	if _, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, document, assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano), id, revision); err != nil {
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
	rows, err := tx.QueryContext(ctx, `SELECT id,gateway_id,agent_id,document_json,revision,generation FROM assignments WHERE gateway_id=? OR agent_id=? ORDER BY id`, nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changed := make([]domain.Assignment, 0)
	now := time.Now().UTC()
	for rows.Next() {
		var assignment domain.Assignment
		var document []byte
		var revision int64
		var assignmentGeneration uint64
		if err := rows.Scan(&assignment.ID, &assignment.GatewayID, &assignment.AgentID, &document, &revision, &assignmentGeneration); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(document, &assignment); err != nil {
			return nil, fmt.Errorf("decode assignment %q: %w", assignment.ID, err)
		}
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
		if _, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Revision, now.Format(time.RFC3339Nano), assignment.ID, revision); err != nil {
			return nil, err
		}
		if err := insertAudit(ctx, tx, actor, "state", "assignment", assignment.ID, assignment.Revision, map[string]string{"state": targetState, "node_id": nodeID, "generation": fmt.Sprint(generation)}); err != nil {
			return nil, err
		}
		changed = append(changed, assignment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
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
