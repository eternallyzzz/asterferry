package controller

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// AdvancedOperationsEnabled is a low-frequency control-plane setting. It is
// kept with the resource repository because updates are audited alongside
// other administrative state; runtime telemetry has its own repository.
func (s *ResourceRepository) AdvancedOperationsEnabled(ctx context.Context) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM runtime_settings WHERE key='advanced_operations_enabled'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	value = strings.TrimSpace(value)
	return strings.EqualFold(value, "true") || value == "1", nil
}

func (s *ResourceRepository) SetAdvancedOperationsEnabled(ctx context.Context, enabled bool, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	value := "false"
	if enabled {
		value = "true"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_settings(key,value,updated_at) VALUES('advanced_operations_enabled',?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, value, now); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "runtime_settings:update", "runtime_settings", "advanced_operations_enabled", 0, map[string]string{"enabled": value}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Settings changes affect the runtime control stream and are therefore
	// published on the shared runtime notification channel.
	s.ChangeBus().notifyRuntimeChanges("")
	return nil
}
