package controller

import (
	"context"
	"fmt"

	"asterferry/internal/domain"
)

func loadAssignmentsBatchNormalized(ctx context.Context, q sqlQueryer, gateways []domain.Node) (map[string][]domain.Assignment, error) {
	gatewayIDs := make([]string, 0, len(gateways))
	for _, gateway := range gateways {
		gatewayIDs = append(gatewayIDs, gateway.ID)
	}
	result := make(map[string][]domain.Assignment, len(gatewayIDs))
	assignments, err := loadAssignmentsByIDsNormalized(ctx, q, gatewayIDs, "gateway_id")
	if err != nil {
		return nil, err
	}
	for _, assignment := range assignments {
		result[assignment.GatewayID] = append(result[assignment.GatewayID], assignment)
	}
	return result, nil
}

func loadAssignmentsByIDsNormalized(ctx context.Context, q sqlQueryer, ids []string, filterColumn string) ([]domain.Assignment, error) {
	if filterColumn != "id" && filterColumn != "gateway_id" && filterColumn != "agent_id" {
		return nil, fmt.Errorf("unsupported assignment batch filter %q", filterColumn)
	}
	result := make([]domain.Assignment, 0)
	for start := 0; start < len(ids); {
		chunk, args, end := batchIDs(ids, start, gatewayCandidateBatchSize)
		rows, err := q.QueryContext(ctx, `SELECT id,gateway_id,agent_id,generation,state,public_endpoint,obfuscation_mode,obfuscation_key_ciphertext,obfuscation_previous_key_ciphertext,obfuscation_key_id,obfuscation_previous_key_id,obfuscation_max_padding_bytes,obfuscation_handshake_shaping,revision,updated_at FROM assignments WHERE `+filterColumn+` IN (`+questionMarks(len(chunk))+`) ORDER BY `+filterColumn+`,id`, args...)
		if err != nil {
			return nil, err
		}
		byID := make(map[string]*domain.Assignment)
		orderedIDs := make([]string, 0)
		for rows.Next() {
			var assignment domain.Assignment
			var generation, maxPadding, revision int64
			var shaping int
			var keyCiphertext, previousKeyCiphertext []byte
			var updated string
			if err := rows.Scan(&assignment.ID, &assignment.GatewayID, &assignment.AgentID, &generation, &assignment.State, &assignment.PublicEndpoint, &assignment.Obfuscation.Mode, &keyCiphertext, &previousKeyCiphertext, &assignment.Obfuscation.KeyID, &assignment.Obfuscation.PreviousKeyID, &maxPadding, &shaping, &revision, &updated); err != nil {
				_ = rows.Close()
				return nil, err
			}
			assignment.Generation, err = storedUint64(generation, "assignment generation")
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			assignment.Revision = revision
			assignment.Obfuscation.KeyCiphertext = append([]byte(nil), keyCiphertext...)
			assignment.Obfuscation.PreviousKeyCiphertext = append([]byte(nil), previousKeyCiphertext...)
			assignment.Obfuscation.MaxPaddingBytes = int(maxPadding)
			assignment.Obfuscation.HandshakeShaping = shaping != 0
			assignment.UpdatedAt, err = parseStoredTime("assignment.updated_at", updated)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			if assignment.State == "" {
				assignment.State = domain.AssignmentPending
			}
			byID[assignment.ID] = &assignment
			orderedIDs = append(orderedIDs, assignment.ID)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(orderedIDs) > 0 {
			childArgs := make([]any, len(orderedIDs))
			for index, id := range orderedIDs {
				childArgs[index] = id
			}
			rows, err = q.QueryContext(ctx, `SELECT assignment_id,position,service_id FROM assignment_services WHERE assignment_id IN (`+questionMarks(len(orderedIDs))+`) ORDER BY assignment_id,position`, childArgs...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id, serviceID string
				var position int64
				if err := rows.Scan(&id, &position, &serviceID); err != nil {
					_ = rows.Close()
					return nil, err
				}
				if assignment := byID[id]; assignment != nil {
					assignment.ServiceIDs = append(assignment.ServiceIDs, serviceID)
				}
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
			rows, err = q.QueryContext(ctx, `SELECT assignment_id,position,service_id,gateway_id,protocol,bind,port FROM assignment_bindings WHERE assignment_id IN (`+questionMarks(len(orderedIDs))+`) ORDER BY assignment_id,position`, childArgs...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id, serviceID, gatewayID, protocol, bind string
				var position, port int64
				if err := rows.Scan(&id, &position, &serviceID, &gatewayID, &protocol, &bind, &port); err != nil {
					_ = rows.Close()
					return nil, err
				}
				if assignment := byID[id]; assignment != nil {
					if gatewayID != assignment.GatewayID {
						_ = rows.Close()
						return nil, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment.bindings", Message: "stored binding gateway does not match assignment"}
					}
					bindingPort, err := storedUint16(port, "assignment binding port")
					if err != nil {
						_ = rows.Close()
						return nil, err
					}
					assignment.Bindings = append(assignment.Bindings, domain.Binding{ServiceID: serviceID, Protocol: protocol, Bind: bind, Port: bindingPort})
				}
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
		for _, id := range orderedIDs {
			assignment := byID[id]
			if err := assignment.Validate(); err != nil {
				return nil, fmt.Errorf("stored assignment is invalid: %w", err)
			}
			result = append(result, *assignment)
		}
		start = end
	}
	return result, nil
}
