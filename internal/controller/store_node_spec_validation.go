package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// updateAssignmentEndpointsTx keeps the derived assignment dial target and
// obfuscation policy consistent with a GatewaySpec edit. It intentionally runs
// after the spec row is written but before the transaction commits, so a
// Gateway change can never be observed without its corresponding assignment
// generation.
func updateAssignmentEndpointsTx(ctx context.Context, tx *sql.Tx, spec domain.GatewaySpec) error {
	if len(spec.PublicEndpoints) == 0 {
		return errors.New("gateway spec has no public endpoints")
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,agent_id,document_json,revision,generation FROM assignments WHERE gateway_id=? ORDER BY id`, spec.NodeID)
	if err != nil {
		return err
	}
	type assignmentEndpointRow struct {
		assignment        domain.Assignment
		revision          int64
		indexedGeneration uint64
	}
	assignmentRows := make([]assignmentEndpointRow, 0)
	for rows.Next() {
		var assignment domain.Assignment
		var document []byte
		var revision int64
		var indexedGeneration uint64
		if err := rows.Scan(&assignment.ID, &assignment.AgentID, &document, &revision, &indexedGeneration); err != nil {
			_ = rows.Close()
			return err
		}
		if err := json.Unmarshal(document, &assignment); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode assignment %q: %w", assignment.ID, err)
		}
		assignmentRows = append(assignmentRows, assignmentEndpointRow{assignment: assignment, revision: revision, indexedGeneration: indexedGeneration})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	endpointSet := make(map[string]struct{}, len(spec.PublicEndpoints))
	for _, endpoint := range spec.PublicEndpoints {
		endpointSet[endpoint] = struct{}{}
	}
	for _, row := range assignmentRows {
		assignment := row.assignment
		revision := row.revision
		indexedGeneration := row.indexedGeneration
		if assignment.Generation != indexedGeneration {
			return fmt.Errorf("assignment %q generation index is inconsistent", assignment.ID)
		}
		endpoint := assignment.PublicEndpoint
		if _, exists := endpointSet[endpoint]; !exists {
			endpoint = spec.PublicEndpoints[0]
		}
		endpointChanged := endpoint != assignment.PublicEndpoint
		obfuscationChanged := !sameObfuscationPolicy(assignment.Obfuscation, spec.Obfuscation)
		if !endpointChanged && !obfuscationChanged {
			continue
		}
		if assignment.Generation == math.MaxUint64 {
			return errors.New("assignment generation is exhausted")
		}
		assignment.PublicEndpoint = endpoint
		if obfuscationChanged {
			// GatewaySpec is already protected by PutGatewaySpec before this
			// transaction reaches the derived update. Copy only the protected
			// representation so assignment rows never acquire plaintext keys.
			assignment.Obfuscation = spec.Obfuscation
			assignment.Obfuscation.Key = nil
			assignment.Obfuscation.PreviousKey = nil
		}
		assignment.Generation++
		// Endpoint edits invalidate acknowledgements, but they must not
		// resurrect a placement that was already fail-closed because one of its
		// identities was disabled/revoked or its Gateway was offline. Such a
		// placement stays degraded until the scheduler creates a fresh, healthy
		// generation.
		if assignment.State != domain.AssignmentDraining && assignment.State != domain.AssignmentDegraded {
			assignment.State = domain.AssignmentPending
		}
		assignment.Revision = revision + 1
		assignment.UpdatedAt = time.Now().UTC()
		updated, marshalErr := json.Marshal(assignment)
		if marshalErr != nil {
			return marshalErr
		}
		result, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,generation=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Generation, assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano), assignment.ID, revision)
		if err != nil {
			return err
		}
		if err := requireRevisionWrite(ctx, tx, result, "assignment", revision, `SELECT revision FROM assignments WHERE id=?`, assignment.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, assignment.ID); err != nil {
			return err
		}
		attributes := map[string]string{"gateway_id": spec.NodeID, "generation": fmt.Sprint(assignment.Generation)}
		if endpointChanged {
			attributes["public_endpoint"] = assignment.PublicEndpoint
		}
		if obfuscationChanged {
			attributes["obfuscation_key_id"] = assignment.Obfuscation.KeyID
			attributes["obfuscation_previous_key_id"] = assignment.Obfuscation.PreviousKeyID
		}
		action := "derived_endpoint"
		if obfuscationChanged {
			action = "derived_gateway"
		}
		if err := insertAudit(ctx, tx, "system", action, "assignment", assignment.ID, assignment.Revision, attributes); err != nil {
			return err
		}
	}
	return nil
}

// validateGatewaySpecTx checks constraints that involve the existing
// assignment index. Listener bindings and service bindings share one public
// namespace, so a spec edit must not be able to introduce a collision after
// the document itself has passed structural validation. The check runs in
// the same transaction that writes the spec; the database UNIQUE constraint
// remains the final race-safe guard for concurrent assignment writers.
func validateGatewaySpecTx(ctx context.Context, tx *sql.Tx, spec domain.GatewaySpec) error {
	rows, err := tx.QueryContext(ctx, `SELECT protocol,bind,port FROM service_bindings WHERE gateway_id=?`, spec.NodeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var protocol, bind string
		var port uint16
		if err := rows.Scan(&protocol, &bind, &port); err != nil {
			return err
		}
		if !portInPool(spec.PortPool, protocol, port) {
			return &domain.ApplyError{Code: "port_outside_pool", Path: "port_pool", Message: fmt.Sprintf("existing binding %s:%d is outside the new gateway %s port pool", bind, port, protocol)}
		}
		for _, listener := range spec.Listeners {
			if bindingKey(listener.Protocol, listener.Bind, listener.Port) == bindingKey(protocol, bind, port) {
				return &PortConflictError{GatewayID: spec.NodeID, Protocol: protocol, Bind: bind, Port: port}
			}
		}
	}
	return rows.Err()
}

func validateAgentSpecTx(ctx context.Context, tx *sql.Tx, spec domain.AgentSpec) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,gateway_id FROM assignments WHERE agent_id=?`, spec.NodeID)
	if err != nil {
		return err
	}
	type assignmentReference struct {
		assignmentID string
		gatewayID    string
	}
	assignments := make([]assignmentReference, 0)
	for rows.Next() {
		var reference assignmentReference
		if err := rows.Scan(&reference.assignmentID, &reference.gatewayID); err != nil {
			_ = rows.Close()
			return err
		}
		assignments = append(assignments, reference)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, reference := range assignments {
		assignmentID, gatewayID := reference.assignmentID, reference.gatewayID
		var labelsJSON []byte
		if err := tx.QueryRowContext(ctx, `SELECT labels_json FROM nodes WHERE id=?`, gatewayID).Scan(&labelsJSON); err != nil {
			return err
		}
		var labels map[string]string
		if len(labelsJSON) > 0 {
			if err := json.Unmarshal(labelsJSON, &labels); err != nil {
				return err
			}
		}
		if !spec.GatewaySelector.Matches(labels) {
			return &domain.ApplyError{Code: "selector_mismatch", Path: "gateway_selector", Message: fmt.Sprintf("agent spec selector no longer matches gateway %q for assignment %q", gatewayID, assignmentID)}
		}
	}
	return nil
}
