package controller

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"asterferry/internal/domain"
)

// CreateNode persists a node identity and its initial lifecycle state.
func (s *ResourceRepository) CreateNode(ctx context.Context, node domain.Node, options WriteOptions) error {
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
	tx, err := s.beginWriteTx(ctx)
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
	_, err = tx.ExecContext(ctx, `INSERT INTO nodes(id,name,enabled,certificate_state,certificate_serial,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`, node.ID, node.Name, boolInt(node.Enabled), defaultCertificateState(node.CertificateState), node.CertificateSerial, node.Revision, node.CreatedAt.Format(time.RFC3339Nano), node.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	if err := insertNodeLabelsTx(ctx, tx, node.ID, node.Labels); err != nil {
		return fmt.Errorf("create node labels: %w", err)
	}
	if err := insertAudit(ctx, tx, options.Actor, "create", "node", node.ID, node.Revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"id": node.ID, "revision": node.Revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(ctx, tx, node.ID)
}

func (s *ResourceRepository) GetNode(ctx context.Context, id string) (domain.Node, error) {
	node, err := loadNodeIdentity(ctx, s.db, id)
	if err != nil {
		return domain.Node{}, err
	}
	if err := s.decorateNodeSpecKind(ctx, &node); err != nil {
		return domain.Node{}, err
	}
	return node, nil
}

func (s *ResourceRepository) ListNodes(ctx context.Context, kind string) ([]domain.Node, error) {
	kind = strings.TrimSpace(kind)
	query := `SELECT n.id,n.name,n.enabled,n.certificate_state,n.certificate_serial,n.revision,n.created_at,n.updated_at,ns.kind FROM nodes n LEFT JOIN node_specs ns ON ns.node_id=n.id`
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
	type nodeRow struct {
		node domain.Node
		kind sql.NullString
	}
	rowsData := make([]nodeRow, 0)
	for rows.Next() {
		var node domain.Node
		var enabled int
		var created, updated string
		var specKind sql.NullString
		err := rows.Scan(&node.ID, &node.Name, &enabled, &node.CertificateState, &node.CertificateSerial, &node.Revision, &created, &updated, &specKind)
		if err != nil {
			return nil, err
		}
		node.Enabled = enabled != 0
		node.CreatedAt, err = parseStoredTime("node.created_at", created)
		if err != nil {
			return nil, err
		}
		node.UpdatedAt, err = parseStoredTime("node.updated_at", updated)
		if err != nil {
			return nil, err
		}
		if err := node.Validate(); err != nil {
			return nil, fmt.Errorf("stored node is invalid: %w", err)
		}
		rowsData = append(rowsData, nodeRow{node: node, kind: specKind})
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	nodeIDs := make([]string, 0, len(rowsData))
	for _, row := range rowsData {
		nodeIDs = append(nodeIDs, row.node.ID)
	}
	labelsByNode, err := loadNodeLabelsForIDs(ctx, s.db, nodeIDs)
	if err != nil {
		return nil, err
	}
	result := make([]domain.Node, 0, len(rowsData))
	for _, row := range rowsData {
		row.node.Labels = labelsByNode[row.node.ID]
		if row.kind.Valid {
			row.node.SpecKind = domain.NodeSpecKind(row.kind.String)
		}
		result = append(result, row.node)
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

func (s *ResourceRepository) ListGatewayViews(ctx context.Context) ([]GatewayView, error) {
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

func (s *ResourceRepository) ListAgentViews(ctx context.Context) ([]AgentView, error) {
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

func (s *ResourceRepository) listNodeSpecViews(ctx context.Context, kind domain.NodeSpecKind) ([]nodeSpecView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id,n.name,n.enabled,n.certificate_state,n.certificate_serial,n.revision,n.created_at,n.updated_at,ns.node_id,ns.kind,ns.revision,ns.updated_at FROM nodes n INNER JOIN node_specs ns ON ns.node_id=n.id WHERE ns.kind=? ORDER BY n.id`, string(kind))
	if err != nil {
		return nil, err
	}
	type viewRow struct {
		nodeID, storedKind, created, nodeUpdated, specUpdated string
		node                                                  domain.Node
		specRevision                                          int64
	}
	baseRows := make([]viewRow, 0)
	for rows.Next() {
		var node domain.Node
		var enabled int
		var created, nodeUpdated string
		var nodeID, storedKind, specUpdated string
		var revision int64
		if err := rows.Scan(&node.ID, &node.Name, &enabled, &node.CertificateState, &node.CertificateSerial, &node.Revision, &created, &nodeUpdated, &nodeID, &storedKind, &revision, &specUpdated); err != nil {
			return nil, err
		}
		node.Enabled = enabled != 0
		node.CreatedAt, err = parseStoredTime("node.created_at", created)
		if err != nil {
			return nil, err
		}
		node.UpdatedAt, err = parseStoredTime("node.updated_at", nodeUpdated)
		if err != nil {
			return nil, err
		}
		node.SpecKind = domain.NodeSpecKind(storedKind)
		baseRows = append(baseRows, viewRow{nodeID: nodeID, storedKind: storedKind, node: node, specRevision: revision, created: created, nodeUpdated: nodeUpdated, specUpdated: specUpdated})
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	nodes := make([]domain.Node, 0, len(baseRows))
	for _, base := range baseRows {
		nodes = append(nodes, base.node)
	}
	var specs map[string]domain.NodeSpec
	if kind == domain.NodeSpecGateway {
		specs, err = loadGatewaySpecsBatchNormalized(ctx, s.db, nodes)
	} else {
		specs, err = loadAgentSpecsBatchNormalized(ctx, s.db, nodes)
	}
	if err != nil {
		return nil, err
	}
	nodeIDs := make([]string, 0, len(baseRows))
	for _, base := range baseRows {
		nodeIDs = append(nodeIDs, base.node.ID)
	}
	labelsByNode, err := loadNodeLabelsForIDs(ctx, s.db, nodeIDs)
	if err != nil {
		return nil, err
	}
	result := make([]nodeSpecView, 0, len(baseRows))
	for _, base := range baseRows {
		base.node.Labels = labelsByNode[base.node.ID]
		spec, ok := specs[base.nodeID]
		if !ok {
			return nil, fmt.Errorf("stored %s spec %q is missing", kind, base.nodeID)
		}
		result = append(result, nodeSpecView{Node: base.node, Spec: spec})
	}
	return result, nil
}

func (s *ResourceRepository) UpdateNode(ctx context.Context, node domain.Node, options WriteOptions) error {
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
	now := time.Now().UTC()
	tx, err := s.beginWriteTx(ctx)
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
	effectiveCertificateState := defaultCertificateState(node.CertificateState)
	if effectiveCertificateState == domain.CertificateDecommissioned && node.Enabled {
		return &domain.ApplyError{Code: "node_decommissioned", Path: "node.enabled", Message: "a decommissioned node must remain disabled until replacement enrollment"}
	}
	if currentCertificateState == domain.CertificateDecommissioned && (node.Enabled || effectiveCertificateState != domain.CertificateDecommissioned) {
		return &domain.ApplyError{Code: "node_decommissioned", Path: "node", Message: "a decommissioned node can only be re-enabled by one-time replacement enrollment"}
	}
	affectedNodes, err := assignmentParticipantIDsTx(ctx, tx, node.ID)
	if err != nil {
		return err
	}
	affectedNodes = append(affectedNodes, node.ID)
	node.Revision = current + 1
	result, err := tx.ExecContext(ctx, `UPDATE nodes SET name=?,enabled=?,certificate_state=?,certificate_serial=?,revision=?,updated_at=? WHERE id=? AND revision=?`, node.Name, boolInt(node.Enabled), defaultCertificateState(node.CertificateState), node.CertificateSerial, node.Revision, now.Format(time.RFC3339Nano), node.ID, current)
	if err != nil {
		return err
	}
	if err := requireRevisionWrite(ctx, tx, result, "node", current, `SELECT revision FROM nodes WHERE id=?`, node.ID); err != nil {
		return err
	}
	if err := insertNodeLabelsTx(ctx, tx, node.ID, node.Labels); err != nil {
		return err
	}
	// A disabled or non-active identity must not retain an applied placement.
	// Quarantine the assignment rows in this same transaction as the identity
	// update, so a concurrent snapshot builder can never publish a new node
	// status while leaving the old public listeners authorized. The direct
	// control-stream action in the API closes online sessions immediately; this
	// durable state also covers offline nodes and reconnects.
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
	return s.commitAndNotifyResources(ctx, tx, affectedNodes...)
}

// DecommissionNode permanently removes a Node from scheduling and control
// plane admission while retaining its identity row as a tombstone. Keeping
// the row is important when Services or Assignments still reference it: the
// business resources remain intact and can be rebound after a replacement
// enrollment, while the old certificate can never authenticate again.
func (s *ResourceRepository) DecommissionNode(ctx context.Context, nodeID string, options WriteOptions) (domain.Node, error) {
	if _, err := s.GetNode(ctx, nodeID); err != nil {
		return domain.Node{}, err
	}
	request := struct {
		ID      string `json:"id"`
		IfMatch int64  `json:"if_match"`
		Action  string `json:"action"`
	}{ID: nodeID, IfMatch: options.IfMatch, Action: "decommission"}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return domain.Node{}, err
	}
	defer tx.Rollback()
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return domain.Node{}, err
	}
	if hit {
		if err := s.commitWriteTx(ctx, tx); err != nil {
			return domain.Node{}, err
		}
		return s.GetNode(ctx, nodeID)
	}
	var currentRevision int64
	var currentEnabled int
	var currentState string
	if err := tx.QueryRowContext(ctx, `SELECT revision,enabled,certificate_state FROM nodes WHERE id=?`, nodeID).Scan(&currentRevision, &currentEnabled, &currentState); err != nil {
		return domain.Node{}, err
	}
	if options.IfMatch <= 0 || options.IfMatch != currentRevision {
		return domain.Node{}, &RevisionConflictError{Resource: "node", Expected: options.IfMatch, Actual: currentRevision}
	}
	if currentState == domain.CertificateDecommissioned && currentEnabled == 0 {
		if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": nodeID, "revision": currentRevision, "state": domain.CertificateDecommissioned}); err != nil {
			return domain.Node{}, err
		}
		if err := s.commitWriteTx(ctx, tx); err != nil {
			return domain.Node{}, err
		}
		return s.GetNode(ctx, nodeID)
	}
	if currentRevision == math.MaxInt64 {
		return domain.Node{}, &domain.ApplyError{Code: "invalid_revision", Path: "node.revision", Message: "node revision is exhausted"}
	}
	affectedNodes, err := assignmentParticipantIDsTx(ctx, tx, nodeID)
	if err != nil {
		return domain.Node{}, err
	}
	affectedNodes = append(affectedNodes, nodeID)
	nextRevision := currentRevision + 1
	result, err := tx.ExecContext(ctx, `UPDATE nodes SET enabled=0,certificate_state=?,revision=?,updated_at=? WHERE id=? AND revision=?`, domain.CertificateDecommissioned, nextRevision, time.Now().UTC().Format(time.RFC3339Nano), nodeID, currentRevision)
	if err != nil {
		return domain.Node{}, err
	}
	if err := requireRevisionWrite(ctx, tx, result, "node", currentRevision, `SELECT revision FROM nodes WHERE id=?`, nodeID); err != nil {
		return domain.Node{}, err
	}
	if err := quarantineAssignmentsForNodeTx(ctx, tx, nodeID); err != nil {
		return domain.Node{}, err
	}
	if err := insertAudit(ctx, tx, options.Actor, "decommission", "node", nodeID, nextRevision, map[string]string{"state": domain.CertificateDecommissioned}); err != nil {
		return domain.Node{}, err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": nodeID, "revision": nextRevision, "state": domain.CertificateDecommissioned}); err != nil {
		return domain.Node{}, err
	}
	if err := s.commitAndNotifyResources(ctx, tx, affectedNodes...); err != nil {
		return domain.Node{}, err
	}
	return s.GetNode(ctx, nodeID)
}

// quarantineAssignmentsForNodeTx moves every placement that references an
// unavailable identity to the fail-closed degraded state. It intentionally
// preserves the assignment identity and shared generation: a later scheduler
// pass can replace the Gateway while both peers still have to acknowledge the
// new placement before it becomes applied again.
func quarantineAssignmentsForNodeTx(ctx context.Context, tx *sql.Tx, nodeID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,gateway_id,agent_id,revision,generation FROM assignments WHERE gateway_id=? OR agent_id=? ORDER BY id`, nodeID, nodeID)
	if err != nil {
		return err
	}
	type assignmentQuarantineRow struct {
		id                string
		gatewayID         string
		agentID           string
		revision          int64
		indexedGeneration int64
	}
	assignmentRows := make([]assignmentQuarantineRow, 0)
	for rows.Next() {
		var row assignmentQuarantineRow
		if err := rows.Scan(&row.id, &row.gatewayID, &row.agentID, &row.revision, &row.indexedGeneration); err != nil {
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
		revision, indexedGeneration := row.revision, row.indexedGeneration
		assignment, err := loadAssignmentNormalized(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("load assignment %q: %w", id, err)
		}
		if assignment.ID != id || assignment.GatewayID != gatewayID || assignment.AgentID != agentID || int64(assignment.Generation) != indexedGeneration {
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
		if revision == int64(^uint64(0)>>1) {
			return &domain.ApplyError{Code: "invalid_revision", Path: "assignment.revision", Message: "assignment revision is exhausted"}
		}
		assignment.State = domain.AssignmentDegraded
		assignment.Revision = revision + 1
		assignment.UpdatedAt = now
		result, err := updateAssignmentColumnsTx(ctx, tx, assignment, revision)
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

// PurgeNode permanently removes a decommissioned Node identity and its
// node-owned state. Decommissioning is deliberately a separate operation:
// it revokes admission and retains the tombstone so a replacement can reuse
// the identity. Purging is irreversible, so it is allowed only after that
// explicit lifecycle transition and only when no business resource still
// depends on the identity.
func (s *ResourceRepository) PurgeNode(ctx context.Context, id string, options WriteOptions) error {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := struct {
		ID      string `json:"id"`
		IfMatch int64  `json:"if_match"`
		Action  string `json:"action"`
	}{ID: id, IfMatch: options.IfMatch, Action: "purge"}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	var revision int64
	var enabled int
	var certificateState string
	if err := tx.QueryRowContext(ctx, `SELECT revision,enabled,certificate_state FROM nodes WHERE id=?`+s.selectForUpdateClause(), id).Scan(&revision, &enabled, &certificateState); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: "node", Expected: options.IfMatch, Actual: revision}
	}
	if enabled != 0 || certificateState != domain.CertificateDecommissioned {
		return &domain.ApplyError{Code: "resource_conflict", Path: "node", Message: "node must be decommissioned before permanent deletion"}
	}
	// Assignments and services remain explicit business dependencies: purging a
	// node must never silently remove a live placement or an Agent's service
	// definitions. Agent Gateway bindings are also counted because they can
	// exist before the scheduler has materialized an Assignment.
	var dependents int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM assignments WHERE gateway_id=? OR agent_id=?) +
		(SELECT COUNT(*) FROM services WHERE agent_id=?) +
		(SELECT COUNT(*) FROM agent_selector_labels WHERE key=? AND value=?)`, id, id, id, agentGatewayBindingStorageKey, id).Scan(&dependents); err != nil {
		return err
	}
	if dependents > 0 {
		return &domain.ApplyError{Code: "resource_conflict", Path: "node", Message: "node has dependent services, assignments or Agent bindings"}
	}
	// node_bootstraps intentionally has no foreign key: it can represent an
	// install intent before the Node identity exists. Clean up a stale intent
	// explicitly so a purged ID cannot retain an old one-time enrollment token.
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_bootstraps WHERE node_id=?`, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=? AND revision=?`, id, revision)
	if err != nil {
		return err
	}
	if err := requireRevisionWrite(ctx, tx, result, "node", revision, `SELECT revision FROM nodes WHERE id=?`, id); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "purge", "node", id, revision, map[string]string{"state": "deleted"}); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": revision}); err != nil {
		return err
	}
	if err := s.commitAndNotifyResources(ctx, tx, id); err != nil {
		return err
	}
	return nil
}

// DeleteNode is kept as an internal compatibility name for callers that used
// the old repository method. It now has the same safety gate as permanent
// deletion; ordinary node removal is handled by DecommissionNode.
func (s *ResourceRepository) DeleteNode(ctx context.Context, id string, options WriteOptions) error {
	return s.PurgeNode(ctx, id, options)
}
