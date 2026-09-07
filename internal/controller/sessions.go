package controller

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type webSession struct {
	UserID    string
	CSRFHash  string
	ExpiresAt time.Time
}

func (s *ResourceRepository) CreateWebSession(ctx context.Context, userID, sessionID, csrf string, expiresAt time.Time) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(csrf) == "" || expiresAt.IsZero() {
		return errors.New("web session fields are required")
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO web_sessions(session_hash,user_id,csrf_hash,expires_at,created_at) VALUES(?,?,?,?,?)`, HashToken(sessionID), userID, HashToken(csrf), expiresAt.UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return s.commitWriteTx(ctx, tx)
}

func (s *ResourceRepository) GetWebSession(ctx context.Context, sessionID string) (webSession, error) {
	if strings.TrimSpace(sessionID) == "" {
		return webSession{}, sql.ErrNoRows
	}
	var value webSession
	var expires string
	var revoked sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT user_id,csrf_hash,expires_at,revoked_at FROM web_sessions WHERE session_hash=?`, HashToken(sessionID)).Scan(&value.UserID, &value.CSRFHash, &expires, &revoked); err != nil {
		return webSession{}, err
	}
	if revoked.Valid && strings.TrimSpace(revoked.String) != "" {
		return webSession{}, sql.ErrNoRows
	}
	parsed, err := parseStoredTime("web_session.expires_at", expires)
	if err != nil {
		return webSession{}, err
	}
	value.ExpiresAt = parsed
	if !time.Now().UTC().Before(parsed) {
		return webSession{}, sql.ErrNoRows
	}
	return value, nil
}

func (s *ResourceRepository) RevokeWebSession(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE web_sessions SET revoked_at=? WHERE session_hash=? AND revoked_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), HashToken(sessionID))
	if err != nil {
		return err
	}
	return s.commitWriteTx(ctx, tx)
}
