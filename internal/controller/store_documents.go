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

func (s *Store) SaveSnapshot(ctx context.Context, record SnapshotRecord) error {
	if record.NodeID == "" || record.Generation == 0 || record.Checksum == "" || len(record.Document) == 0 {
		return errors.New("snapshot node, generation, checksum and document are required")
	}
	if len(record.Document) > maxSnapshotDocument {
		return errors.New("snapshot document is too large")
	}
	if record.Generation > math.MaxInt64 {
		return &domain.ApplyError{Code: "invalid_generation", Path: "generation", Message: "generation exceeds repository limit"}
	}
	var snapshot domain.DesiredSnapshot
	if err := json.Unmarshal(record.Document, &snapshot); err != nil {
		return fmt.Errorf("snapshot document is invalid: %w", err)
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
	if protected, err := json.Marshal(snapshot); err == nil {
		record.Document = protected
	} else {
		return fmt.Errorf("encode protected snapshot: %w", err)
	}
	if snapshot.NodeID != record.NodeID || snapshot.Generation != record.Generation || !strings.EqualFold(snapshot.Checksum, record.Checksum) {
		return errors.New("snapshot metadata does not match document")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	// The encrypted node cache and control envelope both treat the checksum as
	// an integrity boundary.  Persisting a document whose claimed checksum is
	// merely self-consistent with the row metadata would let a corrupted or
	// hand-edited snapshot become the Controller's authoritative last-known
	// state, so recompute it before accepting the write.
	computedChecksum, err := snapshot.ComputeChecksum()
	if err != nil {
		return fmt.Errorf("compute snapshot checksum: %w", err)
	}
	if !strings.EqualFold(computedChecksum, record.Checksum) {
		return &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "snapshot checksum does not match its content"}
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current uint64
	var currentChecksum string
	err = tx.QueryRowContext(ctx, `SELECT generation,checksum FROM desired_snapshots WHERE node_id=?`, record.NodeID).Scan(&current, &currentChecksum)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id=?`, record.NodeID).Scan(&exists); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if record.Generation < current {
			expected := current
			if current < math.MaxInt64 {
				expected++
			}
			return &RevisionConflictError{Resource: "desired_snapshot", Expected: uint64ToRevision(expected), Actual: uint64ToRevision(record.Generation)}
		}
		if record.Generation == current {
			if strings.EqualFold(record.Checksum, currentChecksum) {
				return tx.Commit()
			}
			expected := current
			if current < math.MaxInt64 {
				expected++
			}
			return &RevisionConflictError{Resource: "desired_snapshot", Expected: uint64ToRevision(expected), Actual: uint64ToRevision(record.Generation)}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO desired_snapshots(node_id,generation,checksum,document_json,created_at) VALUES(?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET generation=excluded.generation,checksum=excluded.checksum,document_json=excluded.document_json,created_at=excluded.created_at WHERE excluded.generation > desired_snapshots.generation`, record.NodeID, record.Generation, record.Checksum, record.Document, record.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return s.commitAndNotify(tx, record.NodeID)
}

func (s *Store) LoadSnapshot(ctx context.Context, nodeID string) (SnapshotRecord, error) {
	var record SnapshotRecord
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT node_id,generation,checksum,document_json,created_at FROM desired_snapshots WHERE node_id=?`, nodeID).Scan(&record.NodeID, &record.Generation, &record.Checksum, &record.Document, &created)
	if err != nil {
		return SnapshotRecord{}, err
	}
	record.CreatedAt, err = parseStoredTime("snapshot.created_at", created)
	if err != nil {
		return SnapshotRecord{}, err
	}
	if err := validateSnapshotRecord(record); err != nil {
		return SnapshotRecord{}, err
	}
	return record, nil
}

// validateSnapshotRecord treats the persisted document and its indexed
// metadata as one integrity boundary. Keeping this check on the low-level
// loader means scheduling and control-stream code cannot accidentally consume
// a hand-edited or partially restored row merely because its generation field
// looks plausible.
func validateSnapshotRecord(record SnapshotRecord) error {
	if record.NodeID == "" || record.Generation == 0 || record.Checksum == "" || len(record.Document) == 0 {
		return errors.New("stored snapshot metadata is incomplete")
	}
	var snapshot domain.DesiredSnapshot
	if err := json.Unmarshal(record.Document, &snapshot); err != nil {
		return fmt.Errorf("stored snapshot document is invalid: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if snapshot.NodeID != record.NodeID || snapshot.Generation != record.Generation || !strings.EqualFold(snapshot.Checksum, record.Checksum) {
		return &domain.ApplyError{Code: "snapshot_metadata_mismatch", Message: "stored snapshot metadata does not match its document"}
	}
	computed, err := snapshot.ComputeChecksum()
	if err != nil {
		return fmt.Errorf("compute stored snapshot checksum: %w", err)
	}
	if !strings.EqualFold(computed, record.Checksum) {
		return &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "stored snapshot checksum does not match its content"}
	}
	return nil
}

func (s *Store) GetSnapshot(ctx context.Context, nodeID string) (domain.DesiredSnapshot, error) {
	record, err := s.LoadSnapshot(ctx, nodeID)
	if err != nil {
		return domain.DesiredSnapshot{}, err
	}
	var snapshot domain.DesiredSnapshot
	if err := json.Unmarshal(record.Document, &snapshot); err != nil {
		return domain.DesiredSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return domain.DesiredSnapshot{}, err
	}
	if snapshot.NodeID != record.NodeID || snapshot.Generation != record.Generation || !strings.EqualFold(snapshot.Checksum, record.Checksum) {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "snapshot_metadata_mismatch", Message: "stored snapshot metadata does not match its document"}
	}
	computedChecksum, err := snapshot.ComputeChecksum()
	if err != nil {
		return domain.DesiredSnapshot{}, fmt.Errorf("compute snapshot checksum: %w", err)
	}
	if !strings.EqualFold(computedChecksum, record.Checksum) {
		return domain.DesiredSnapshot{}, &domain.ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "stored snapshot checksum does not match its content"}
	}
	return snapshot, nil
}

func (s *Store) SaveObserved(ctx context.Context, record ObservedRecord) error {
	if record.NodeID == "" || len(record.Document) == 0 {
		return errors.New("observed state node and document are required")
	}
	if len(record.Document) > 16<<20 {
		return errors.New("observed state document is too large")
	}
	if record.Generation > math.MaxInt64 {
		return &domain.ApplyError{Code: "invalid_generation", Path: "applied_generation", Message: "observed generation exceeds repository limit"}
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	if record.UpdatedAt.After(time.Now().UTC().Add(5 * time.Minute)) {
		return &domain.ApplyError{Code: "invalid_observed_state", Path: "updated_at", Message: "observed timestamp is too far in the future"}
	}
	var observed domain.ObservedState
	if err := json.Unmarshal(record.Document, &observed); err != nil {
		return fmt.Errorf("observed document is invalid: %w", err)
	}
	if err := observed.Validate(); err != nil {
		return err
	}
	if !observed.ObservedAt.IsZero() && observed.ObservedAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return &domain.ApplyError{Code: "invalid_observed_state", Path: "observed_at", Message: "observed timestamp is too far in the future"}
	}
	if observed.NodeID != record.NodeID || observed.AppliedGeneration != record.Generation {
		return errors.New("observed metadata does not match document")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id=?`, record.NodeID).Scan(&exists); err != nil {
		return err
	}
	var desiredGeneration uint64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM desired_snapshots WHERE node_id=?`, record.NodeID).Scan(&desiredGeneration); err == nil {
		if record.Generation > desiredGeneration {
			return &RevisionConflictError{Resource: "observed_state", Expected: uint64ToRevision(desiredGeneration), Actual: uint64ToRevision(record.Generation)}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	} else if record.Generation != 0 {
		return &RevisionConflictError{Resource: "observed_state", Expected: 0, Actual: uint64ToRevision(record.Generation)}
	}
	var current uint64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM observed_states WHERE node_id=?`, record.NodeID).Scan(&current); err == nil {
		if record.Generation < current {
			return &RevisionConflictError{Resource: "observed_state", Expected: uint64ToRevision(current), Actual: uint64ToRevision(record.Generation)}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO observed_states(node_id,generation,document_json,updated_at) VALUES(?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET generation=excluded.generation,document_json=excluded.document_json,updated_at=excluded.updated_at`, record.NodeID, record.Generation, record.Document, record.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return s.commitAndNotifyResourceOnly(tx, record.NodeID)
}

func (s *Store) LoadObserved(ctx context.Context, nodeID string) (ObservedRecord, error) {
	var record ObservedRecord
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT node_id,generation,document_json,updated_at FROM observed_states WHERE node_id=?`, nodeID).Scan(&record.NodeID, &record.Generation, &record.Document, &updated)
	if err != nil {
		return ObservedRecord{}, err
	}
	record.UpdatedAt, err = parseStoredTime("observed.updated_at", updated)
	if err != nil {
		return ObservedRecord{}, err
	}
	var observed domain.ObservedState
	if err := json.Unmarshal(record.Document, &observed); err != nil {
		return ObservedRecord{}, fmt.Errorf("stored observed document is invalid: %w", err)
	}
	if err := observed.Validate(); err != nil {
		return ObservedRecord{}, err
	}
	if !observed.ObservedAt.IsZero() && observed.ObservedAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return ObservedRecord{}, &domain.ApplyError{Code: "invalid_observed_state", Path: "observed_at", Message: "observed timestamp is too far in the future"}
	}
	if observed.NodeID != record.NodeID || observed.AppliedGeneration != record.Generation {
		return ObservedRecord{}, errors.New("stored observed metadata does not match its document")
	}
	return record, nil
}

func (s *Store) GetObserved(ctx context.Context, nodeID string) (domain.ObservedState, error) {
	record, err := s.LoadObserved(ctx, nodeID)
	if err != nil {
		return domain.ObservedState{}, err
	}
	var observed domain.ObservedState
	if err := json.Unmarshal(record.Document, &observed); err != nil {
		return domain.ObservedState{}, err
	}
	if err := observed.Validate(); err != nil {
		return domain.ObservedState{}, err
	}
	return observed, nil
}

func (s *Store) putDocument(ctx context.Context, table, nodeID string, value any, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	requestValue := value
	switch typed := value.(type) {
	case domain.GatewaySpec:
		typed.Revision = 0
		typed.Obfuscation = obfuscationRequestPolicy(typed.Obfuscation)
		requestValue = typed
	case domain.AgentSpec:
		typed.Revision = 0
		requestValue = typed
	}
	idempotentRequest := struct {
		Value   any   `json:"value"`
		IfMatch int64 `json:"if_match"`
	}{Value: requestValue, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, idempotentRequest)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	affectedNodes := []string{nodeID}
	if table == "gateway_specs" {
		participants, err := assignmentParticipantIDsTx(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		affectedNodes = append(affectedNodes, participants...)
	}
	if table == "gateway_specs" {
		gateway, ok := value.(domain.GatewaySpec)
		if !ok {
			return errors.New("gateway spec document has an invalid type")
		}
		if err := validateGatewaySpecTx(ctx, tx, gateway); err != nil {
			return err
		}
	} else if table == "agent_specs" {
		agent, ok := value.(domain.AgentSpec)
		if !ok {
			return errors.New("agent spec document has an invalid type")
		}
		if err := validateAgentSpecTx(ctx, tx, agent); err != nil {
			return err
		}
	}
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM `+table+` WHERE node_id=?`, nodeID).Scan(&revision)
	isInsert := errors.Is(err, sql.ErrNoRows)
	if isInsert {
		revision = 1
		if options.IfMatch > 0 {
			return &RevisionConflictError{Resource: table, Expected: options.IfMatch, Actual: 0}
		}
	} else if err == nil {
		if options.IfMatch <= 0 || options.IfMatch != revision {
			return &RevisionConflictError{Resource: table, Expected: options.IfMatch, Actual: revision}
		}
		revision++
	} else {
		return err
	}
	// Revisions are repository-owned metadata. Keep the canonical value in
	// the JSON document as well as in the indexed column.
	switch typed := value.(type) {
	case domain.GatewaySpec:
		typed.Revision = revision
		value = typed
	case domain.AgentSpec:
		typed.Revision = revision
		value = typed
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if isInsert {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+table+`(node_id,document_json,revision,updated_at) VALUES(?,?,?,?)`, nodeID, b, revision, now)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE `+table+` SET document_json=?,revision=?,updated_at=? WHERE node_id=? AND revision=?`, b, revision, now, nodeID, revision-1)
	}
	if err != nil {
		return err
	}
	// A Gateway endpoint is part of the assignment's dial target. Keep every
	// assignment on this Gateway aligned with the newly committed endpoint in
	// the same transaction, and advance its shared generation so the Agent
	// tears down the old QUIC session and reconnects to the new address.
	if table == "gateway_specs" {
		gateway, ok := value.(domain.GatewaySpec)
		if !ok {
			return errors.New("gateway spec document has an invalid type")
		}
		if err := updateAssignmentEndpointsTx(ctx, tx, gateway); err != nil {
			return err
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "upsert", strings.TrimSuffix(table, "_specs"), nodeID, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"node_id": nodeID, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

// updateAssignmentEndpointsTx keeps the derived assignment dial target
// consistent with a GatewaySpec edit. It intentionally runs after the spec
// row is written but before the transaction commits, so an endpoint change
// can never be observed without its corresponding assignment generation.
func updateAssignmentEndpointsTx(ctx context.Context, tx *sql.Tx, spec domain.GatewaySpec) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,agent_id,document_json,revision,generation FROM assignments WHERE gateway_id=? ORDER BY id`, spec.NodeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	endpointSet := make(map[string]struct{}, len(spec.PublicEndpoints))
	for _, endpoint := range spec.PublicEndpoints {
		endpointSet[endpoint] = struct{}{}
	}
	for rows.Next() {
		var assignment domain.Assignment
		var document []byte
		var revision int64
		var indexedGeneration uint64
		if err := rows.Scan(&assignment.ID, &assignment.AgentID, &document, &revision, &indexedGeneration); err != nil {
			return err
		}
		if err := json.Unmarshal(document, &assignment); err != nil {
			return fmt.Errorf("decode assignment %q: %w", assignment.ID, err)
		}
		if assignment.Generation != indexedGeneration {
			return fmt.Errorf("assignment %q generation index is inconsistent", assignment.ID)
		}
		endpoint := assignment.PublicEndpoint
		if _, exists := endpointSet[endpoint]; !exists {
			endpoint = spec.PublicEndpoints[0]
		}
		if endpoint == assignment.PublicEndpoint {
			continue
		}
		if assignment.Generation == math.MaxUint64 {
			return errors.New("assignment generation is exhausted")
		}
		assignment.PublicEndpoint = endpoint
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
		if _, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,generation=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Generation, assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano), assignment.ID, revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, assignment.ID); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, "system", "derived_endpoint", "assignment", assignment.ID, assignment.Revision, map[string]string{"gateway_id": spec.NodeID, "generation": fmt.Sprint(assignment.Generation)}); err != nil {
			return err
		}
	}
	return rows.Err()
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
	defer rows.Close()
	for rows.Next() {
		var assignmentID, gatewayID string
		if err := rows.Scan(&assignmentID, &gatewayID); err != nil {
			return err
		}
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
	return rows.Err()
}

func (s *Store) deleteDocument(ctx context.Context, table, nodeID string, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := struct {
		NodeID  string `json:"node_id"`
		IfMatch int64  `json:"if_match"`
	}{NodeID: nodeID, IfMatch: options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return nil
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM `+table+` WHERE node_id=?`, nodeID).Scan(&revision); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: table, Expected: options.IfMatch, Actual: revision}
	}
	if table == "gateway_specs" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignments WHERE gateway_id=?`, nodeID).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return &domain.ApplyError{Code: "resource_conflict", Path: "gateway_spec", Message: "gateway spec has active assignments"}
		}
	} else if table == "agent_specs" {
		var serviceCount, assignmentCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM services WHERE agent_id=?`, nodeID).Scan(&serviceCount); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignments WHERE agent_id=?`, nodeID).Scan(&assignmentCount); err != nil {
			return err
		}
		if serviceCount > 0 || assignmentCount > 0 {
			return &domain.ApplyError{Code: "resource_conflict", Path: "agent_spec", Message: "agent spec has services or assignments"}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE node_id=? AND revision=?`, nodeID, revision); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", strings.TrimSuffix(table, "_specs"), nodeID, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"node_id": nodeID, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, nodeID)
}

func (s *Store) putServiceDocument(ctx context.Context, service domain.Service, options WriteOptions) error {
	requestService := service
	requestService.Revision = 0
	requestService.UpdatedAt = time.Time{}
	idempotentRequest := struct {
		Service domain.Service `json:"service"`
		IfMatch int64          `json:"if_match"`
	}{Service: requestService, IfMatch: options.IfMatch}
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
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
	affectedNodes := []string{service.AgentID}
	participants, err := assignmentParticipantIDsForServiceTx(ctx, tx, service.ID)
	if err != nil {
		return err
	}
	affectedNodes = append(affectedNodes, participants...)
	var revision int64
	var previousDocument []byte
	var previous domain.Service
	hadPrevious := false
	err = tx.QueryRowContext(ctx, `SELECT revision,document_json FROM services WHERE id=?`, service.ID).Scan(&revision, &previousDocument)
	if errors.Is(err, sql.ErrNoRows) {
		revision = 1
		if options.IfMatch > 0 {
			return &RevisionConflictError{Resource: "service", Expected: options.IfMatch, Actual: 0}
		}
		service.Revision = revision
		service.UpdatedAt = nowTime
		b, marshalErr := json.Marshal(service)
		if marshalErr != nil {
			return marshalErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO services(id,agent_id,document_json,revision,updated_at) VALUES(?,?,?,?,?)`, service.ID, service.AgentID, b, revision, now)
	} else if err == nil {
		hadPrevious = true
		if options.IfMatch <= 0 || options.IfMatch != revision {
			return &RevisionConflictError{Resource: "service", Expected: options.IfMatch, Actual: revision}
		}
		if decodeErr := json.Unmarshal(previousDocument, &previous); decodeErr != nil {
			return fmt.Errorf("decode existing service: %w", decodeErr)
		}
		// An unassigned service may move between Agents. The old Agent's
		// node-scoped snapshot must be invalidated too, otherwise targeted
		// notifications leave the old node serving a stale service document.
		affectedNodes = append(affectedNodes, previous.AgentID)
		if previous.AgentID != service.AgentID || previous.Protocol != service.Protocol || previous.PublicBind != service.PublicBind || previous.PublicPort != service.PublicPort {
			assigned, assignmentErr := serviceHasAssignment(ctx, tx, service.ID)
			if assignmentErr != nil {
				return assignmentErr
			}
			if assigned {
				return &domain.ApplyError{Code: "resource_conflict", Path: "service", Message: "assigned service cannot change agent, protocol, bind, or port"}
			}
		}
		if !selectorsEqual(previous.GatewaySelector, service.GatewaySelector) {
			assignment, assigned, lookupErr := assignmentForService(ctx, tx, service.ID)
			if lookupErr != nil {
				return lookupErr
			}
			if assigned {
				var labelsJSON []byte
				if err := tx.QueryRowContext(ctx, `SELECT labels_json FROM nodes WHERE id=?`, assignment.GatewayID).Scan(&labelsJSON); err != nil {
					return err
				}
				var labels map[string]string
				if len(labelsJSON) > 0 {
					if err := json.Unmarshal(labelsJSON, &labels); err != nil {
						return err
					}
				}
				if !service.GatewaySelector.Matches(labels) {
					return &domain.ApplyError{Code: "selector_mismatch", Path: "service.gateway_selector", Message: "assigned service selector does not match its gateway"}
				}
			}
		}
		revision++
		service.Revision = revision
		service.UpdatedAt = nowTime
		b, marshalErr := json.Marshal(service)
		if marshalErr != nil {
			return marshalErr
		}
		_, err = tx.ExecContext(ctx, `UPDATE services SET agent_id=?,document_json=?,revision=?,updated_at=? WHERE id=? AND revision=?`, service.AgentID, b, revision, now, service.ID, revision-1)
	}
	if err != nil {
		return err
	}
	if hadPrevious && !sameServiceContent(previous, service) {
		// A service document is consumed by both ends of its assignment. Mark
		// the placement pending and advance the shared assignment generation in
		// the same transaction as the service write; otherwise one node could
		// start using a new local target while its peer still authorizes the old
		// target under an applied assignment.
		if err := bumpAssignmentsForServiceTx(ctx, tx, service.ID); err != nil {
			return err
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "upsert", "service", service.ID, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, idempotentRequest, map[string]any{"id": service.ID, "revision": revision}); err != nil {
		return err
	}
	return s.commitAndNotifyResources(tx, affectedNodes...)
}

func assignmentParticipantIDsTx(ctx context.Context, tx *sql.Tx, nodeID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT gateway_id,agent_id FROM assignments WHERE gateway_id=? OR agent_id=?`, nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var gatewayID, agentID string
		if err := rows.Scan(&gatewayID, &agentID); err != nil {
			return nil, err
		}
		result = append(result, gatewayID, agentID)
	}
	return result, rows.Err()
}

func assignmentParticipantIDsForServiceTx(ctx context.Context, tx *sql.Tx, serviceID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT assignments.gateway_id,assignments.agent_id FROM assignments JOIN assignment_services ON assignment_services.assignment_id=assignments.id WHERE assignment_services.service_id=?`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var gatewayID, agentID string
		if err := rows.Scan(&gatewayID, &agentID); err != nil {
			return nil, err
		}
		result = append(result, gatewayID, agentID)
	}
	return result, rows.Err()
}

// bumpAssignmentsForServiceTx invalidates the shared placement generation for
// every assignment that consumes a changed Service. The resource and
// assignment updates are deliberately part of one SQLite transaction so a
// snapshot builder can never observe the new target with an old applied
// assignment. Degraded/draining assignments remain fail-closed; a later
// scheduler pass can replace them with a fresh placement.
func bumpAssignmentsForServiceTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT assignments.id,assignments.document_json,assignments.revision,assignments.generation FROM assignments JOIN assignment_services ON assignment_services.assignment_id=assignments.id WHERE assignment_services.service_id=? ORDER BY assignments.id`, serviceID)
	if err != nil {
		return err
	}
	type assignmentRow struct {
		id         string
		document   []byte
		revision   int64
		generation uint64
	}
	assigned := make([]assignmentRow, 0)
	for rows.Next() {
		var row assignmentRow
		if err := rows.Scan(&row.id, &row.document, &row.revision, &row.generation); err != nil {
			_ = rows.Close()
			return err
		}
		assigned = append(assigned, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range assigned {
		id, document, revision, indexedGeneration := row.id, row.document, row.revision, row.generation
		var assignment domain.Assignment
		if err := json.Unmarshal(document, &assignment); err != nil {
			return fmt.Errorf("decode assignment %q: %w", id, err)
		}
		contains := false
		for _, candidate := range assignment.ServiceIDs {
			if candidate == serviceID {
				contains = true
				break
			}
		}
		if !contains {
			continue
		}
		if assignment.ID != id || assignment.Generation != indexedGeneration {
			return &domain.ApplyError{Code: "resource_metadata_mismatch", Path: "assignment", Message: "stored assignment metadata does not match its row"}
		}
		if assignment.Generation == math.MaxUint64 {
			return &domain.ApplyError{Code: "invalid_generation", Path: "assignment.generation", Message: "assignment generation is exhausted"}
		}
		if revision == math.MaxInt64 {
			return &domain.ApplyError{Code: "invalid_revision", Path: "assignment.revision", Message: "assignment revision is exhausted"}
		}
		if assignment.State == "" {
			assignment.State = domain.AssignmentPending
		}
		assignment.Generation++
		if assignment.State != domain.AssignmentDegraded && assignment.State != domain.AssignmentDraining {
			assignment.State = domain.AssignmentPending
		}
		assignment.Revision = revision + 1
		assignment.UpdatedAt = time.Now().UTC()
		updated, err := json.Marshal(assignment)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE assignments SET document_json=?,generation=?,revision=?,updated_at=? WHERE id=? AND revision=?`, updated, assignment.Generation, assignment.Revision, assignment.UpdatedAt.Format(time.RFC3339Nano), id, revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignment_acks WHERE assignment_id=?`, id); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, "system", "derived_service", "assignment", id, assignment.Revision, map[string]string{"service_id": serviceID, "generation": fmt.Sprint(assignment.Generation)}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func serviceHasAssignment(ctx context.Context, tx *sql.Tx, serviceID string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignment_services WHERE service_id=?`, serviceID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func assignmentForService(ctx context.Context, tx *sql.Tx, serviceID string) (domain.Assignment, bool, error) {
	var document []byte
	err := tx.QueryRowContext(ctx, `SELECT assignments.document_json FROM assignments JOIN assignment_services ON assignment_services.assignment_id=assignments.id WHERE assignment_services.service_id=?`, serviceID).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Assignment{}, false, nil
	}
	if err != nil {
		return domain.Assignment{}, false, err
	}
	var assignment domain.Assignment
	if err := json.Unmarshal(document, &assignment); err != nil {
		return domain.Assignment{}, false, err
	}
	return assignment, true, nil
}
