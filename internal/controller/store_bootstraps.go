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
	NodeID string `json:"node_id"`
	// Role is a legacy internal hint. The pending resource exposes only the
	// optional configured spec kind; a generic installation may have neither.
	Role      string            `json:"-"`
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
	TokenHash       string
	GatewaySpecJSON []byte
	AgentSpecJSON   []byte
}

// CreatePendingNodeBootstrap stores only the installation intent and a hash
// of the one-time enrollment token. The node identity and its business spec
// are created later by IssueNodeCertificate, after the installer reaches the
// Controller with the token.
func (s *Store) CreatePendingNodeBootstrap(ctx context.Context, node domain.Node, platform, arch string, gatewaySpec *domain.GatewaySpec, agentSpec *domain.AgentSpec, options WriteOptions) (string, PendingNodeBootstrap, error) {
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

	var gatewayJSON, agentJSON []byte
	if gatewaySpec != nil && agentSpec != nil {
		return "", PendingNodeBootstrap{}, &domain.ApplyError{Code: "invalid_spec", Path: "spec", Message: "a pending node can contain only one behavior spec"}
	}
	if node.Role != "" && node.Role != domain.RoleGateway && node.Role != domain.RoleAgent {
		return "", PendingNodeBootstrap{}, &domain.ApplyError{Code: "invalid_spec_kind", Path: "spec.kind", Message: "node behavior must be gateway or agent"}
	}
	if gatewaySpec != nil {
		if node.Role != "" && node.Role != domain.RoleGateway {
			return "", PendingNodeBootstrap{}, &domain.ApplyError{Code: "invalid_spec_kind", Path: "spec.kind", Message: "gateway spec does not match node behavior"}
		}
		node.Role = domain.RoleGateway
		spec := *gatewaySpec
		spec.NodeID = node.ID
		spec.Revision = 0
		if spec.Labels == nil {
			spec.Labels = cloneStringMap(node.Labels)
		}
		if err := spec.Validate(); err != nil {
			return "", PendingNodeBootstrap{}, err
		}
		if err := s.protectObfuscationPolicy(&spec.Obfuscation); err != nil {
			return "", PendingNodeBootstrap{}, err
		}
		gatewayJSON, err = json.Marshal(spec)
		if err != nil {
			return "", PendingNodeBootstrap{}, err
		}
	} else if agentSpec != nil || node.Role == domain.RoleAgent {
		if node.Role != "" && node.Role != domain.RoleAgent {
			return "", PendingNodeBootstrap{}, &domain.ApplyError{Code: "invalid_spec_kind", Path: "spec.kind", Message: "agent spec does not match node behavior"}
		}
		node.Role = domain.RoleAgent
		if agentSpec == nil {
			spec := domain.AgentSpec{
				NodeID:  node.ID,
				Limits:  domain.AgentLimits{MaxConnections: 4096, MaxStreams: 1024, MaxBufferBytes: 64 << 20},
				Logging: domain.LoggingPolicy{Level: "info", Format: "json"},
			}
			agentSpec = &spec
		}
		spec := *agentSpec
		spec.NodeID = node.ID
		spec.Revision = 0
		if err := spec.Validate(); err != nil {
			return "", PendingNodeBootstrap{}, err
		}
		agentJSON, err = json.Marshal(spec)
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
		NodeID: node.ID, Role: node.Role, Name: node.Name, Labels: cloneStringMap(node.Labels),
		Enabled: node.Enabled, Platform: platform, Arch: arch,
		ExpiresAt: now.Add(EnrollmentTTL), CreatedAt: now,
	}
	if len(gatewayJSON) > 0 {
		pending.SpecKind = string(domain.NodeSpecGateway)
	} else if len(agentJSON) > 0 {
		pending.SpecKind = string(domain.NodeSpecAgent)
	}
	request := pendingBootstrapRequestForHash(pending, gatewaySpec, agentSpec)

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
	_, err = tx.ExecContext(ctx, `INSERT INTO node_bootstraps(node_id,role,name,labels_json,enabled,platform,arch,gateway_spec_json,agent_spec_json,token_hash,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		node.ID, node.Role, node.Name, labels, boolInt(node.Enabled), platform, arch, nullableJSON(gatewayJSON), nullableJSON(agentJSON), tokenHash, pending.ExpiresAt.Format(time.RFC3339Nano), pending.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return "", PendingNodeBootstrap{}, fmt.Errorf("create pending node bootstrap: %w", err)
	}
	if err := insertAudit(ctx, tx, options.Actor, "create", "node_bootstrap", node.ID, 0, map[string]string{"role": node.Role, "platform": platform, "arch": arch}); err != nil {
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

func pendingBootstrapRequestForHash(pending PendingNodeBootstrap, gatewaySpec *domain.GatewaySpec, agentSpec *domain.AgentSpec) any {
	request := struct {
		NodeID      string            `json:"node_id"`
		Role        string            `json:"role"`
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels,omitempty"`
		Enabled     bool              `json:"enabled"`
		Platform    string            `json:"platform"`
		Arch        string            `json:"arch"`
		GatewaySpec any               `json:"gateway_spec,omitempty"`
		AgentSpec   any               `json:"agent_spec,omitempty"`
	}{NodeID: pending.NodeID, Role: pending.Role, Name: pending.Name, Labels: pending.Labels, Enabled: pending.Enabled, Platform: pending.Platform, Arch: pending.Arch}
	if gatewaySpec != nil {
		spec := *gatewaySpec
		spec.NodeID = pending.NodeID
		spec.Revision = 0
		spec.Obfuscation = obfuscationRequestPolicyWithKeyIDs(spec.Obfuscation)
		request.GatewaySpec = spec
	}
	if agentSpec != nil {
		spec := *agentSpec
		spec.NodeID = pending.NodeID
		spec.Revision = 0
		request.AgentSpec = spec
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

const pendingBootstrapSelect = `SELECT node_id,role,name,labels_json,enabled,platform,arch,expires_at,created_at,CASE WHEN gateway_spec_json IS NOT NULL THEN 'gateway' WHEN agent_spec_json IS NOT NULL THEN 'agent' ELSE '' END FROM node_bootstraps`

func loadPendingBootstrapTx(ctx context.Context, tx *sql.Tx, nodeID string) (pendingNodeBootstrap, error) {
	return scanPendingBootstrapWithSpec(tx.QueryRowContext(ctx, `SELECT node_id,role,name,labels_json,enabled,platform,arch,expires_at,created_at,token_hash,gateway_spec_json,agent_spec_json FROM node_bootstraps WHERE node_id=?`, nodeID))
}

func (s *Store) pendingBootstrapForToken(ctx context.Context, tokenHash string) (pendingNodeBootstrap, error) {
	return scanPendingBootstrapWithSpec(s.db.QueryRowContext(ctx, `SELECT node_id,role,name,labels_json,enabled,platform,arch,expires_at,created_at,token_hash,gateway_spec_json,agent_spec_json FROM node_bootstraps WHERE token_hash=?`, tokenHash))
}

func scanPendingBootstrap(row scanner) (PendingNodeBootstrap, error) {
	var pending PendingNodeBootstrap
	var labels []byte
	var enabled int
	var expires, created string
	if err := row.Scan(&pending.NodeID, &pending.Role, &pending.Name, &labels, &enabled, &pending.Platform, &pending.Arch, &expires, &created, &pending.SpecKind); err != nil {
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
	var labels, gatewayJSON, agentJSON []byte
	var enabled int
	var expires, created string
	if err := row.Scan(&pending.NodeID, &pending.Role, &pending.Name, &labels, &enabled, &pending.Platform, &pending.Arch, &expires, &created, &pending.TokenHash, &gatewayJSON, &agentJSON); err != nil {
		return pendingNodeBootstrap{}, err
	}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &pending.Labels); err != nil {
			return pendingNodeBootstrap{}, err
		}
	}
	pending.Enabled = enabled != 0
	pending.GatewaySpecJSON = append([]byte(nil), gatewayJSON...)
	pending.AgentSpecJSON = append([]byte(nil), agentJSON...)
	if len(gatewayJSON) > 0 {
		pending.SpecKind = string(domain.NodeSpecGateway)
	} else if len(agentJSON) > 0 {
		pending.SpecKind = string(domain.NodeSpecAgent)
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
	return domain.Node{ID: p.NodeID, Role: p.Role, Name: p.Name, Labels: cloneStringMap(p.Labels), Enabled: p.Enabled}
}
