package controller

import (
	"context"
	"time"
)

// PruneHistory removes records whose retention window has elapsed. It is a
// single transaction so a failed cleanup cannot leave a half-applied policy.
// A non-positive duration disables cleanup for that table.
func (s *Store) PruneHistory(ctx context.Context, now time.Time, idempotencyTTL, auditTTL time.Duration) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if idempotencyTTL > 0 {
		cutoff := now.Add(-idempotencyTTL).Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < ?`, cutoff); err != nil {
			return err
		}
	}
	if auditTTL > 0 {
		cutoff := now.Add(-auditTTL).Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `DELETE FROM audit_events WHERE created_at < ?`, cutoff); err != nil {
			return err
		}
	}
	return tx.Commit()
}
