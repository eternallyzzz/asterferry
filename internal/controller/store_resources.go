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
	_, err = tx.ExecContext(ctx, `INSERT INTO nodes(id, name, labels_json, enabled, certificate_state, certificate_serial, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, node.ID, node.Name, labels, boolInt(node.Enabled), defaultCertificateState(node.CertificateState), node.CertificateSerial, node.Revision, node.CreatedAt.Format(time.RFC3339Nano), node.UpdatedAt.Format(time.RFC3339Nano))
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
	row := s.db.QueryRowContext(ctx, `SELECT id, name, labels_json, enabled, certificate_state, certificate_serial, revision, created_at, updated_at FROM nodes WHERE id = ?`, id)
	node, err := scanNode(row)
	if err != nil {
		return domain.Node{}, err
	}
	if err := s.decorateNodeSpecKind(ctx, &node); err != nil {
		return domain.Node{}, err
	}
	return node, nil
}

func (s *Store) ListNodes(ctx context.Context, kind string) ([]domain.Node, error) {
	kind = strings.TrimSpace(kind)
	query := `SELECT n.id, n.name, n.labels_json, n.enabled, n.certificate_state, n.certificate_serial, n.revision, n.created_at, n.updated_at, ns.kind FROM nodes n LEFT JOIN node_specs ns ON ns.node_id=n.id`
	args := []any{}
	if kind != "" {
		query += ` WHERE ns.kind = ?`
		args = append(args, kind)
	}
	query += ` ORDER BY n.id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	result := []domain.Node{}
	for rows.Next() {
		node, err := scanNodeWithSpecKind(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GatewayView and AgentView are projections for internal callers.
type GatewayView struct {
	Node domain.Node
	Spec *domain.GatewaySpec
}

type AgentView struct {
	Node domain.Node
	Spec *domain.AgentSpec
}

func (s *Store) ListGatewayViews(ctx context.Context) ([]GatewayView, error) {
	views, err := s.listNodeSpecViews(ctx, domain.NodeSpecGateway)
	if err != nil {
		return nil, err
	}
	result := make([]GatewayView, 0, len(views))
	for _, view := range views {
		if view.Spec.Gateway == nil {
			continue
		}
		value := *view.Spec.Gateway
		value.Revision = view.Spec.Revision
		result = append(result, GatewayView{Node: view.Node, Spec: &value})
	}
	return result, nil
}

func (s *Store) ListAgentViews(ctx context.Context) ([]AgentView, error) {
	views, err := s.listNodeSpecViews(ctx, domain.NodeSpecAgent)
	if err != nil {
		return nil, err
	}
	result := make([]AgentView, 0, len(views))
	for _, view := range views {
		if view.Spec.Agent == nil {
			continue
		}
		value := *view.Spec.Agent
		value.Revision = view.Spec.Revision
		result = append(result, AgentView{Node: view.Node, Spec: &value})
	}
	return result, nil
}

type nodeSpecView struct {
	Node domain.Node
	Spec domain.NodeSpec
}

func (s *Store) listNodeSpecViews(ctx context.Context, kind domain.NodeSpecKind) ([]nodeSpecView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id, n.name, n.labels_json, n.enabled, n.certificate_state, n.certificate_serial, n.revision, n.created_at, n.updated_at, ns.kind, ns.document_json, ns.revision, ns.updated_at FROM nodes n INNER JOIN node_specs ns ON ns.node_id=n.id WHERE ns.kind=? ORDER BY n.id`, string(kind))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]nodeSpecView, 0)
	for rows.Next() {
		node, spec, err := scanNodeAndSpec(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, nodeSpecView{Node: node, Spec: spec})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
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
	var currentCertificateState string
	var currentEnabled int
	if err := tx.QueryRowContext(ctx, `SELECT revision,enabled,certificate_state FROM nodes WHERE id = ?`, node.ID).Scan(&current, &currentEnabled, &currentCertificateState); err != nil {
		return err
	}
	if current != options.IfMatch {
		return &RevisionConflictError{Resource: "node", Expected: options.IfMatch, Actual: current}
	}
	affectedNodes, err := assignmentParticipantIDsTx(ctx, tx, node.ID)
	if err != nil {
		return err
	}
	affectedNodes = append(affectedNodes, node.ID)
	node.Revision = current + 1
	result, err := tx.ExecContext(ctx, `UPDATE nodes SET name=?, labels_json=?, enabled=?, certificate_state=?, certificate_serial=?, revision=?, updated_at=? WHERE id=? AND revision=?`, node.Name, labels, boolInt(node.Enabled), defaultCertificateState(node.CertificateState), node.CertificateSerial, node.Revision, now.Format(time.RFC3339Nano), node.ID, current)
	if err != nil {
		return err
	}
	if err := requireRevisionWrite(ctx, tx, result, "node", current, `SELECT revision FROM nodes WHERE id=?`, node.ID); err != nil {
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
	type assignmentQuarantineRow struct {
		id                string
		gatewayID         string
		agentID           string
		document          []byte
		revision          int64
		indexedGeneration uint64
	}
	assignmentRows := make([]assignmentQuarantineRow, 0)
	for rows.Next() {
		var row assignmentQuarantineRow
		if err := rows.Scan(&row.id, &row.gatewayID, &row.agentID, &row.document, &row.revision, &row.indexedGeneration); err != nil {
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
	now := time.Now().UTC()
	for _, row := range assignmentRows {
		id, gatewayID, agentID := row.id, row.gatewayID, row.agentID
		document, revision, indexedGeneration := row.document, row.revision, row.indexedGeneration
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
		result, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Revision, now.Format(time.RFC3339Nano), id, revision)
		if err != nil {
			return err
		}
		if err := requireRevisionWrite(ctx, tx, result, "assignment", revision, `SELECT revision FROM assignments WHERE id=?`, id); err != nil {
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
	return nil
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
	// Specs are node-owned configuration and are safe to remove with an
	// otherwise unused identity. Assignments and services remain explicit
	// business dependencies: deleting a node must never silently remove a live
	// placement or an Agent's service definitions.
	var dependents int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM assignments WHERE gateway_id=? OR agent_id=?) +
		(SELECT COUNT(*) FROM services WHERE agent_id=?)`, id, id, id).Scan(&dependents); err != nil {
		return err
	}
	if dependents > 0 {
		return &domain.ApplyError{Code: "resource_conflict", Path: "node", Message: "node has dependent services or assignments"}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=? AND revision=?`, id, revision)
	if err != nil {
		return err
	}
	if err := requireRevisionWrite(ctx, tx, result, "node", revision, `SELECT revision FROM nodes WHERE id=?`, id); err != nil {
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
	return s.PutNodeSpec(ctx, domain.NewGatewayNodeSpec(spec), options)
}

func (s *Store) DeleteGatewaySpec(ctx context.Context, nodeID string, options WriteOptions) error {
	return s.DeleteNodeSpec(ctx, nodeID, options)
}

func (s *Store) GetGatewaySpec(ctx context.Context, nodeID string) (domain.GatewaySpec, error) {
	spec, err := s.GetNodeSpec(ctx, nodeID)
	if err != nil {
		return domain.GatewaySpec{}, err
	}
	if spec.Kind != domain.NodeSpecGateway || spec.Gateway == nil {
		return domain.GatewaySpec{}, sql.ErrNoRows
	}
	value := *spec.Gateway
	value.Revision = spec.Revision
	return value, nil
}

func (s *Store) PutAgentSpec(ctx context.Context, spec domain.AgentSpec, options WriteOptions) error {
	return s.PutNodeSpec(ctx, domain.NewAgentNodeSpec(spec), options)
}

func (s *Store) DeleteAgentSpec(ctx context.Context, nodeID string, options WriteOptions) error {
	return s.DeleteNodeSpec(ctx, nodeID, options)
}

func (s *Store) GetAgentSpec(ctx context.Context, nodeID string) (domain.AgentSpec, error) {
	spec, err := s.GetNodeSpec(ctx, nodeID)
	if err != nil {
		return domain.AgentSpec{}, err
	}
	if spec.Kind != domain.NodeSpecAgent || spec.Agent == nil {
		return domain.AgentSpec{}, sql.ErrNoRows
	}
	value := *spec.Agent
	value.Revision = spec.Revision
	return value, nil
}

func (s *Store) PutService(ctx context.Context, service domain.Service, options WriteOptions) error {
	if err := service.Validate(); err != nil {
		return err
	}
	spec, specErr := s.GetNodeSpec(ctx, service.AgentID)
	if specErr != nil {
		return specErr
	}
	if spec.Kind != domain.NodeSpecAgent {
		return errors.New("service agent has the wrong node kind")
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
	result, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id=? AND revision=?`, id, revision)
	if err != nil {
		return err
	}
	if err := requireRevisionWrite(ctx, tx, result, "service", revision, `SELECT revision FROM services WHERE id=?`, id); err != nil {
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
