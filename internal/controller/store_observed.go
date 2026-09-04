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

func (s *Store) SaveObserved(ctx context.Context, record ObservedRecord) error {
	return s.saveObserved(ctx, record, false)
}

// SaveObservedHeartbeat accepts a lower applied generation because a
// heartbeat is a liveness report, not an authoritative monotonic write. A
// node can legitimately restart from an older last-known-good cache while it
// is unable to apply the current desired generation; tearing down the
// control stream in that state only hides the degraded condition.
func (s *Store) SaveObservedHeartbeat(ctx context.Context, record ObservedRecord) error {
	return s.saveObserved(ctx, record, true)
}

func (s *Store) saveObserved(ctx context.Context, record ObservedRecord, allowGenerationRollback bool) error {
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
		if record.Generation < current && !allowGenerationRollback {
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
