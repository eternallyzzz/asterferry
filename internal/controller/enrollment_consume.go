package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) RevokeEnrollmentToken(ctx context.Context, id string) error {
	return s.RevokeEnrollmentTokenWithOptions(ctx, id, WriteOptions{Actor: "system"})
}

func (s *Store) RevokeEnrollmentTokenWithOptions(ctx context.Context, id string, options WriteOptions) error {
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
	result, err := tx.ExecContext(ctx, `UPDATE enrollment_tokens SET used_at=? WHERE id=? AND used_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	count, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return fmt.Errorf("revoke enrollment token: rows affected: %w", affectedErr)
	}
	if count == 0 {
		return errors.New("enrollment token not found or already revoked")
	}
	if err := insertAudit(ctx, tx, options.Actor, "revoke", "enrollment_token", id, 1, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]string{"id": id}); err != nil {
		return err
	}
	return tx.Commit()
}

// consumeEnrollmentToken is a small package-level token-consumption helper;
// the production enrollment path consumes the token in the issuance
// transaction. Tests exercise it directly.
//
//lint:ignore U1000 package tests exercise token consumption directly.
func (s *Store) consumeEnrollmentToken(ctx context.Context, plain string) error {
	if strings.TrimSpace(plain) == "" {
		return errors.New("enrollment token is required")
	}
	digest := HashToken(plain)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storageFailure("begin enrollment token transaction", err)
	}
	defer tx.Rollback()
	if err := consumeEnrollmentTokenTx(ctx, tx, digest); err != nil {
		if isCredentialError(err) {
			return err
		}
		return storageFailure("consume enrollment token", err)
	}
	return storageFailure("commit enrollment token", tx.Commit())
}

func consumeEnrollmentTokenTx(ctx context.Context, tx *sql.Tx, digest string) error {
	var id, expires string
	var used sqlNullString
	if err := tx.QueryRowContext(ctx, `SELECT id,expires_at,used_at FROM enrollment_tokens WHERE token_hash=?`, digest).Scan(&id, &expires, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidEnrollmentToken
		}
		return fmt.Errorf("load enrollment token: %w", err)
	}
	if used.Valid {
		return ErrEnrollmentTokenUsed
	}
	expiry, err := parseStoredTime("enrollment_token.expires_at", expires)
	if err != nil {
		return err
	}
	if !time.Now().Before(expiry) {
		return ErrEnrollmentTokenExpired
	}
	result, err := tx.ExecContext(ctx, `UPDATE enrollment_tokens SET used_at=? WHERE id=? AND used_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	count, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return fmt.Errorf("consume enrollment token: rows affected: %w", affectedErr)
	}
	if count != 1 {
		return ErrEnrollmentTokenUsed
	}
	return nil
}

func (s *Store) validateEnrollmentToken(ctx context.Context, plain string) error {
	if strings.TrimSpace(plain) == "" {
		return ErrInvalidEnrollmentToken
	}
	var expires string
	var used sqlNullString
	err := s.db.QueryRowContext(ctx, `SELECT expires_at,used_at FROM enrollment_tokens WHERE token_hash=?`, HashToken(plain)).Scan(&expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidEnrollmentToken
	}
	if err != nil {
		return storageFailure("validate enrollment token", err)
	}
	if used.Valid {
		return ErrEnrollmentTokenUsed
	}
	expiresAt, err := parseStoredTime("enrollment_token.expires_at", expires)
	if err != nil {
		return err
	}
	if !time.Now().UTC().Before(expiresAt) {
		return ErrEnrollmentTokenExpired
	}
	return nil
}

// sqlNullString is kept local to avoid leaking database/sql implementation
// details into the public enrollment model.
type sqlNullString struct {
	String string
	Valid  bool
}

func (v *sqlNullString) Scan(src any) error {
	switch value := src.(type) {
	case nil:
		v.String, v.Valid = "", false
	case string:
		v.String, v.Valid = value, true
	case []byte:
		v.String, v.Valid = string(value), true
	default:
		return fmt.Errorf("unsupported nullable value %T", src)
	}
	return nil
}
