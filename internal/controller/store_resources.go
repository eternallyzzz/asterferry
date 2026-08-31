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

// CreateNode persists a node identity and its initial lifecycle state.
func (s *Store) CreateNode(ctx context.Context, node domain.Node, options WriteOptions) error {
	if err := validateNode(node); err != nil {
		return err
	}
	requestNode := node
	requestNode.Revision = 0
	requestNode.CreatedAt = time.Time{}
	requestNode.UpdatedAt = time.Time{}
	idempotentRequest := struct {
		Node domain.Node `json:"node"`
	}{Node: requestNode}
	// Revisions are assigned by the repository.  Accepting a caller-supplied
	// revision would allow a newly created resource to jump over future
	// If-Match values and would make idempotent retries ambiguous.
	node.Revision = 1
	now := time.Now().UTC()
	node.CreatedAt = now
	node.UpdatedAt = now
	labels, err := json.Marshal(node.Labels)
	if err != nil {
		return err
	}
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
	_, err = tx.ExecContext(ctx, `INSERT INTO nodes(id, role, name, labels_json, enabled, certificate_state, certificate_serial, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, node.ID, node.Role, node.Name, labels, boolInt(node.Enabled), defaultCertificateState(node.CertificateState), node.CertificateSerial, node.Revision, node.CreatedAt.Format(time.RFC3339Nano), node.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	if err := insertAudit(ctx, tx, options.Actor, "create", "node", node.ID, node.Revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"id": node.ID, "revision": node.Revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, node.ID)
}

func (s *Store) GetNode(ctx context.Context, id string) (domain.Node, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, role, name, labels_json, enabled, certificate_state, certificate_serial, revision, created_at, updated_at FROM nodes WHERE id = ?`, id)
	return scanNode(row)
}

func (s *Store) ListNodes(ctx context.Context, role string) ([]domain.Node, error) {
	query := `SELECT id, role, name, labels_json, enabled, certificate_state, certificate_serial, revision, created_at, updated_at FROM nodes`
	args := []any{}
	if strings.TrimSpace(role) != "" {
		query += ` WHERE role = ?`
		args = append(args, role)
	}
	query += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Node{}
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

// GatewayView and AgentView are one-query list projections used by the REST
// list endpoints. Keeping the optional spec in the projection avoids an N+1
// Get*Spec query for every node while preserving the existing JSON response.
type GatewayView struct {
	Node domain.Node
	Spec *domain.GatewaySpec
}

type AgentView struct {
	Node domain.Node
	Spec *domain.AgentSpec
}

func (s *Store) ListGatewayViews(ctx context.Context) ([]GatewayView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id,n.role,n.name,n.labels_json,n.enabled,n.certificate_state,n.certificate_serial,n.revision,n.created_at,n.updated_at,g.document_json,g.revision FROM nodes n LEFT JOIN gateway_specs g ON g.node_id=n.id WHERE n.role=? ORDER BY n.id`, domain.RoleGateway)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]GatewayView, 0)
	for rows.Next() {
		var node domain.Node
		var labels, document []byte
		var enabled int
		var created, updated string
		var specRevision sql.NullInt64
		if err := rows.Scan(&node.ID, &node.Role, &node.Name, &labels, &enabled, &node.CertificateState, &node.CertificateSerial, &node.Revision, &created, &updated, &document, &specRevision); err != nil {
			return nil, err
		}
		if len(labels) > 0 {
			if err := json.Unmarshal(labels, &node.Labels); err != nil {
				return nil, err
			}
		}
		node.Enabled = enabled != 0
		var parseErr error
		node.CreatedAt, parseErr = parseStoredTime("node.created_at", created)
		if parseErr != nil {
			return nil, parseErr
		}
		node.UpdatedAt, parseErr = parseStoredTime("node.updated_at", updated)
		if parseErr != nil {
			return nil, parseErr
		}
		if err := node.Validate(); err != nil {
			return nil, fmt.Errorf("stored gateway node is invalid: %w", err)
		}
		view := GatewayView{Node: node}
		if len(document) > 0 && specRevision.Valid {
			var spec domain.GatewaySpec
			if err := json.Unmarshal(document, &spec); err != nil {
				return nil, err
			}
			if spec.NodeID != node.ID {
				return nil, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "gateway.node_id", Message: "stored gateway spec node id does not match its row"}
			}
			spec.Revision = specRevision.Int64
			if err := spec.Validate(); err != nil {
				return nil, fmt.Errorf("stored gateway spec is invalid: %w", err)
			}
			view.Spec = &spec
		}
		result = append(result, view)
	}
	return result, rows.Err()
}

func (s *Store) ListAgentViews(ctx context.Context) ([]AgentView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id,n.role,n.name,n.labels_json,n.enabled,n.certificate_state,n.certificate_serial,n.revision,n.created_at,n.updated_at,a.document_json,a.revision FROM nodes n LEFT JOIN agent_specs a ON a.node_id=n.id WHERE n.role=? ORDER BY n.id`, domain.RoleAgent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AgentView, 0)
	for rows.Next() {
		var node domain.Node
		var labels, document []byte
		var enabled int
		var created, updated string
		var specRevision sql.NullInt64
		if err := rows.Scan(&node.ID, &node.Role, &node.Name, &labels, &enabled, &node.CertificateState, &node.CertificateSerial, &node.Revision, &created, &updated, &document, &specRevision); err != nil {
			return nil, err
		}
		if len(labels) > 0 {
			if err := json.Unmarshal(labels, &node.Labels); err != nil {
				return nil, err
			}
		}
		node.Enabled = enabled != 0
		var parseErr error
		node.CreatedAt, parseErr = parseStoredTime("node.created_at", created)
		if parseErr != nil {
			return nil, parseErr
		}
		node.UpdatedAt, parseErr = parseStoredTime("node.updated_at", updated)
		if parseErr != nil {
			return nil, parseErr
		}
		if err := node.Validate(); err != nil {
			return nil, fmt.Errorf("stored agent node is invalid: %w", err)
		}
		view := AgentView{Node: node}
		if len(document) > 0 && specRevision.Valid {
			var spec domain.AgentSpec
			if err := json.Unmarshal(document, &spec); err != nil {
				return nil, err
			}
			if spec.NodeID != node.ID {
				return nil, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "agent.node_id", Message: "stored agent spec node id does not match its row"}
			}
			spec.Revision = specRevision.Int64
			if err := spec.Validate(); err != nil {
				return nil, fmt.Errorf("stored agent spec is invalid: %w", err)
			}
			view.Spec = &spec
		}
		result = append(result, view)
	}
	return result, rows.Err()
}

func (s *Store) UpdateNode(ctx context.Context, node domain.Node, options WriteOptions) error {
	if err := validateNode(node); err != nil {
		return err
	}
	if options.IfMatch <= 0 {
		return &RevisionConflictError{Resource: "node", Expected: options.IfMatch, Actual: node.Revision}
	}
	requestNode := node
	idempotentRequest := struct {
		Node    domain.Node `json:"node"`
		IfMatch int64       `json:"if_match"`
	}{Node: requestNode, IfMatch: options.IfMatch}
	labels, err := json.Marshal(node.Labels)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
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
	var current int64
	var currentRole, currentCertificateState string
	var currentEnabled int
	if err := tx.QueryRowContext(ctx, `SELECT revision,role,enabled,certificate_state FROM nodes WHERE id = ?`, node.ID).Scan(&current, &currentRole, &currentEnabled, &currentCertificateState); err != nil {
		return err
	}
	if current != options.IfMatch {
		return &RevisionConflictError{Resource: "node", Expected: options.IfMatch, Actual: current}
	}
	if currentRole != node.Role {
		return &domain.ApplyError{Code: "immutable_field", Path: "role", Message: "node role cannot be changed"}
	}
	affectedNodes, err := assignmentParticipantIDsTx(ctx, tx, node.ID)
	if err != nil {
		return err
	}
	affectedNodes = append(affectedNodes, node.ID)
	node.Revision = current + 1
	_, err = tx.ExecContext(ctx, `UPDATE nodes SET role=?, name=?, labels_json=?, enabled=?, certificate_state=?, certificate_serial=?, revision=?, updated_at=? WHERE id=? AND revision=?`, node.Role, node.Name, labels, boolInt(node.Enabled), defaultCertificateState(node.CertificateState), node.CertificateSerial, node.Revision, now.Format(time.RFC3339Nano), node.ID, current)
	if err != nil {
		return err
	}
	// A disabled or non-active identity must not retain an applied placement.
	// Quarantine the assignment rows in this same transaction as the identity
	// update, so a concurrent snapshot builder can never publish a new node
	// status while leaving the old public listeners authorized. The direct
	// control-stream action in the API closes online sessions immediately; this
	// durable state also covers offline nodes and reconnects.
	effectiveCertificateState := defaultCertificateState(node.CertificateState)
	quarantine := !node.Enabled || effectiveCertificateState != domain.CertificateActive
	if currentEnabled == 0 && node.Enabled && effectiveCertificateState == domain.CertificateActive {
		// Re-enabling a node does not restore a previous placement. It remains
		// degraded until the scheduler creates a fresh, acknowledged generation.
		quarantine = false
	}
	if currentCertificateState == domain.CertificateRevoked && effectiveCertificateState == domain.CertificateActive {
		// A certificate-state repair is not a placement acknowledgement. Keep
		// previously quarantined assignments closed until ScheduleAgent creates
		// and both nodes apply a fresh generation.
		quarantine = true
	}
	if quarantine {
		if err := quarantineAssignmentsForNodeTx(ctx, tx, node.ID); err != nil {
			return err
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "update", "node", node.ID, node.Revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"id": node.ID, "revision": node.Revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

// quarantineAssignmentsForNodeTx moves every placement that references an
// unavailable identity to the fail-closed degraded state. It intentionally
// preserves the assignment identity and shared generation: a later scheduler
// pass can replace the Gateway while both peers still have to acknowledge the
// new placement before it becomes applied again.
func quarantineAssignmentsForNodeTx(ctx context.Context, tx *sql.Tx, nodeID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,gateway_id,agent_id,document_json,revision,generation FROM assignments WHERE gateway_id=? OR agent_id=? ORDER BY id`, nodeID, nodeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	now := time.Now().UTC()
	for rows.Next() {
		var id, gatewayID, agentID string
		var document []byte
		var revision int64
		var indexedGeneration uint64
		if err := rows.Scan(&id, &gatewayID, &agentID, &document, &revision, &indexedGeneration); err != nil {
			return err
		}
		var assignment domain.Assignment
		if err := json.Unmarshal(document, &assignment); err != nil {
			return fmt.Errorf("decode assignment %q: %w", id, err)
		}
		if assignment.ID != id || assignment.GatewayID != gatewayID || assignment.AgentID != agentID || assignment.Generation != indexedGeneration {
			return &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment", Message: "stored assignment metadata does not match its row"}
		}
		if assignment.State == "" {
			assignment.State = domain.AssignmentPending
		}
		if assignment.State == domain.AssignmentDegraded {
			// Clear stale acknowledgements even when the state is already degraded;
			// they must never satisfy the barrier after a later repair.
			if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, id); err != nil {
				return err
			}
			if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
				return err
			}
			continue
		}
		if revision == math.MaxInt64 {
			return &domain.ApplyError{Code: "invalid_revision", Path: "assignment.revision", Message: "assignment revision is exhausted"}
		}
		assignment.State = domain.AssignmentDegraded
		assignment.Revision = revision + 1
		assignment.UpdatedAt = now
		updated, err := json.Marshal(assignment)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Revision, now.Format(time.RFC3339Nano), id, revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, id); err != nil {
			return err
		}
		if err := deleteAssignmentBindingsTx(ctx, tx, assignment); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, "system", "node_quarantine", "assignment", id, assignment.Revision, map[string]string{"node_id": nodeID, "state": domain.AssignmentDegraded}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) DeleteNode(ctx context.Context, id string, options WriteOptions) error {
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
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM nodes WHERE id=?`, id).Scan(&revision); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: "node", Expected: options.IfMatch, Actual: revision}
	}
	// Node deletion is intentionally not a cascading business-data operation.
	// A forgotten node must be disabled or its assignments/services removed
	// explicitly first; otherwise a single identity CRUD request could silently
	// erase the last-known desired state for an Agent or release a Gateway's
	// public bindings without an auditable placement change.
	var dependents int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM assignments WHERE gateway_id=? OR agent_id=?) +
		(SELECT COUNT(*) FROM services WHERE agent_id=?) +
		(SELECT COUNT(*) FROM gateway_specs WHERE node_id=?) +
		(SELECT COUNT(*) FROM agent_specs WHERE node_id=?)`, id, id, id, id, id).Scan(&dependents); err != nil {
		return err
	}
	if dependents > 0 {
		return &domain.ApplyError{Code: "resource_conflict", Path: "node", Message: "node has dependent specs, services, or assignments"}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=? AND revision=?`, id, revision); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", "node", id, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": revision}); err != nil {
		return err
	}
	if err := s.commitAndNotifyResources(tx, id); err != nil {
		return err
	}
	if s.metrics != nil {
		s.metrics.removeNode(id)
	}
	return nil
}

func (s *Store) PutGatewaySpec(ctx context.Context, spec domain.GatewaySpec, options WriteOptions) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	node, err := s.GetNode(ctx, spec.NodeID)
	if err != nil {
		return err
	}
	if node.Role != domain.RoleGateway {
		return errors.New("gateway spec node has the wrong role")
	}
	if err := s.protectObfuscationPolicy(&spec.Obfuscation); err != nil {
		return err
	}
	return s.putDocument(ctx, "gateway_specs", spec.NodeID, spec, options)
}

func (s *Store) DeleteGatewaySpec(ctx context.Context, nodeID string, options WriteOptions) error {
	return s.deleteDocument(ctx, "gateway_specs", nodeID, options)
}

func (s *Store) GetGatewaySpec(ctx context.Context, nodeID string) (domain.GatewaySpec, error) {
	var data []byte
	var revision int64
	if err := s.db.QueryRowContext(ctx, `SELECT document_json,revision FROM gateway_specs WHERE node_id=?`, nodeID).Scan(&data, &revision); err != nil {
		return domain.GatewaySpec{}, err
	}
	var spec domain.GatewaySpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return domain.GatewaySpec{}, err
	}
	if spec.NodeID != nodeID {
		return domain.GatewaySpec{}, &domain.ApplyError{Code: "node_mismatch", Path: "gateway.node_id", Message: "stored gateway spec node id does not match its row"}
	}
	spec.Revision = revision
	if err := spec.Validate(); err != nil {
		return domain.GatewaySpec{}, fmt.Errorf("stored gateway spec is invalid: %w", err)
	}
	return spec, nil
}

func (s *Store) PutAgentSpec(ctx context.Context, spec domain.AgentSpec, options WriteOptions) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	node, err := s.GetNode(ctx, spec.NodeID)
	if err != nil {
		return err
	}
	if node.Role != domain.RoleAgent {
		return errors.New("agent spec node has the wrong role")
	}
	return s.putDocument(ctx, "agent_specs", spec.NodeID, spec, options)
}

func (s *Store) DeleteAgentSpec(ctx context.Context, nodeID string, options WriteOptions) error {
	return s.deleteDocument(ctx, "agent_specs", nodeID, options)
}

func (s *Store) GetAgentSpec(ctx context.Context, nodeID string) (domain.AgentSpec, error) {
	var data []byte
	var revision int64
	if err := s.db.QueryRowContext(ctx, `SELECT document_json,revision FROM agent_specs WHERE node_id=?`, nodeID).Scan(&data, &revision); err != nil {
		return domain.AgentSpec{}, err
	}
	var spec domain.AgentSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return domain.AgentSpec{}, err
	}
	if spec.NodeID != nodeID {
		return domain.AgentSpec{}, &domain.ApplyError{Code: "node_mismatch", Path: "agent.node_id", Message: "stored agent spec node id does not match its row"}
	}
	spec.Revision = revision
	if err := spec.Validate(); err != nil {
		return domain.AgentSpec{}, fmt.Errorf("stored agent spec is invalid: %w", err)
	}
	return spec, nil
}

func (s *Store) PutService(ctx context.Context, service domain.Service, options WriteOptions) error {
	if err := service.Validate(); err != nil {
		return err
	}
	node, err := s.GetNode(ctx, service.AgentID)
	if err != nil {
		return err
	}
	if node.Role != domain.RoleAgent {
		return errors.New("service agent has the wrong role")
	}
	return s.putServiceDocument(ctx, service, options)
}

func (s *Store) GetService(ctx context.Context, id string) (domain.Service, error) {
	var data []byte
	var revision int64
	var indexedAgent string
	if err := s.db.QueryRowContext(ctx, `SELECT agent_id,document_json,revision FROM services WHERE id=?`, id).Scan(&indexedAgent, &data, &revision); err != nil {
		return domain.Service{}, err
	}
	var service domain.Service
	if err := json.Unmarshal(data, &service); err != nil {
		return domain.Service{}, err
	}
	if service.ID != id || service.AgentID != indexedAgent {
		return domain.Service{}, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "service", Message: "stored service metadata does not match its row"}
	}
	service.Revision = revision
	if err := service.Validate(); err != nil {
		return domain.Service{}, fmt.Errorf("stored service is invalid: %w", err)
	}
	return service, nil
}

func (s *Store) ListServices(ctx context.Context, agentID string) ([]domain.Service, error) {
	query := `SELECT id,agent_id,document_json,revision FROM services`
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
	defer rows.Close()
	result := []domain.Service{}
	for rows.Next() {
		var id, indexedAgent string
		var data []byte
		var revision int64
		if err := rows.Scan(&id, &indexedAgent, &data, &revision); err != nil {
			return nil, err
		}
		var service domain.Service
		if err := json.Unmarshal(data, &service); err != nil {
			return nil, err
		}
		if service.ID != id || service.AgentID != indexedAgent {
			return nil, &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "service", Message: "stored service metadata does not match its row"}
		}
		service.Revision = revision
		if err := service.Validate(); err != nil {
			return nil, fmt.Errorf("stored service is invalid: %w", err)
		}
		result = append(result, service)
	}
	return result, rows.Err()
}

func (s *Store) DeleteService(ctx context.Context, id string, options WriteOptions) error {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id=? AND revision=?`, id, revision); err != nil {
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
