package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"asterferry/internal/domain"
)

// issuePendingNodeCertificate completes the install-first lifecycle. The
// pending intent, enrollment token and node identity are committed together;
// role configuration remains a separate post-enrollment operation.
func (s *ResourceRepository) issuePendingNodeCertificate(ctx context.Context, config Config, token, nodeID string, csrDER []byte, pending pendingNodeBootstrap) (Certificate, error) {
	if pending.NodeID != nodeID {
		return Certificate{}, ErrEnrollmentNodeMismatch
	}
	if !time.Now().UTC().Before(pending.ExpiresAt) {
		return Certificate{}, ErrEnrollmentTokenExpired
	}
	request, err := parseEnrollmentCSR(csrDER, nodeID)
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

	tx, err := s.beginWriteTx(ctx)
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

	certificate, err := signNodeCertificateWithCA(caCert, caKey, caPEM, nodeID, request.PublicKey)
	if err != nil {
		return Certificate{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO nodes(id,name,enabled,certificate_state,certificate_serial,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, nodeID, current.Name, boolInt(current.Enabled), domain.CertificateActive, certificate.Serial, 1, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return Certificate{}, storageFailure("create enrolled node", err)
	}
	if err := insertNodeLabelsTx(ctx, tx, nodeID, current.Labels); err != nil {
		return Certificate{}, storageFailure("create enrolled node labels", err)
	}
	// Enrollment creates only the Node identity. Role and business behavior
	// are deliberately configured later through the node spec API.
	if err := insertAudit(ctx, tx, "system", "enroll", "node", nodeID, 1, map[string]string{"serial": certificate.Serial}); err != nil {
		return Certificate{}, storageFailure("record pending node enrollment", err)
	}
	if err := s.commitAndNotifyResources(ctx, tx, nodeID); err != nil {
		return Certificate{}, storageFailure("commit pending node enrollment", err)
	}
	return certificate, nil
}

func parseEnrollmentCSR(csrDER []byte, nodeID string) (*x509.CertificateRequest, error) {
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
	if err := validateCSRIdentity(request, nodeID); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidEnrollmentRequest, err)
	}
	return request, nil
}
