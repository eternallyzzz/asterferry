package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"asterferry/internal/domain"
)

// PendingNodeBootstrap is the non-secret metadata for an installation command
// that has been issued but has not enrolled yet. It is deliberately separate
// from domain.Node: an installation that has never reached the Controller is
// not an enrolled identity and must not be schedulable.
type PendingNodeBootstrap struct {
	NodeID    string            `json:"node_id"`
	SpecKind  string            `json:"spec_kind,omitempty"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Enabled   bool              `json:"enabled"`
	Platform  string            `json:"platform"`
	Arch      string            `json:"arch"`
	ExpiresAt time.Time         `json:"expires_at"`
	CreatedAt time.Time         `json:"created_at"`
}

type pendingNodeBootstrap struct {
	PendingNodeBootstrap
	TokenHash string
	SpecJSON  []byte
}

// CreatePendingNodeBootstrap stores only the installation intent and a hash
// of the one-time enrollment token. The node identity and its business spec
// are created later by IssueNodeCertificate, after the installer reaches the
// Controller with the token.
func (s *Store) CreatePendingNodeBootstrap(ctx context.Context, node domain.Node, platform, arch string, spec *domain.NodeSpec, options WriteOptions) (string, PendingNodeBootstrap, error) {
	if err := node.Validate(); err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	if node.CertificateState != "" || node.CertificateSerial != "" || node.Revision != 0 || !node.CreatedAt.IsZero() || !node.UpdatedAt.IsZero() {
		return "", PendingNodeBootstrap{}, &domain.ApplyError{Code: "invalid_bootstrap_node", Path: "node", Message: "pending installation node metadata must not contain certificate or repository state"}
	}
	platform, arch, err := normalizeBootstrapPlatform(platform, arch)
	if err != nil {
		return "", PendingNodeBootstrap{}, err
	}

	var specJSON []byte
	if spec != nil {
		value := *spec
		value.NodeID = node.ID
		value.Revision = 0
		value.UpdatedAt = time.Time{}
		if value.Gateway != nil {
			value.Gateway.NodeID = node.ID
			if value.Gateway.Labels == nil {
				value.Gateway.Labels = cloneStringMap(node.Labels)
			}
			if err := s.protectObfuscationPolicy(&value.Gateway.Obfuscation); err != nil {
				return "", PendingNodeBootstrap{}, err
			}
		}
		if value.Agent != nil {
			value.Agent.NodeID = node.ID
		}
		if err := value.Validate(); err != nil {
			return "", PendingNodeBootstrap{}, err
		}
		specJSON, err = json.Marshal(value)
		if err != nil {
			return "", PendingNodeBootstrap{}, err
		}
	}

	plain, _, err := NewAPIToken()
	if err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	plain = nodeEnrollmentToken(node.ID, plain)
	tokenHash := HashToken(plain)
	now := time.Now().UTC()
	pending := PendingNodeBootstrap{
		NodeID: node.ID, Name: node.Name, Labels: cloneStringMap(node.Labels),
		Enabled: node.Enabled, Platform: platform, Arch: arch,
		ExpiresAt: now.Add(EnrollmentTTL), CreatedAt: now,
	}
	if spec != nil {
		pending.SpecKind = string(spec.Kind)
	}
	request := pendingBootstrapRequestForHash(pending, spec)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	defer tx.Rollback()
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	if hit {
		stored, loadErr := loadPendingBootstrapFromIdempotency(ctx, tx, options.IdempotencyKey)
		if loadErr != nil {
			return "", PendingNodeBootstrap{}, loadErr
		}
		if err := tx.Commit(); err != nil {
			return "", PendingNodeBootstrap{}, err
		}
		return "", stored.PendingNodeBootstrap, ErrSecretAlreadyCreated
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id=?`, node.ID).Scan(&exists); err == nil {
		return "", PendingNodeBootstrap{}, &domain.ApplyError{Code: "already_exists", Path: "node.id", Message: "an enrolled node with this ID already exists"}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", PendingNodeBootstrap{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM node_bootstraps WHERE node_id=?`, node.ID).Scan(&exists); err == nil {
		return "", PendingNodeBootstrap{}, &domain.ApplyError{Code: "bootstrap_pending", Path: "node.id", Message: "an installation command for this node is already pending; reissue it from the pending installations list"}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", PendingNodeBootstrap{}, err
	}
	labels, err := json.Marshal(node.Labels)
	if err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO node_bootstraps(node_id,name,labels_json,enabled,platform,arch,spec_json,token_hash,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		node.ID, node.Name, labels, boolInt(node.Enabled), platform, arch, nullableJSON(specJSON), tokenHash, pending.ExpiresAt.Format(time.RFC3339Nano), pending.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return "", PendingNodeBootstrap{}, fmt.Errorf("create pending node bootstrap: %w", err)
	}
	if err := insertAudit(ctx, tx, options.Actor, "create", "node_bootstrap", node.ID, 0, map[string]string{"platform": platform, "arch": arch}); err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, pendingBootstrapIdempotencyResponse(pending)); err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	return plain, pending, nil
}

func pendingBootstrapRequestForHash(pending PendingNodeBootstrap, spec *domain.NodeSpec) any {
	request := struct {
		NodeID   string            `json:"node_id"`
		Name     string            `json:"name"`
		Labels   map[string]string `json:"labels,omitempty"`
		Enabled  bool              `json:"enabled"`
		Platform string            `json:"platform"`
		Arch     string            `json:"arch"`
		Spec     any               `json:"spec,omitempty"`
	}{NodeID: pending.NodeID, Name: pending.Name, Labels: pending.Labels, Enabled: pending.Enabled, Platform: pending.Platform, Arch: pending.Arch}
	if spec != nil {
		value := *spec
		value.NodeID = pending.NodeID
		value.Revision = 0
		if value.Gateway != nil {
			value.Gateway.Obfuscation = obfuscationRequestPolicyWithKeyIDs(value.Gateway.Obfuscation)
		}
		request.Spec = value
	}
	return request
}

func obfuscationRequestPolicyWithKeyIDs(policy domain.ObfuscationPolicy) domain.ObfuscationPolicy {
	result := obfuscationRequestPolicy(policy)
	if result.KeyID == "" && len(policy.Key) > 0 {
		result.KeyID = obfuscationKeyID(policy.Key)
	}
	if result.PreviousKeyID == "" && len(policy.PreviousKey) > 0 {
		result.PreviousKeyID = obfuscationKeyID(policy.PreviousKey)
	}
	return result
}

func nullableJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func loadPendingBootstrapFromIdempotency(ctx context.Context, tx *sql.Tx, key string) (pendingNodeBootstrap, error) {
	var response []byte
	if err := tx.QueryRowContext(ctx, `SELECT response_json FROM idempotency_keys WHERE key=?`, strings.TrimSpace(key)).Scan(&response); err != nil {
		return pendingNodeBootstrap{}, err
	}
	// Keep enough non-secret metadata in the idempotency record to answer a
	// retry even if the installer already consumed the pending row. The token
	// itself is never persisted and therefore remains unrecoverable.
	var replay struct {
		Pending *PendingNodeBootstrap `json:"pending"`
	}
	if err := json.Unmarshal(response, &replay); err == nil && replay.Pending != nil && replay.Pending.NodeID != "" {
		return pendingNodeBootstrap{PendingNodeBootstrap: *replay.Pending}, nil
	}
	var metadata struct {
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(response, &metadata); err != nil || metadata.NodeID == "" {
		return pendingNodeBootstrap{}, errors.New("pending bootstrap idempotency response is invalid")
	}
	return loadPendingBootstrapTx(ctx, tx, metadata.NodeID)
}

func pendingBootstrapIdempotencyResponse(pending PendingNodeBootstrap) map[string]any {
	return map[string]any{"node_id": pending.NodeID, "pending": pending}
}

func (s *Store) GetPendingNodeBootstrap(ctx context.Context, nodeID string) (PendingNodeBootstrap, error) {
	return scanPendingBootstrap(s.db.QueryRowContext(ctx, pendingBootstrapSelect+` WHERE node_id=?`, nodeID))
}

func (s *Store) ListPendingNodeBootstraps(ctx context.Context) ([]PendingNodeBootstrap, error) {
	rows, err := s.db.QueryContext(ctx, pendingBootstrapSelect+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]PendingNodeBootstrap, 0)
	for rows.Next() {
		pending, err := scanPendingBootstrap(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, pending)
	}
	return result, rows.Err()
}

func (s *Store) ReissuePendingNodeBootstrap(ctx context.Context, nodeID string, options WriteOptions) (string, PendingNodeBootstrap, error) {
	request := struct {
		NodeID string `json:"node_id"`
		Action string `json:"action"`
	}{NodeID: nodeID, Action: "reissue"}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	defer tx.Rollback()
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	if hit {
		pending, loadErr := loadPendingBootstrapFromIdempotency(ctx, tx, options.IdempotencyKey)
		if loadErr != nil {
			return "", PendingNodeBootstrap{}, loadErr
		}
		if err := tx.Commit(); err != nil {
			return "", PendingNodeBootstrap{}, err
		}
		return "", pending.PendingNodeBootstrap, ErrSecretAlreadyCreated
	}
	pending, err := loadPendingBootstrapTx(ctx, tx, nodeID)
	if err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	plain, _, err := NewAPIToken()
	if err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	plain = nodeEnrollmentToken(nodeID, plain)
	now := time.Now().UTC()
	pending.ExpiresAt = now.Add(EnrollmentTTL)
	if _, err := tx.ExecContext(ctx, `UPDATE node_bootstraps SET token_hash=?,expires_at=? WHERE node_id=?`, HashToken(plain), pending.ExpiresAt.Format(time.RFC3339Nano), nodeID); err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	if err := insertAudit(ctx, tx, options.Actor, "reissue", "node_bootstrap", nodeID, 0, nil); err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, pendingBootstrapIdempotencyResponse(pending.PendingNodeBootstrap)); err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", PendingNodeBootstrap{}, err
	}
	return plain, pending.PendingNodeBootstrap, nil
}

func (s *Store) DeletePendingNodeBootstrap(ctx context.Context, nodeID string, options WriteOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request := struct {
		NodeID string `json:"node_id"`
		Action string `json:"action"`
	}{NodeID: nodeID, Action: "delete"}
	hit, err := idempotencyHit(ctx, tx, options.IdempotencyKey, request)
	if err != nil {
		return err
	}
	if hit {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM node_bootstraps WHERE node_id=?`, nodeID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	if err := insertAudit(ctx, tx, options.Actor, "delete", "node_bootstrap", nodeID, 0, nil); err != nil {
		return err
	}
	if err := recordIdempotency(ctx, tx, options.IdempotencyKey, request, map[string]any{"node_id": nodeID}); err != nil {
		return err
	}
	return tx.Commit()
}

const pendingBootstrapSelect = `SELECT node_id,name,labels_json,enabled,platform,arch,expires_at,created_at,CASE WHEN spec_json IS NOT NULL THEN json_extract(spec_json,'$.kind') ELSE '' END FROM node_bootstraps`

func loadPendingBootstrapTx(ctx context.Context, tx *sql.Tx, nodeID string) (pendingNodeBootstrap, error) {
	return scanPendingBootstrapWithSpec(tx.QueryRowContext(ctx, `SELECT node_id,name,labels_json,enabled,platform,arch,expires_at,created_at,token_hash,spec_json FROM node_bootstraps WHERE node_id=?`, nodeID))
}

func (s *Store) pendingBootstrapForToken(ctx context.Context, tokenHash string) (pendingNodeBootstrap, error) {
	return scanPendingBootstrapWithSpec(s.db.QueryRowContext(ctx, `SELECT node_id,name,labels_json,enabled,platform,arch,expires_at,created_at,token_hash,spec_json FROM node_bootstraps WHERE token_hash=?`, tokenHash))
}

func scanPendingBootstrap(row scanner) (PendingNodeBootstrap, error) {
	var pending PendingNodeBootstrap
	var labels []byte
	var enabled int
	var expires, created string
	if err := row.Scan(&pending.NodeID, &pending.Name, &labels, &enabled, &pending.Platform, &pending.Arch, &expires, &created, &pending.SpecKind); err != nil {
		return PendingNodeBootstrap{}, err
	}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &pending.Labels); err != nil {
			return PendingNodeBootstrap{}, err
		}
	}
	pending.Enabled = enabled != 0
	var err error
	pending.ExpiresAt, err = parseStoredTime("node_bootstrap.expires_at", expires)
	if err != nil {
		return PendingNodeBootstrap{}, err
	}
	pending.CreatedAt, err = parseStoredTime("node_bootstrap.created_at", created)
	if err != nil {
		return PendingNodeBootstrap{}, err
	}
	return pending, nil
}

func scanPendingBootstrapWithSpec(row scanner) (pendingNodeBootstrap, error) {
	var pending pendingNodeBootstrap
	var labels, specJSON []byte
	var enabled int
	var expires, created string
	if err := row.Scan(&pending.NodeID, &pending.Name, &labels, &enabled, &pending.Platform, &pending.Arch, &expires, &created, &pending.TokenHash, &specJSON); err != nil {
		return pendingNodeBootstrap{}, err
	}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &pending.Labels); err != nil {
			return pendingNodeBootstrap{}, err
		}
	}
	pending.Enabled = enabled != 0
	pending.SpecJSON = append([]byte(nil), specJSON...)
	if len(specJSON) > 0 {
		var spec domain.NodeSpec
		if err := json.Unmarshal(specJSON, &spec); err == nil {
			pending.SpecKind = string(spec.Kind)
		}
	}
	var err error
	pending.ExpiresAt, err = parseStoredTime("node_bootstrap.expires_at", expires)
	if err != nil {
		return pendingNodeBootstrap{}, err
	}
	pending.CreatedAt, err = parseStoredTime("node_bootstrap.created_at", created)
	if err != nil {
		return pendingNodeBootstrap{}, err
	}
	return pending, nil
}

func (p PendingNodeBootstrap) node() domain.Node {
	return domain.Node{ID: p.NodeID, Name: p.Name, Labels: cloneStringMap(p.Labels), Enabled: p.Enabled}
}
