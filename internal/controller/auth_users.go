package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Repository) CreateUser(ctx context.Context, username, password, role string) (User, error) {
	return s.CreateUserWithOptions(ctx, username, password, role, WriteOptions{Actor: "system"})
}

func (s *Repository) CreateUserWithOptions(ctx context.Context, username, password, role string, options WriteOptions) (User, error) {
	username = strings.TrimSpace(username)
	if err := validateUsername(username); err != nil {
		return User{}, err
	}
	if err := ValidateRole(role); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	id, err := randomID()
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	request := struct {
		Username        string `json:"username"`
		Role            string `json:"role"`
		PasswordChanged bool   `json:"password_changed"`
	}{Username: username, Role: role, PasswordChanged: true}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return User{}, err
	}
	if hit {
		return getUserTx(ctx, tx, "username", username)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,password_changed_at,role,enabled,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, username, hash, now.Format(time.RFC3339Nano), role, 1, 1, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	if err := insertAudit(ctx, tx, options.Actor, "create", "user", id, 1, map[string]string{"username": username, "role": role}); err != nil {
		return User{}, err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": 1}); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return User{ID: id, Username: username, Role: role, Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now}, nil
}

// dummyPasswordHash is a fixed, valid Argon2id value used for failed logins
// where the username does not exist. Running the same verifier in that case
// prevents the database lookup result from becoming a username timing oracle.
const dummyPasswordHash = "$argon2id$v=19$m=65536,t=3,p=2$YXN0ZXJmZXJyeS1kdW1teSE$PcdkQIKcsmT6A6y+xYGrZHtf7nOnuwy4AX8onzMTER0"

func (s *Repository) Authenticate(ctx context.Context, username, password string) (User, error) {
	var user User
	var hash, created, updated, passwordChanged string
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT id,username,password_hash,password_changed_at,role,enabled,revision,created_at,updated_at FROM users WHERE username=?`, strings.TrimSpace(username)).Scan(&user.ID, &user.Username, &hash, &passwordChanged, &user.Role, &enabled, &user.Revision, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		_ = VerifyPassword(dummyPasswordHash, password)
		return User{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, storageFailure("authenticate user", err)
	}
	if !VerifyPassword(hash, password) {
		return User{}, ErrInvalidCredentials
	}
	if enabled == 0 {
		return User{}, ErrUserDisabled
	}
	user.Enabled = true
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

func (s *Repository) UpdateUser(ctx context.Context, id string, update UserUpdate, options WriteOptions) (User, error) {
	if strings.TrimSpace(id) == "" {
		return User{}, sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var user User
	var hash, created, updated, passwordChanged string
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT id,username,password_hash,password_changed_at,role,enabled,revision,created_at,updated_at FROM users WHERE id=?`, id).Scan(&user.ID, &user.Username, &hash, &passwordChanged, &user.Role, &enabled, &user.Revision, &created, &updated); err != nil {
		return User{}, err
	}
	user.Enabled = enabled != 0
	user.CreatedAt, err = parseStoredTime("user.created_at", created)
	if err != nil {
		return User{}, err
	}
	user.UpdatedAt, err = parseStoredTime("user.updated_at", updated)
	if err != nil {
		return User{}, err
	}
	user.PasswordChangedAt, err = parseStoredTime("user.password_changed_at", passwordChanged)
	if err != nil {
		return User{}, err
	}
	if update.Username != nil {
		name := strings.TrimSpace(*update.Username)
		if err := validateUsername(name); err != nil {
			return User{}, err
		}
		user.Username = name
	}
	if update.Role != nil {
		if err := ValidateRole(*update.Role); err != nil {
			return User{}, err
		}
		if user.Role == RoleAdmin && *update.Role != RoleAdmin && user.Enabled {
			var admins int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role=? AND enabled=1`, RoleAdmin).Scan(&admins); err != nil {
				return User{}, err
			}
			if admins <= 1 {
				return User{}, errors.New("cannot demote the last enabled admin")
			}
		}
		user.Role = *update.Role
	}
	if update.Enabled != nil {
		if !*update.Enabled && user.Role == RoleAdmin {
			var admins int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role=? AND enabled=1`, RoleAdmin).Scan(&admins); err != nil {
				return User{}, err
			}
			if admins <= 1 {
				return User{}, errors.New("cannot disable the last enabled admin")
			}
		}
		user.Enabled = *update.Enabled
	}
	request := map[string]any{"id": id, "username": update.Username, "password_changed": update.Password != nil, "role": update.Role, "enabled": update.Enabled, "if_match": options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return User{}, err
	}
	if hit {
		return getUserTx(ctx, tx, "id", id)
	}
	if options.IfMatch <= 0 || options.IfMatch != user.Revision {
		return User{}, &RevisionConflictError{Resource: "user", Expected: options.IfMatch, Actual: user.Revision}
	}
	now := time.Now().UTC()
	passwordChangedAt := now
	if update.Password != nil && !passwordChangedAt.After(user.PasswordChangedAt) {
		// PasswordChangedAt is the session revocation marker. Keep it strictly
		// monotonic even on platforms whose wall clock has coarse resolution.
		passwordChangedAt = user.PasswordChangedAt.Add(time.Nanosecond)
	}
	if update.Password != nil {
		hash, err = HashPassword(*update.Password)
		if err != nil {
			return User{}, err
		}
		user.PasswordChangedAt = passwordChangedAt
	}
	newRevision := user.Revision + 1
	result, err := tx.ExecContext(ctx, `UPDATE users SET username=?,password_hash=?,password_changed_at=?,role=?,enabled=?,revision=?,updated_at=? WHERE id=? AND revision=?`, user.Username, hash, user.PasswordChangedAt.Format(time.RFC3339Nano), user.Role, boolInt(user.Enabled), newRevision, now.Format(time.RFC3339Nano), id, user.Revision)
	if err != nil {
		return User{}, err
	}
	if err := requireRevisionWrite(ctx, tx, result, "user", user.Revision, `SELECT revision FROM users WHERE id=?`, id); err != nil {
		return User{}, err
	}
	if update.Password != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now.Format(time.RFC3339Nano), id); err != nil {
			return User{}, err
		}
	}
	if err := insertAudit(ctx, tx, options.Actor, "update", "user", id, newRevision, map[string]string{"username": user.Username, "role": user.Role}); err != nil {
		return User{}, err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": newRevision}); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	user.Revision = newRevision
	user.UpdatedAt = now
	return user, nil
}

func (s *Repository) DeleteUser(ctx context.Context, id string, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := map[string]any{"id": id, "if_match": options.IfMatch}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return tx.Commit()
	}
	var role string
	var enabled int
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT role,enabled,revision FROM users WHERE id=?`, id).Scan(&role, &enabled, &revision); err != nil {
		return err
	}
	if options.IfMatch <= 0 || options.IfMatch != revision {
		return &RevisionConflictError{Resource: "user", Expected: options.IfMatch, Actual: revision}
	}
	if role == RoleAdmin && enabled != 0 {
		var admins int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role=? AND enabled=1`, RoleAdmin).Scan(&admins); err != nil {
			return err
		}
		if admins <= 1 {
			return errors.New("cannot delete the last enabled admin")
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id=? AND revision=?`, id, revision)
	if err != nil {
		return err
	}
	if err := requireRevisionWrite(ctx, tx, result, "user", revision, `SELECT revision FROM users WHERE id=?`, id); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", "user", id, revision, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"id": id, "revision": revision}); err != nil {
		return err
	}
	return tx.Commit()
}
