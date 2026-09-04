package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

func (s *Repository) SaveSnapshot(ctx context.Context, record SnapshotRecord) error {
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
	result, err := tx.ExecContext(ctx, `INSERT INTO desired_snapshots(node_id,generation,checksum,payload_json,created_at) VALUES(?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET generation=excluded.generation,checksum=excluded.checksum,payload_json=excluded.payload_json,created_at=excluded.created_at WHERE excluded.generation > desired_snapshots.generation`, record.NodeID, record.Generation, record.Checksum, record.Document, record.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("save snapshot rows affected: %w", err)
	}
	if affected == 0 {
		// Another Controller process may have won the conditional upsert after
		// this transaction's preflight SELECT. Re-read the committed row so a
		// true same-content retry remains idempotent while a stale/different
		// write is reported instead of being acknowledged as persisted.
		var persistedGeneration uint64
		var persistedChecksum string
		if err := tx.QueryRowContext(ctx, `SELECT generation,checksum FROM desired_snapshots WHERE node_id=?`, record.NodeID).Scan(&persistedGeneration, &persistedChecksum); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &RevisionConflictError{Resource: "desired_snapshot", Expected: uint64ToRevision(record.Generation), Actual: 0}
			}
			return err
		}
		if record.Generation == persistedGeneration && strings.EqualFold(record.Checksum, persistedChecksum) {
			return tx.Commit()
		}
		expected := persistedGeneration
		if expected < math.MaxInt64 {
			expected++
		}
		return &RevisionConflictError{Resource: "desired_snapshot", Expected: uint64ToRevision(expected), Actual: uint64ToRevision(record.Generation)}
	}
	if affected != 1 {
		return fmt.Errorf("save snapshot affected %d rows", affected)
	}
	return s.commitAndNotify(tx, record.NodeID)
}

func (s *Repository) LoadSnapshot(ctx context.Context, nodeID string) (SnapshotRecord, error) {
	var record SnapshotRecord
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT node_id,generation,checksum,payload_json,created_at FROM desired_snapshots WHERE node_id=?`, nodeID).Scan(&record.NodeID, &record.Generation, &record.Checksum, &record.Document, &created)
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

func (s *Repository) GetSnapshot(ctx context.Context, nodeID string) (domain.DesiredSnapshot, error) {
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
