package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *ResourceRepository) CreateAPIToken(ctx context.Context, userID, name string, expiresAt *time.Time) (string, APIToken, error) {
	return s.CreateAPITokenWithOptions(ctx, userID, name, expiresAt, WriteOptions{Actor: "system"})
}

func (s *ResourceRepository) CreateAPITokenWithOptions(ctx context.Context, userID, name string, expiresAt *time.Time, options WriteOptions) (string, APIToken, error) {
	userID = strings.TrimSpace(userID)
	name = strings.TrimSpace(name)
	if userID == "" || name == "" {
		return "", APIToken{}, errors.New("token user and name are required")
	}
	if len(name) > 128 || strings.ContainsAny(name, "\x00\r\n") {
		return "", APIToken{}, errors.New("token name is invalid")
	}
	var enabled int
	if err := s.db.QueryRowContext(ctx, `SELECT enabled FROM users WHERE id=?`, userID).Scan(&enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", APIToken{}, sql.ErrNoRows
		}
		return "", APIToken{}, err
	}
	if enabled == 0 {
		return "", APIToken{}, errors.New("user is disabled")
	}
	if expiresAt != nil {
		expires := expiresAt.UTC()
		if !expires.After(time.Now().UTC()) {
			return "", APIToken{}, errors.New("token expiration must be in the future")
		}
		expiresAt = &expires
	}
	request := map[string]any{"user_id": userID, "name": name}
	if expiresAt != nil {
		request["expires_at"] = expiresAt.Format(time.RFC3339Nano)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", APIToken{}, err
	}
	defer tx.Rollback()
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return "", APIToken{}, err
	}
	if hit {
		var response []byte
		if err := tx.QueryRowContext(ctx, `SELECT response_json FROM idempotency_keys WHERE key=?`, strings.TrimSpace(options.IdempotencyKey)).Scan(&response); err != nil {
			return "", APIToken{}, err
		}
		var metadata struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(response, &metadata); err != nil || metadata.ID == "" {
			return "", APIToken{}, errors.New("idempotency response is invalid")
		}
		var token APIToken
		var expiry, revoked sql.NullString
		var created string
		if err := tx.QueryRowContext(ctx, `SELECT id,user_id,name,expires_at,revoked_at,created_at FROM api_tokens WHERE id=?`, metadata.ID).Scan(&token.ID, &token.UserID, &token.Name, &expiry, &revoked, &created); err != nil {
			return "", APIToken{}, err
		}
		if expiry.Valid {
			value, parseErr := parseStoredTime("api_token.expires_at", expiry.String)
			if parseErr != nil {
				return "", APIToken{}, parseErr
			}
			token.ExpiresAt = &value
		}
		if revoked.Valid {
			value, parseErr := parseStoredTime("api_token.revoked_at", revoked.String)
			if parseErr != nil {
				return "", APIToken{}, parseErr
			}
			token.RevokedAt = &value
		}
		var parseErr error
		token.CreatedAt, parseErr = parseStoredTime("api_token.created_at", created)
		if parseErr != nil {
			return "", APIToken{}, parseErr
		}
		if err := tx.Commit(); err != nil {
			return "", APIToken{}, err
		}
		return "", token, ErrSecretAlreadyCreated
	}
	plain, digest, err := NewAPIToken()
	if err != nil {
		return "", APIToken{}, err
	}
	id, err := randomID()
	if err != nil {
		return "", APIToken{}, err
	}
	now := time.Now().UTC()
	var expiry any
	if expiresAt != nil {
		expiry = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO api_tokens(id,user_id,token_hash,name,expires_at,created_at) VALUES(?,?,?,?,?,?)`, id, userID, digest, name, expiry, now.Format(time.RFC3339Nano))
	if err != nil {
		return "", APIToken{}, err
	}
	if err := insertAudit(ctx, tx, options.Actor, "create", "api_token", id, 1, map[string]string{"user_id": userID, "name": name}); err != nil {
		return "", APIToken{}, err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]string{"id": id}); err != nil {
		return "", APIToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", APIToken{}, err
	}
	return plain, APIToken{ID: id, UserID: userID, Name: name, ExpiresAt: expiresAt, CreatedAt: now}, nil
}

func (s *ResourceRepository) ListAPITokens(ctx context.Context, userID string) ([]APIToken, error) {
	query := `SELECT id,user_id,name,expires_at,revoked_at,created_at FROM api_tokens`
	args := []any{}
	if strings.TrimSpace(userID) != "" {
		query += ` WHERE user_id=?`
		args = append(args, userID)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []APIToken{}
	for rows.Next() {
		var token APIToken
		var expires, revoked sql.NullString
		var created string
		if err := rows.Scan(&token.ID, &token.UserID, &token.Name, &expires, &revoked, &created); err != nil {
			return nil, err
		}
		if expires.Valid {
			value, parseErr := parseStoredTime("api_token.expires_at", expires.String)
			if parseErr != nil {
				return nil, parseErr
			}
			token.ExpiresAt = &value
		}
		if revoked.Valid {
			value, parseErr := parseStoredTime("api_token.revoked_at", revoked.String)
			if parseErr != nil {
				return nil, parseErr
			}
			token.RevokedAt = &value
		}
		var parseErr error
		token.CreatedAt, parseErr = parseStoredTime("api_token.created_at", created)
		if parseErr != nil {
			return nil, parseErr
		}
		result = append(result, token)
	}
	return result, rows.Err()
}

func (s *ResourceRepository) AuthenticateToken(ctx context.Context, token string) (User, error) {
	digest := HashToken(token)
	var user User
	var enabled int
	var revision int64
	var created, updated string
	var expires sql.NullString
	var passwordChanged string
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.role,u.enabled,u.revision,u.created_at,u.updated_at,u.password_changed_at,t.expires_at FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE t.token_hash=? AND t.revoked_at IS NULL`, digest).Scan(&user.ID, &user.Username, &user.Role, &enabled, &revision, &created, &updated, &passwordChanged, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrInvalidAPIToken
	}
	if err != nil {
		return User{}, storageFailure("authenticate API token", err)
	}
	if enabled == 0 {
		return User{}, ErrUserDisabled
	}
	if expires.Valid {
		expiry, parseErr := parseStoredTime("api_token.expires_at", expires.String)
		if parseErr != nil {
			return User{}, parseErr
		}
		if !time.Now().Before(expiry) {
			return User{}, ErrAPITokenExpired
		}
	}
	user.Enabled = true
	user.Revision = revision
	var parseErr error
	user.CreatedAt, parseErr = parseStoredTime("user.created_at", created)
	if parseErr != nil {
		return User{}, parseErr
	}
	user.UpdatedAt, parseErr = parseStoredTime("user.updated_at", updated)
	if parseErr != nil {
		return User{}, parseErr
	}
	user.PasswordChangedAt, parseErr = parseStoredTime("user.password_changed_at", passwordChanged)
	if parseErr != nil {
		return User{}, parseErr
	}
	return user, nil
}

func (s *ResourceRepository) RevokeAPIToken(ctx context.Context, id string) error {
	return s.RevokeAPITokenWithOptions(ctx, id, WriteOptions{Actor: "system"})
}

func (s *ResourceRepository) RevokeAPITokenForUser(ctx context.Context, userID, id string, options WriteOptions) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(id) == "" {
		return sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := map[string]string{"user_id": strings.TrimSpace(userID), "id": strings.TrimSpace(id)}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), id, userID)
	if err != nil {
		return err
	}
	count, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return fmt.Errorf("revoke API token: rows affected: %w", affectedErr)
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	if err := insertAudit(ctx, tx, options.Actor, "revoke", "api_token", id, 1, map[string]string{"user_id": userID}); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]string{"id": id}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *ResourceRepository) RevokeAPITokenWithOptions(ctx context.Context, id string, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := map[string]string{"id": strings.TrimSpace(id)}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return tx.Commit()
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now, id)
	if err != nil {
		return err
	}
	count, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return fmt.Errorf("revoke API token: rows affected: %w", affectedErr)
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	if err := insertAudit(ctx, tx, options.Actor, "revoke", "api_token", id, 1, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]string{"id": id}); err != nil {
		return err
	}
	return tx.Commit()
}
