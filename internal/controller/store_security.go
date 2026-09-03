package controller

import (
	"context"
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

func idempotencyHit(ctx context.Context, tx *sql.Tx, key string, request any) (bool, error) {
	key = strings.TrimSpace(key)
	if err := validateIdempotencyKey(key); err != nil {
		return false, err
	}
	if key == "" {
		return false, nil
	}
	hash, err := requestHash(request)
	if err != nil {
		return false, err
	}
	// Reserve the key in the same transaction as the business mutation. On
	// PostgreSQL, a concurrent INSERT for the same key waits for the first
	// transaction to commit or roll back, closing the SELECT-then-INSERT TOCTOU
	// window. The placeholder is never committed without recordIdempotency
	// because both operations share the caller's transaction.
	result, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys(key,request_hash,response_json,created_at) VALUES(?,?,?,?) ON CONFLICT(key) DO NOTHING`, key, hash, []byte(`{}`), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reserve idempotency key rows affected: %w", err)
	}
	if affected == 1 {
		return false, nil
	}
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT request_hash FROM idempotency_keys WHERE key=?`, key).Scan(&stored)
	if err != nil {
		return false, err
	}
	if stored != hash {
		return false, errors.New("idempotency key was used for a different request")
	}
	return true, nil
}

func recordIdempotency(ctx context.Context, tx *sql.Tx, key string, request, response any) error {
	key = strings.TrimSpace(key)
	if err := validateIdempotencyKey(key); err != nil {
		return err
	}
	if key == "" {
		return nil
	}
	hash, err := requestHash(request)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE idempotency_keys SET response_json=? WHERE key=? AND request_hash=?`, encoded, key, hash)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("record idempotency key rows affected: %w", err)
	}
	if affected != 1 {
		return errors.New("idempotency key reservation was not held")
	}
	return nil
}

func validateIdempotencyKey(key string) error {
	if len(key) > 128 || strings.ContainsAny(key, "\x00\r\n") {
		return errors.New("idempotency key is invalid")
	}
	return nil
}

func requestHash(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) protectObfuscationPolicy(policy *domain.ObfuscationPolicy) error {
	if policy == nil {
		return nil
	}
	if policy.Mode == "" || policy.Mode == "standard" {
		policy.Key = nil
		policy.PreviousKey = nil
		policy.KeyCiphertext = nil
		policy.PreviousKeyCiphertext = nil
		policy.KeyID = ""
		policy.PreviousKeyID = ""
		return nil
	}
	if policy.Mode != "camouflage" {
		return &domain.ApplyError{Code: "invalid_obfuscation", Path: "obfuscation.mode", Message: "obfuscation mode must be standard or camouflage"}
	}
	if len(policy.Key) > 0 {
		if len(policy.Key) != 32 {
			return errors.New("data-plane obfuscation key must contain exactly 32 bytes")
		}
		ciphertext, err := EncryptSecret(s.masterKey[:], policy.Key)
		if err != nil {
			return fmt.Errorf("encrypt obfuscation key: %w", err)
		}
		policy.KeyCiphertext = ciphertext
		policy.KeyID = obfuscationKeyID(policy.Key)
		policy.Key = nil
	}
	if len(policy.KeyCiphertext) == 0 {
		return errors.New("camouflage mode requires a protected current key")
	}
	current, err := DecryptSecret(s.masterKey[:], policy.KeyCiphertext)
	if err != nil || len(current) != 32 {
		return errors.New("camouflage current key is not a valid Controller-encrypted 32-byte key")
	}
	currentKeyID := domain.ObfuscationKeyID(current)
	if policy.KeyID != "" && policy.KeyID != currentKeyID {
		return errors.New("obfuscation key id does not match the encrypted current key")
	}
	policy.KeyID = currentKeyID
	if len(policy.PreviousKey) > 0 {
		if len(policy.PreviousKey) != 32 {
			return errors.New("previous data-plane obfuscation key must contain exactly 32 bytes")
		}
		ciphertext, err := EncryptSecret(s.masterKey[:], policy.PreviousKey)
		if err != nil {
			return fmt.Errorf("encrypt previous obfuscation key: %w", err)
		}
		policy.PreviousKeyCiphertext = ciphertext
		policy.PreviousKeyID = obfuscationKeyID(policy.PreviousKey)
		policy.PreviousKey = nil
	}
	if len(policy.PreviousKeyCiphertext) > 0 {
		previous, err := DecryptSecret(s.masterKey[:], policy.PreviousKeyCiphertext)
		if err != nil || len(previous) != 32 {
			return errors.New("previous obfuscation key is not a valid Controller-encrypted 32-byte key")
		}
		previousKeyID := domain.ObfuscationKeyID(previous)
		if policy.PreviousKeyID != "" && policy.PreviousKeyID != previousKeyID {
			return errors.New("obfuscation previous key id does not match the encrypted previous key")
		}
		policy.PreviousKeyID = previousKeyID
	}
	policy.Key = nil
	policy.PreviousKey = nil
	return nil
}

func obfuscationKeyID(key []byte) string {
	return domain.ObfuscationKeyID(key)
}

func obfuscationRequestPolicy(policy domain.ObfuscationPolicy) domain.ObfuscationPolicy {
	return domain.ObfuscationPolicy{Mode: policy.Mode, KeyID: policy.KeyID, PreviousKeyID: policy.PreviousKeyID, MaxPaddingBytes: policy.MaxPaddingBytes, HandshakeShaping: policy.HandshakeShaping}
}

// sameObfuscationPolicy compares the non-secret policy identity used by both
// the Gateway listener and the Agent dialer. Ciphertexts are intentionally
// excluded: re-encrypting the same key does not change data-plane behavior,
// while a key/policy identity change must produce a new assignment generation.
func sameObfuscationPolicy(left, right domain.ObfuscationPolicy) bool {
	leftMode := left.Mode
	if leftMode == "" {
		leftMode = "standard"
	}
	rightMode := right.Mode
	if rightMode == "" {
		rightMode = "standard"
	}
	return leftMode == rightMode &&
		left.KeyID == right.KeyID &&
		left.PreviousKeyID == right.PreviousKeyID &&
		left.MaxPaddingBytes == right.MaxPaddingBytes &&
		left.HandshakeShaping == right.HandshakeShaping
}

// SnapshotDocumentForWire decrypts only the data-plane keys needed by an
// authenticated node. The persisted desired snapshot retains ciphertext and
// is never sent to a node as key material; the returned document is ephemeral
// and contains plaintext keys protected by the mTLS control stream.
func (s *Store) SnapshotDocumentForWire(document []byte) ([]byte, error) {
	var snapshot domain.DesiredSnapshot
	if err := json.Unmarshal(document, &snapshot); err != nil {
		return nil, fmt.Errorf("decode snapshot for wire: %w", err)
	}
	if snapshot.Gateway != nil {
		if err := s.decryptObfuscationPolicyForWire(&snapshot.Gateway.Obfuscation); err != nil {
			return nil, err
		}
	}
	for index := range snapshot.Assignments {
		if err := s.decryptObfuscationPolicyForWire(&snapshot.Assignments[index].Obfuscation); err != nil {
			return nil, fmt.Errorf("assignment %q obfuscation: %w", snapshot.Assignments[index].ID, err)
		}
	}
	if err := snapshot.Validate(); err != nil {
		return nil, fmt.Errorf("snapshot for wire is invalid: %w", err)
	}
	return json.Marshal(snapshot)
}

func (s *Store) decryptObfuscationPolicyForWire(policy *domain.ObfuscationPolicy) error {
	if policy == nil || policy.Mode == "" || policy.Mode == "standard" {
		if policy != nil {
			policy.Key = nil
			policy.PreviousKey = nil
			policy.KeyCiphertext = nil
			policy.PreviousKeyCiphertext = nil
		}
		return nil
	}
	if len(policy.Key) == 0 {
		key, err := DecryptSecret(s.masterKey[:], policy.KeyCiphertext)
		if err != nil || len(key) != 32 {
			return errors.New("current obfuscation key cannot be decrypted")
		}
		policy.Key = key
	}
	if policy.KeyID != "" && policy.KeyID != domain.ObfuscationKeyID(policy.Key) {
		return errors.New("wire obfuscation key id does not match current key")
	}
	policy.KeyID = domain.ObfuscationKeyID(policy.Key)
	if len(policy.PreviousKeyCiphertext) > 0 && len(policy.PreviousKey) == 0 {
		key, err := DecryptSecret(s.masterKey[:], policy.PreviousKeyCiphertext)
		if err != nil || len(key) != 32 {
			return errors.New("previous obfuscation key cannot be decrypted")
		}
		policy.PreviousKey = key
	}
	if len(policy.Key) != 32 || (len(policy.PreviousKey) != 0 && len(policy.PreviousKey) != 32) {
		return errors.New("wire obfuscation key has an invalid length")
	}
	if len(policy.PreviousKey) > 0 {
		previousKeyID := domain.ObfuscationKeyID(policy.PreviousKey)
		if policy.PreviousKeyID != "" && policy.PreviousKeyID != previousKeyID {
			return errors.New("wire obfuscation previous key id does not match previous key")
		}
		policy.PreviousKeyID = previousKeyID
	}
	policy.KeyCiphertext = nil
	policy.PreviousKeyCiphertext = nil
	return nil
}
