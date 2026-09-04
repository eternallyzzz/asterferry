package controller

import (
	"asterferry/internal/domain"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *ResourceRepository) CreateEnrollmentToken(ctx context.Context, ttl time.Duration) (string, EnrollmentToken, error) {
	return s.CreateEnrollmentTokenWithOptions(ctx, ttl, WriteOptions{Actor: "system"})
}

func (s *ResourceRepository) CreateEnrollmentTokenWithOptions(ctx context.Context, ttl time.Duration, options WriteOptions) (string, EnrollmentToken, error) {
	return s.createEnrollmentTokenWithOptions(ctx, "", ttl, options)
}

// CreateNodeEnrollmentToken creates a short-lived enrollment credential that
// is cryptographically bound to one pre-created node. The binding is encoded
// in the one-time plaintext token and is covered by the stored token hash, so
// this feature does not require a schema change to the generic enrollment
// token table.
func (s *ResourceRepository) CreateNodeEnrollmentToken(ctx context.Context, nodeID string, ttl time.Duration) (string, EnrollmentToken, error) {
	return s.CreateNodeEnrollmentTokenWithOptions(ctx, nodeID, ttl, WriteOptions{Actor: "system"})
}

func (s *ResourceRepository) CreateNodeEnrollmentTokenWithOptions(ctx context.Context, nodeID string, ttl time.Duration, options WriteOptions) (string, EnrollmentToken, error) {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return "", EnrollmentToken{}, err
	}
	return s.createEnrollmentTokenWithOptions(ctx, nodeID, ttl, options)
}

func (s *ResourceRepository) createEnrollmentTokenWithOptions(ctx context.Context, nodeID string, ttl time.Duration, options WriteOptions) (string, EnrollmentToken, error) {
	if ttl <= 0 {
		ttl = EnrollmentTTL
	}
	if ttl > EnrollmentTTL {
		return "", EnrollmentToken{}, errors.New("enrollment token lifetime cannot exceed 15 minutes")
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)
	request := map[string]any{"ttl_seconds": int64(ttl / time.Second)}
	if nodeID != "" {
		request["node_id"] = nodeID
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return "", EnrollmentToken{}, err
	}
	defer tx.Rollback()
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return "", EnrollmentToken{}, err
	}
	if hit {
		var response []byte
		if err := tx.QueryRowContext(ctx, `SELECT response_json FROM idempotency_keys WHERE key=?`, strings.TrimSpace(options.IdempotencyKey)).Scan(&response); err != nil {
			return "", EnrollmentToken{}, err
		}
		var metadata struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(response, &metadata); err != nil || metadata.ID == "" {
			return "", EnrollmentToken{}, errors.New("idempotency response is invalid")
		}
		var token EnrollmentToken
		var expiry, created string
		var used sqlNullString
		if err := tx.QueryRowContext(ctx, `SELECT id,expires_at,used_at,created_at FROM enrollment_tokens WHERE id=?`, metadata.ID).Scan(&token.ID, &expiry, &used, &created); err != nil {
			return "", EnrollmentToken{}, err
		}
		var parseErr error
		token.ExpiresAt, parseErr = parseStoredTime("enrollment_token.expires_at", expiry)
		if parseErr != nil {
			return "", EnrollmentToken{}, parseErr
		}
		token.CreatedAt, parseErr = parseStoredTime("enrollment_token.created_at", created)
		if parseErr != nil {
			return "", EnrollmentToken{}, parseErr
		}
		if used.Valid {
			value, parseErr := parseStoredTime("enrollment_token.used_at", used.String)
			if parseErr != nil {
				return "", EnrollmentToken{}, parseErr
			}
			token.UsedAt = &value
		}
		if err := s.commitWriteTx(ctx, tx); err != nil {
			return "", EnrollmentToken{}, err
		}
		return "", token, ErrSecretAlreadyCreated
	}
	plain, digest, err := NewAPIToken()
	if err != nil {
		return "", EnrollmentToken{}, err
	}
	if nodeID != "" {
		var enabled int
		if err := tx.QueryRowContext(ctx, `SELECT enabled FROM nodes WHERE id=?`, nodeID).Scan(&enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", EnrollmentToken{}, ErrNodeNotEnrolled
			}
			return "", EnrollmentToken{}, err
		}
		if enabled == 0 {
			return "", EnrollmentToken{}, fmt.Errorf("%w: node is disabled", ErrNodeEnrollmentNotAllowed)
		}
		plain = nodeEnrollmentToken(nodeID, plain)
		digest = HashToken(plain)
	}
	// Enrollment tokens use the same one-way digest format as API tokens. The
	// plaintext is returned exactly once and is never persisted.
	id, err := randomID()
	if err != nil {
		return "", EnrollmentToken{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO enrollment_tokens(id,token_hash,expires_at,created_at) VALUES(?,?,?,?)`, id, digest, expires.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return "", EnrollmentToken{}, err
	}
	if err := insertAudit(ctx, tx, options.Actor, "create", "enrollment_token", id, 1, nil); err != nil {
		return "", EnrollmentToken{}, err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]string{"id": id}); err != nil {
		return "", EnrollmentToken{}, err
	}
	if err := s.commitWriteTx(ctx, tx); err != nil {
		return "", EnrollmentToken{}, err
	}
	return plain, EnrollmentToken{ID: id, ExpiresAt: expires, CreatedAt: now}, nil
}

const nodeEnrollmentTokenPrefix = "afn_"

func nodeEnrollmentToken(nodeID, randomToken string) string {
	return nodeEnrollmentTokenPrefix + hex.EncodeToString([]byte(nodeID)) + "_" + strings.TrimPrefix(randomToken, "af_")
}

func parseNodeEnrollmentToken(token string) (nodeID string, bound bool, err error) {
	if !strings.HasPrefix(token, nodeEnrollmentTokenPrefix) {
		return "", false, nil
	}
	rest := strings.TrimPrefix(token, nodeEnrollmentTokenPrefix)
	parts := strings.SplitN(rest, "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", true, errors.New("node enrollment token is malformed")
	}
	decoded, err := hex.DecodeString(parts[0])
	if err != nil || len(decoded) == 0 {
		return "", true, errors.New("node enrollment token node binding is malformed")
	}
	nodeID = string(decoded)
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return "", true, errors.New("node enrollment token node binding is invalid")
	}
	return nodeID, true, nil
}

func (s *ResourceRepository) ListEnrollmentTokens(ctx context.Context) ([]EnrollmentToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,expires_at,used_at,created_at FROM enrollment_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []EnrollmentToken{}
	for rows.Next() {
		var token EnrollmentToken
		var expires, created string
		var used sqlNullString
		if err := rows.Scan(&token.ID, &expires, &used, &created); err != nil {
			return nil, err
		}
		var parseErr error
		token.ExpiresAt, parseErr = parseStoredTime("enrollment_token.expires_at", expires)
		if parseErr != nil {
			return nil, parseErr
		}
		token.CreatedAt, parseErr = parseStoredTime("enrollment_token.created_at", created)
		if parseErr != nil {
			return nil, parseErr
		}
		if used.Valid {
			value, parseErr := parseStoredTime("enrollment_token.used_at", used.String)
			if parseErr != nil {
				return nil, parseErr
			}
			token.UsedAt = &value
		}
		result = append(result, token)
	}
	return result, rows.Err()
}
