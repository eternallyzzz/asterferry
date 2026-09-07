package controller

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"asterferry/internal/domain"
)

// Assignment is a relationship aggregate. Its child rows are replaced in
// one transaction so the service order and binding order remain synchronized
// with the scalar assignment revision.
func loadAssignmentNormalized(ctx context.Context, q sqlQueryer, id string) (domain.Assignment, error) {
	var assignment domain.Assignment
	var generation, revision, maxPadding int64
	var shaping int
	var keyCiphertext, previousKeyCiphertext []byte
	var updated string
	if err := q.QueryRowContext(ctx, `SELECT id,gateway_id,agent_id,generation,state,public_endpoint,obfuscation_mode,obfuscation_key_ciphertext,obfuscation_previous_key_ciphertext,obfuscation_key_id,obfuscation_previous_key_id,obfuscation_max_padding_bytes,obfuscation_handshake_shaping,revision,updated_at FROM assignments WHERE id=?`, id).Scan(&assignment.ID, &assignment.GatewayID, &assignment.AgentID, &generation, &assignment.State, &assignment.PublicEndpoint, &assignment.Obfuscation.Mode, &keyCiphertext, &previousKeyCiphertext, &assignment.Obfuscation.KeyID, &assignment.Obfuscation.PreviousKeyID, &maxPadding, &shaping, &revision, &updated); err != nil {
		return domain.Assignment{}, err
	}
	var err error
	assignment.UpdatedAt, err = parseStoredTime("assignment.updated_at", updated)
	if err != nil {
		return domain.Assignment{}, err
	}
	assignment.Generation, err = storedUint64(generation, "assignment generation")
	if err != nil {
		return domain.Assignment{}, err
	}
	assignment.Revision = revision
	assignment.Obfuscation.KeyCiphertext = append([]byte(nil), keyCiphertext...)
	assignment.Obfuscation.PreviousKeyCiphertext = append([]byte(nil), previousKeyCiphertext...)
	assignment.Obfuscation.MaxPaddingBytes = int(maxPadding)
	assignment.Obfuscation.HandshakeShaping = shaping != 0
	if assignment.State == "" {
		assignment.State = domain.AssignmentPending
	}
	rows, err := q.QueryContext(ctx, `SELECT position,service_id FROM assignment_services WHERE assignment_id=? ORDER BY position`, id)
	if err != nil {
		return domain.Assignment{}, err
	}
	for expected := 0; rows.Next(); expected++ {
		var position int64
		var serviceID string
		if err := rows.Scan(&position, &serviceID); err != nil {
			_ = rows.Close()
			return domain.Assignment{}, err
		}
		if err := requireStoredPosition(position, expected, "assignment service"); err != nil {
			_ = rows.Close()
			return domain.Assignment{}, err
		}
		assignment.ServiceIDs = append(assignment.ServiceIDs, serviceID)
	}
	if err := rows.Close(); err != nil {
		return domain.Assignment{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.Assignment{}, err
	}
	rows, err = q.QueryContext(ctx, `SELECT position,service_id,gateway_id,protocol,bind,port FROM assignment_bindings WHERE assignment_id=? ORDER BY position`, id)
	if err != nil {
		return domain.Assignment{}, err
	}
	for expected := 0; rows.Next(); expected++ {
		var position, port int64
		var binding domain.Binding
		var gatewayID string
		if err := rows.Scan(&position, &binding.ServiceID, &gatewayID, &binding.Protocol, &binding.Bind, &port); err != nil {
			_ = rows.Close()
			return domain.Assignment{}, err
		}
		if err := requireStoredPosition(position, expected, "assignment binding"); err != nil {
			_ = rows.Close()
			return domain.Assignment{}, err
		}
		if gatewayID != assignment.GatewayID {
			_ = rows.Close()
			return domain.Assignment{}, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment.bindings", Message: "stored binding gateway does not match assignment"}
		}
		binding.Port, err = storedUint16(port, "assignment binding port")
		if err != nil {
			_ = rows.Close()
			return domain.Assignment{}, err
		}
		assignment.Bindings = append(assignment.Bindings, binding)
	}
	if err := rows.Close(); err != nil {
		return domain.Assignment{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.Assignment{}, err
	}
	if err := assignment.Validate(); err != nil {
		return domain.Assignment{}, fmt.Errorf("stored assignment is invalid: %w", err)
	}
	return assignment, nil
}

func updateAssignmentColumnsTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment, expectedRevision int64) (sql.Result, error) {
	return tx.ExecContext(ctx, `UPDATE assignments SET gateway_id=?,agent_id=?,generation=?,state=?,public_endpoint=?,obfuscation_mode=?,obfuscation_key_ciphertext=?,obfuscation_previous_key_ciphertext=?,obfuscation_key_id=?,obfuscation_previous_key_id=?,obfuscation_max_padding_bytes=?,obfuscation_handshake_shaping=?,revision=?,updated_at=? WHERE id=? AND revision=?`, assignment.GatewayID, assignment.AgentID, assignment.Generation, assignment.State, assignment.PublicEndpoint, assignment.Obfuscation.Mode, nullableBytes(assignment.Obfuscation.KeyCiphertext), nullableBytes(assignment.Obfuscation.PreviousKeyCiphertext), assignment.Obfuscation.KeyID, assignment.Obfuscation.PreviousKeyID, assignment.Obfuscation.MaxPaddingBytes, boolInt(assignment.Obfuscation.HandshakeShaping), assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano), assignment.ID, expectedRevision)
}

func insertAssignmentColumnsTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) (sql.Result, error) {
	return tx.ExecContext(ctx, `INSERT INTO assignments(id,gateway_id,agent_id,generation,state,public_endpoint,obfuscation_mode,obfuscation_key_ciphertext,obfuscation_previous_key_ciphertext,obfuscation_key_id,obfuscation_previous_key_id,obfuscation_max_padding_bytes,obfuscation_handshake_shaping,revision,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, assignment.ID, assignment.GatewayID, assignment.AgentID, assignment.Generation, assignment.State, assignment.PublicEndpoint, assignment.Obfuscation.Mode, nullableBytes(assignment.Obfuscation.KeyCiphertext), nullableBytes(assignment.Obfuscation.PreviousKeyCiphertext), assignment.Obfuscation.KeyID, assignment.Obfuscation.PreviousKeyID, assignment.Obfuscation.MaxPaddingBytes, boolInt(assignment.Obfuscation.HandshakeShaping), assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano))
}

func replaceAssignmentChildrenTx(ctx context.Context, tx *sql.Tx, assignment domain.Assignment) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_services WHERE assignment_id=?`, assignment.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_bindings WHERE assignment_id=?`, assignment.ID); err != nil {
		return err
	}
	for position, serviceID := range assignment.ServiceIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_services(assignment_id,position,service_id) VALUES(?,?,?)`, assignment.ID, position, serviceID); err != nil {
			if isUniqueConstraint(err) {
				return &domain.ApplyError{Code: "resource_conflict", Path: "service_ids", Message: fmt.Sprintf("service %q is already assigned", serviceID)}
			}
			return err
		}
	}
	if assignment.State == domain.AssignmentDegraded || assignment.State == domain.AssignmentDraining {
		return nil
	}
	for position, binding := range assignment.Bindings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_bindings(assignment_id,position,service_id,gateway_id,protocol,bind,port) VALUES(?,?,?,?,?,?,?)`, assignment.ID, position, binding.ServiceID, assignment.GatewayID, binding.Protocol, normalizeBind(binding.Bind), binding.Port); err != nil {
			if isUniqueConstraint(err) {
				return &PortConflictError{GatewayID: assignment.GatewayID, Protocol: binding.Protocol, Bind: binding.Bind, Port: binding.Port}
			}
			return err
		}
	}
	return nil
}
