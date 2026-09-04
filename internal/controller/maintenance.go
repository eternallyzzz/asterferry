package controller

import (
	"context"
	"time"
)

// PruneHistory removes records whose retention window has elapsed. It is a
// single transaction so a failed cleanup cannot leave a half-applied policy.
// A non-positive duration disables cleanup for that table.
func (s *ResourceRepository) PruneHistory(ctx context.Context, now time.Time, idempotencyTTL, auditTTL time.Duration) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.beginWriteTx(ctx)
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
	// Pending installation intents are credentials as well as UI state. Once
	// their enrollment window has elapsed they can no longer be useful and
	// must not accumulate indefinitely in the Controller database.
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_bootstraps WHERE expires_at <= ?`, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM web_sessions WHERE expires_at <= ? OR revoked_at IS NOT NULL`, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if err := s.commitWriteTx(ctx, tx); err != nil {
		return err
	}
	return nil
}
