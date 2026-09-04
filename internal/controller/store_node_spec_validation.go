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

// updateAssignmentEndpointsTx keeps derived assignment endpoint and
// obfuscation state consistent with a GatewaySpec edit.
func updateAssignmentEndpointsTx(ctx context.Context, tx *sql.Tx, spec domain.GatewaySpec) error {
	if len(spec.PublicEndpoints) == 0 {
		return errors.New("gateway spec has no public endpoints")
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,revision,generation FROM assignments WHERE gateway_id=? ORDER BY id`, spec.NodeID)
	if err != nil {
		return err
	}
	type assignmentRow struct {
		id         string
		revision   int64
		generation uint64
	}
	assignmentRows := make([]assignmentRow, 0)
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
		assignmentRows = append(assignmentRows, row)
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
		assignment, err := loadAssignmentNormalized(ctx, tx, row.id)
		if err != nil {
			return fmt.Errorf("load assignment %q: %w", row.id, err)
		}
		if assignment.Generation != row.generation {
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
		if assignment.Generation == math.MaxUint64 || row.revision == math.MaxInt64 {
			return errors.New("assignment generation or revision is exhausted")
		}
		assignment.PublicEndpoint = endpoint
		if obfuscationChanged {
			assignment.Obfuscation = spec.Obfuscation
			assignment.Obfuscation.Key = nil
			assignment.Obfuscation.PreviousKey = nil
		}
		assignment.Generation++
		if assignment.State != domain.AssignmentDraining && assignment.State != domain.AssignmentDegraded {
			assignment.State = domain.AssignmentPending
		}
		assignment.Revision = row.revision + 1
		assignment.UpdatedAt = time.Now().UTC()
		result, err := updateAssignmentColumnsTx(ctx, tx, assignment, row.revision)
		if err != nil {
			return err
		}
		if err := requireRevisionWrite(ctx, tx, result, "assignment", row.revision, `SELECT revision FROM assignments WHERE id=?`, assignment.ID); err != nil {
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

func validateGatewaySpecTx(ctx context.Context, tx *sql.Tx, spec domain.GatewaySpec) error {
	rows, err := tx.QueryContext(ctx, `SELECT protocol,bind,port FROM assignment_bindings WHERE gateway_id=?`, spec.NodeID)
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
		labels, err := loadNodeLabels(ctx, tx, reference.gatewayID)
		if err != nil {
			return err
		}
		if !spec.GatewaySelector.Matches(labels) {
			return &domain.ApplyError{Code: "selector_mismatch", Path: "gateway_selector", Message: fmt.Sprintf("agent spec selector no longer matches gateway %q for assignment %q", reference.gatewayID, reference.assignmentID)}
		}
	}
	return nil
}
