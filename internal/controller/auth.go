package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"asterferry/internal/domain"
)

const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	Enabled   bool      `json:"enabled"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type APIToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type UserUpdate struct {
	Username *string
	Password *string
	Role     *string
	Enabled  *bool
}

func ValidateRole(role string) error {
	if role != RoleViewer && role != RoleOperator && role != RoleAdmin {
		return fmt.Errorf("unknown role %q", role)
	}
	return nil
}

func (s *Store) CreateUser(ctx context.Context, username, password, role string) (User, error) {
	return s.CreateUserWithOptions(ctx, username, password, role, WriteOptions{Actor: "system"})
}

func (s *Store) CreateUserWithOptions(ctx context.Context, username, password, role string, options WriteOptions) (User, error) {
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
		Username       string `json:"username"`
		Role           string `json:"role"`
		PasswordDigest string `json:"password_digest"`
	}{Username: username, Role: role, PasswordDigest: passwordDigest(password)}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return User{}, err
	}
	if hit {
		return getUserTx(ctx, tx, "username", username)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,enabled,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, id, username, hash, role, 1, 1, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
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
	return User{ID: id, Username: username, Role: role, Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}, nil
}

func passwordDigest(password string) string {
	digest := sha256.Sum256([]byte(password))
	return hex.EncodeToString(digest[:])
}

func (s *Store) Authenticate(ctx context.Context, username, password string) (User, error) {
	var user User
	var hash, created, updated string
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT id,username,password_hash,role,enabled,revision,created_at,updated_at FROM users WHERE username=?`, strings.TrimSpace(username)).Scan(&user.ID, &user.Username, &hash, &user.Role, &enabled, &user.Revision, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) || !VerifyPassword(hash, password) {
		return User{}, errors.New("invalid credentials")
	}
	if err != nil {
		return User{}, err
	}
	if enabled == 0 {
		return User{}, errors.New("user is disabled")
	}
	user.Enabled = true
	user.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	user.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return user, nil
}

func (s *Store) UpdateUser(ctx context.Context, id string, update UserUpdate, options WriteOptions) (User, error) {
	if strings.TrimSpace(id) == "" {
		return User{}, sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var user User
	var hash, created, updated string
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT id,username,password_hash,role,enabled,revision,created_at,updated_at FROM users WHERE id=?`, id).Scan(&user.ID, &user.Username, &hash, &user.Role, &enabled, &user.Revision, &created, &updated); err != nil {
		return User{}, err
	}
	user.Enabled = enabled != 0
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
	if update.Password != nil {
		hash, err = HashPassword(*update.Password)
		if err != nil {
			return User{}, err
		}
	}
	passwordMarker := ""
	if update.Password != nil {
		passwordMarker = passwordDigest(*update.Password)
	}
	request := map[string]any{"id": id, "username": update.Username, "password_digest": passwordMarker, "role": update.Role, "enabled": update.Enabled, "if_match": options.IfMatch}
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
	newRevision := user.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE users SET username=?,password_hash=?,role=?,enabled=?,revision=?,updated_at=? WHERE id=? AND revision=?`, user.Username, hash, user.Role, boolInt(user.Enabled), newRevision, now.Format(time.RFC3339Nano), id, user.Revision); err != nil {
		return User{}, err
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
	user.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	user.Revision = newRevision
	user.UpdatedAt = now
	return user, nil
}

func (s *Store) DeleteUser(ctx context.Context, id string, options WriteOptions) error {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id=? AND revision=?`, id, revision); err != nil {
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

func (s *Store) CreateAPIToken(ctx context.Context, userID, name string, expiresAt *time.Time) (string, APIToken, error) {
	return s.CreateAPITokenWithOptions(ctx, userID, name, expiresAt, WriteOptions{Actor: "system"})
}

func (s *Store) CreateAPITokenWithOptions(ctx context.Context, userID, name string, expiresAt *time.Time, options WriteOptions) (string, APIToken, error) {
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
			value, parseErr := time.Parse(time.RFC3339Nano, expiry.String)
			if parseErr == nil {
				token.ExpiresAt = &value
			}
		}
		if revoked.Valid {
			value, parseErr := time.Parse(time.RFC3339Nano, revoked.String)
			if parseErr == nil {
				token.RevokedAt = &value
			}
		}
		token.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		return "", token, tx.Commit()
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

func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]APIToken, error) {
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
			if value, err := time.Parse(time.RFC3339Nano, expires.String); err == nil {
				token.ExpiresAt = &value
			}
		}
		if revoked.Valid {
			if value, err := time.Parse(time.RFC3339Nano, revoked.String); err == nil {
				token.RevokedAt = &value
			}
		}
		token.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		result = append(result, token)
	}
	return result, rows.Err()
}

func (s *Store) AuthenticateToken(ctx context.Context, token string) (User, error) {
	digest := HashToken(token)
	var user User
	var enabled int
	var revision int64
	var created, updated string
	var expires sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.role,u.enabled,u.revision,u.created_at,u.updated_at,t.expires_at FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE t.token_hash=? AND t.revoked_at IS NULL`, digest).Scan(&user.ID, &user.Username, &user.Role, &enabled, &revision, &created, &updated, &expires)
	if err != nil {
		return User{}, errors.New("invalid API token")
	}
	if enabled == 0 {
		return User{}, errors.New("user is disabled")
	}
	if expires.Valid {
		expiry, parseErr := time.Parse(time.RFC3339Nano, expires.String)
		if parseErr != nil || !time.Now().Before(expiry) {
			return User{}, errors.New("API token has expired")
		}
	}
	user.Enabled = true
	user.Revision = revision
	user.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	user.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return user, nil
}

func (s *Store) RevokeAPIToken(ctx context.Context, id string) error {
	return s.RevokeAPITokenWithOptions(ctx, id, WriteOptions{Actor: "system"})
}

func (s *Store) RevokeAPITokenForUser(ctx context.Context, userID, id string, options WriteOptions) error {
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
	if count, _ := result.RowsAffected(); count == 0 {
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

func (s *Store) RevokeAPITokenWithOptions(ctx context.Context, id string, options WriteOptions) error {
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
	count, _ := result.RowsAffected()
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

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Usernames are identifiers in the API but need not use the narrower
// resource-ID alphabet: operators commonly use an email address or a
// directory principal. Keep the value bounded and single-line while allowing
// those interoperable forms.
func validateUsername(username string) error {
	if username == "" || len(username) > 128 || strings.TrimSpace(username) == "" || strings.ContainsAny(username, "\x00\r\n") {
		return &domain.ApplyError{Code: "invalid_username", Path: "username", Message: "username must contain 1 to 128 single-line characters"}
	}
	return nil
}
