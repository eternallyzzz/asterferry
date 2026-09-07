package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"asterferry/internal/domain"
)

func (s *ResourceRepository) SaveObserved(ctx context.Context, record ObservedRecord) error {
	return s.saveObserved(ctx, record, false)
}

// SaveObservedHeartbeat accepts a lower applied generation because a
// heartbeat is a liveness report, not an authoritative monotonic write.
func (s *ResourceRepository) SaveObservedHeartbeat(ctx context.Context, record ObservedRecord) error {
	return s.saveObserved(ctx, record, true)
}

func (s *ResourceRepository) saveObserved(ctx context.Context, record ObservedRecord, allowGenerationRollback bool) error {
	if record.NodeID == "" || len(record.Document) == 0 {
		return errors.New("observed state node and document are required")
	}
	if len(record.Document) > 16<<20 {
		return errors.New("observed state document is too large")
	}
	var observed domain.ObservedState
	if err := json.Unmarshal(record.Document, &observed); err != nil {
		return fmt.Errorf("observed document is invalid: %w", err)
	}
	if err := observed.Validate(); err != nil {
		return err
	}
	if observed.NodeID != record.NodeID || observed.AppliedGeneration != record.Generation {
		return errors.New("observed metadata does not match document")
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
	if observed.ObservedAt.IsZero() {
		observed.ObservedAt = record.UpdatedAt
	}
	if observed.ObservedAt.After(time.Now().UTC().Add(5 * time.Minute)) {
		return &domain.ApplyError{Code: "invalid_observed_state", Path: "observed_at", Message: "observed timestamp is too far in the future"}
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id=?`, record.NodeID).Scan(&exists); err != nil {
		return err
	}
	var desiredGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM desired_snapshots WHERE node_id=?`, record.NodeID).Scan(&desiredGeneration); err == nil {
		desired, conversionErr := storedUint64(desiredGeneration, "desired snapshot generation")
		if conversionErr != nil {
			return conversionErr
		}
		if record.Generation > desired {
			return &RevisionConflictError{Resource: "observed_state", Expected: uint64ToRevision(desired), Actual: uint64ToRevision(record.Generation)}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	} else if record.Generation != 0 {
		return &RevisionConflictError{Resource: "observed_state", Expected: 0, Actual: uint64ToRevision(record.Generation)}
	}
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM observed_states WHERE node_id=?`, record.NodeID).Scan(&current); err == nil {
		currentGeneration, conversionErr := storedUint64(current, "observed generation")
		if conversionErr != nil {
			return conversionErr
		}
		if record.Generation < currentGeneration && !allowGenerationRollback {
			return &RevisionConflictError{Resource: "observed_state", Expected: uint64ToRevision(currentGeneration), Actual: uint64ToRevision(record.Generation)}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := writeObservedStateTx(ctx, tx, observed, record.UpdatedAt); err != nil {
		return err
	}
	return s.commitAndNotifyResourceOnly(ctx, tx, record.NodeID)
}

func (s *ResourceRepository) LoadObserved(ctx context.Context, nodeID string) (ObservedRecord, error) {
	observed, updated, err := loadObservedStateNormalized(ctx, s.db, nodeID)
	if err != nil {
		return ObservedRecord{}, err
	}
	document, err := json.Marshal(observed)
	if err != nil {
		return ObservedRecord{}, err
	}
	updatedAt, err := parseStoredTime("observed.updated_at", updated)
	if err != nil {
		return ObservedRecord{}, err
	}
	return ObservedRecord{NodeID: observed.NodeID, Generation: observed.AppliedGeneration, Document: document, UpdatedAt: updatedAt}, nil
}

func (s *ResourceRepository) GetObserved(ctx context.Context, nodeID string) (domain.ObservedState, error) {
	observed, _, err := loadObservedStateNormalized(ctx, s.db, nodeID)
	return observed, err
}
