package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"asterferry/internal/domain"
)

// issuePendingNodeCertificate completes the install-first lifecycle. The
// pending intent, enrollment token, node identity and initial spec are all
// committed together, so a failed or racing enrollment cannot leave a node
// that has no certificate, or a certificate for a node that has no spec.
func (s *Store) issuePendingNodeCertificate(ctx context.Context, config Config, token, role, nodeID string, csrDER []byte, pending pendingNodeBootstrap) (Certificate, error) {
	if pending.Role != "" && pending.Role != role {
		return Certificate{}, ErrEnrollmentRoleMismatch
	}
	if pending.NodeID != nodeID {
		return Certificate{}, ErrEnrollmentNodeMismatch
	}
	if !time.Now().UTC().Before(pending.ExpiresAt) {
		return Certificate{}, ErrEnrollmentTokenExpired
	}
	request, err := parseEnrollmentCSR(csrDER, nodeID, role)
	if err != nil {
		return Certificate{}, err
	}
	caCert, caKey, err := readCA(config.CACertPath, config.CAKeyPath)
	if err != nil {
		return Certificate{}, err
	}
	caPEM, err := os.ReadFile(config.CACertPath)
	if err != nil {
		return Certificate{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Certificate{}, storageFailure("begin pending enrollment transaction", err)
	}
	defer tx.Rollback()
	current, err := loadPendingBootstrapTx(ctx, tx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return Certificate{}, ErrEnrollmentTokenUsed
	}
	if err != nil {
		return Certificate{}, storageFailure("reload pending node bootstrap", err)
	}
	if current.TokenHash != HashToken(token) {
		return Certificate{}, ErrInvalidEnrollmentToken
	}
	if current.Role != "" && current.Role != role {
		return Certificate{}, ErrEnrollmentRoleMismatch
	}
	if !time.Now().UTC().Before(current.ExpiresAt) {
		return Certificate{}, ErrEnrollmentTokenExpired
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id=?`, nodeID).Scan(&exists); err == nil {
		return Certificate{}, ErrNodeEnrollmentNotAllowed
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Certificate{}, storageFailure("check pending node identity", err)
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM node_bootstraps WHERE node_id=? AND token_hash=?`, nodeID, HashToken(token))
	if err != nil {
		return Certificate{}, storageFailure("consume pending node bootstrap", err)
	}
	if affected, affectedErr := deleted.RowsAffected(); affectedErr != nil {
		return Certificate{}, storageFailure("consume pending node bootstrap rows affected", affectedErr)
	} else if affected != 1 {
		return Certificate{}, ErrEnrollmentTokenUsed
	}

	certificate, err := signNodeCertificateWithCA(caCert, caKey, caPEM, nodeID, role, request.PublicKey)
	if err != nil {
		return Certificate{}, err
	}
	now := time.Now().UTC()
	labels, err := json.Marshal(current.Labels)
	if err != nil {
		return Certificate{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO nodes(id,role,name,labels_json,enabled,certificate_state,certificate_serial,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, nodeID, role, current.Name, labels, boolInt(current.Enabled), domain.CertificateActive, certificate.Serial, 1, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return Certificate{}, storageFailure("create enrolled node", err)
	}
	if len(current.GatewaySpecJSON) > 0 {
		var spec domain.GatewaySpec
		if err := json.Unmarshal(current.GatewaySpecJSON, &spec); err != nil {
			return Certificate{}, storageFailure("decode pending gateway spec", err)
		}
		spec.NodeID = nodeID
		spec.Revision = 1
		if err := validateGatewaySpecTx(ctx, tx, spec); err != nil {
			return Certificate{}, err
		}
		document, err := json.Marshal(spec)
		if err != nil {
			return Certificate{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_specs(node_id,document_json,revision,updated_at) VALUES(?,?,?,?)`, nodeID, document, 1, now.Format(time.RFC3339Nano)); err != nil {
			return Certificate{}, storageFailure("create enrolled gateway spec", err)
		}
		envelope, err := json.Marshal(domain.NodeSpec{NodeID: nodeID, Kind: domain.NodeSpecGateway, Gateway: &spec, Revision: 1, UpdatedAt: now})
		if err != nil {
			return Certificate{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_specs(node_id,kind,document_json,revision,updated_at) VALUES(?,?,?,?,?)`, nodeID, string(domain.NodeSpecGateway), envelope, 1, now.Format(time.RFC3339Nano)); err != nil {
			return Certificate{}, storageFailure("create enrolled gateway node spec", err)
		}
	} else if len(current.AgentSpecJSON) > 0 {
		var spec domain.AgentSpec
		if err := json.Unmarshal(current.AgentSpecJSON, &spec); err != nil {
			return Certificate{}, storageFailure("decode pending agent spec", err)
		}
		spec.NodeID = nodeID
		spec.Revision = 1
		if err := validateAgentSpecTx(ctx, tx, spec); err != nil {
			return Certificate{}, err
		}
		document, err := json.Marshal(spec)
		if err != nil {
			return Certificate{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_specs(node_id,document_json,revision,updated_at) VALUES(?,?,?,?)`, nodeID, document, 1, now.Format(time.RFC3339Nano)); err != nil {
			return Certificate{}, storageFailure("create enrolled agent spec", err)
		}
		envelope, err := json.Marshal(domain.NodeSpec{NodeID: nodeID, Kind: domain.NodeSpecAgent, Agent: &spec, Revision: 1, UpdatedAt: now})
		if err != nil {
			return Certificate{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_specs(node_id,kind,document_json,revision,updated_at) VALUES(?,?,?,?,?)`, nodeID, string(domain.NodeSpecAgent), envelope, 1, now.Format(time.RFC3339Nano)); err != nil {
			return Certificate{}, storageFailure("create enrolled agent node spec", err)
		}
	}
	if err := insertAudit(ctx, tx, "system", "enroll", "node", nodeID, 1, map[string]string{"serial": certificate.Serial, "role": role}); err != nil {
		return Certificate{}, storageFailure("record pending node enrollment", err)
	}
	if err := s.commitAndNotifyResources(tx, nodeID); err != nil {
		return Certificate{}, storageFailure("commit pending node enrollment", err)
	}
	return certificate, nil
}

func parseEnrollmentCSR(csrDER []byte, nodeID, role string) (*x509.CertificateRequest, error) {
	request, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("%w: parse enrollment CSR: %w", ErrInvalidEnrollmentRequest, err)
	}
	if err := request.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: enrollment CSR signature is invalid: %w", ErrInvalidEnrollmentRequest, err)
	}
	if _, ok := request.PublicKey.(ed25519.PublicKey); !ok {
		return nil, fmt.Errorf("%w: enrollment CSR key must be Ed25519", ErrInvalidEnrollmentRequest)
	}
	if err := validateCSRIdentity(request, nodeID, role); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidEnrollmentRequest, err)
	}
	return request, nil
}
