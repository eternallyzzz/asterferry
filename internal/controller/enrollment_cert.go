package controller

import (
	"asterferry/internal/domain"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"
)

func (s *Repository) IssueNodeCertificate(ctx context.Context, config Config, token, nodeID string, csrDER []byte) (Certificate, error) {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return Certificate{}, fmt.Errorf("%w: %w", ErrInvalidEnrollmentRequest, err)
	}
	if boundNodeID, bound, err := parseNodeEnrollmentToken(token); err != nil {
		return Certificate{}, fmt.Errorf("%w: %w", ErrInvalidEnrollmentRequest, err)
	} else if bound && boundNodeID != nodeID {
		return Certificate{}, ErrEnrollmentNodeMismatch
	}
	// A bootstrap token may refer to an installation intent whose node row does
	// not exist yet. Complete that intent atomically during enrollment; legacy
	// administrator-created tokens continue through the pre-created-node path
	// below.
	pending, pendingErr := s.pendingBootstrapForToken(ctx, HashToken(token))
	if pendingErr == nil {
		return s.issuePendingNodeCertificate(ctx, config, token, nodeID, csrDER, pending)
	}
	if !errors.Is(pendingErr, sql.ErrNoRows) {
		return Certificate{}, storageFailure("look up pending node bootstrap", pendingErr)
	}
	node, err := s.GetNode(ctx, nodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Certificate{}, ErrNodeNotEnrolled
		}
		return Certificate{}, storageFailure("load node for enrollment", err)
	}
	if !node.Enabled || node.CertificateState == domain.CertificateRevoked {
		return Certificate{}, fmt.Errorf("%w: node is disabled", ErrNodeEnrollmentNotAllowed)
	}
	if err := s.validateEnrollmentToken(ctx, token); err != nil {
		return Certificate{}, err
	}
	request, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return Certificate{}, fmt.Errorf("%w: parse enrollment CSR: %w", ErrInvalidEnrollmentRequest, err)
	}
	if err := request.CheckSignature(); err != nil {
		return Certificate{}, fmt.Errorf("%w: enrollment CSR signature is invalid: %w", ErrInvalidEnrollmentRequest, err)
	}
	if _, ok := request.PublicKey.(ed25519.PublicKey); !ok {
		return Certificate{}, fmt.Errorf("%w: enrollment CSR key must be Ed25519", ErrInvalidEnrollmentRequest)
	}
	if err := validateCSRIdentity(request, nodeID); err != nil {
		return Certificate{}, fmt.Errorf("%w: %w", ErrInvalidEnrollmentRequest, err)
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
		return Certificate{}, storageFailure("begin enrollment transaction", err)
	}
	defer tx.Rollback()
	var revision int64
	var certificateState string
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT revision,enabled,certificate_state FROM nodes WHERE id=?`, nodeID).Scan(&revision, &enabled, &certificateState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Certificate{}, ErrNodeNotEnrolled
		}
		return Certificate{}, storageFailure("reload node for enrollment", err)
	}
	// Re-check mutable enrollment preconditions inside the write transaction.
	// The initial GetNode call is only a fast rejection path; an administrator
	// can revoke/disable a node while CSR parsing and signing are in progress.
	if enabled == 0 || certificateState == domain.CertificateRevoked {
		return Certificate{}, fmt.Errorf("%w: node is disabled", ErrNodeEnrollmentNotAllowed)
	}
	if err := validateCSRIdentity(request, nodeID); err != nil {
		return Certificate{}, fmt.Errorf("%w: %w", ErrInvalidEnrollmentRequest, err)
	}
	// Consume the one-time token before performing the CA signature while the
	// transaction is still open. Any signing or persistence failure rolls the
	// transaction back, so a valid token is not lost to an internal error.
	if err := consumeEnrollmentTokenTx(ctx, tx, HashToken(token)); err != nil {
		if !isCredentialError(err) && !errors.Is(err, ErrInvalidEnrollmentRequest) {
			return Certificate{}, storageFailure("consume enrollment token", err)
		}
		return Certificate{}, err
	}
	certificate, err := signNodeCertificateWithCA(caCert, caKey, caPEM, nodeID, request.PublicKey)
	if err != nil {
		return Certificate{}, err
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE nodes SET certificate_state=?,certificate_serial=?,revision=?,updated_at=? WHERE id=? AND revision=? AND enabled=1 AND certificate_state<>?`, domain.CertificateActive, certificate.Serial, revision+1, updatedAt, nodeID, revision, domain.CertificateRevoked)
	if err != nil {
		return Certificate{}, storageFailure("update node certificate", err)
	}
	affected, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return Certificate{}, storageFailure("certificate issuance rows affected", affectedErr)
	}
	if affected != 1 {
		return Certificate{}, fmt.Errorf("%w: node enrollment state changed during certificate issuance", ErrNodeEnrollmentNotAllowed)
	}
	if err := insertAudit(ctx, tx, "system", "enroll", "node_certificate", nodeID, revision+1, map[string]string{"serial": certificate.Serial}); err != nil {
		return Certificate{}, storageFailure("record enrollment audit", err)
	}
	if err := tx.Commit(); err != nil {
		return Certificate{}, storageFailure("commit enrollment", err)
	}
	return certificate, nil
}

// RenewNodeCertificate issues a fresh certificate for an already enrolled
// node. The gRPC control stream authenticates the caller with its current
// mTLS certificate before invoking this method, so no enrollment token is
// needed for the rotation path.
func (s *Repository) RenewNodeCertificate(ctx context.Context, config Config, nodeID string, csrDER []byte) (Certificate, error) {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return Certificate{}, fmt.Errorf("%w: %w", ErrInvalidEnrollmentRequest, err)
	}
	node, err := s.GetNode(ctx, nodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Certificate{}, ErrNodeNotEnrolled
		}
		return Certificate{}, storageFailure("load node for renewal", err)
	}
	if !node.Enabled || node.CertificateState == domain.CertificateRevoked {
		return Certificate{}, fmt.Errorf("%w: node is disabled or certificate is revoked", ErrNodeEnrollmentNotAllowed)
	}
	request, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return Certificate{}, fmt.Errorf("%w: parse renewal CSR: %w", ErrInvalidEnrollmentRequest, err)
	}
	if err := request.CheckSignature(); err != nil {
		return Certificate{}, fmt.Errorf("%w: renewal CSR signature is invalid: %w", ErrInvalidEnrollmentRequest, err)
	}
	if _, ok := request.PublicKey.(ed25519.PublicKey); !ok {
		return Certificate{}, fmt.Errorf("%w: renewal CSR key must be Ed25519", ErrInvalidEnrollmentRequest)
	}
	if err := validateCSRIdentity(request, node.ID); err != nil {
		return Certificate{}, fmt.Errorf("%w: %w", ErrInvalidEnrollmentRequest, err)
	}
	certificate, err := signNodeCertificate(config, node.ID, request.PublicKey)
	if err != nil {
		return Certificate{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Certificate{}, storageFailure("begin renewal transaction", err)
	}
	defer tx.Rollback()
	var revision int64
	var certificateState string
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT revision,enabled,certificate_state FROM nodes WHERE id=?`, nodeID).Scan(&revision, &enabled, &certificateState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Certificate{}, ErrNodeNotEnrolled
		}
		return Certificate{}, storageFailure("reload node for renewal", err)
	}
	if enabled == 0 || certificateState == domain.CertificateRevoked {
		return Certificate{}, fmt.Errorf("%w: node is disabled or certificate is revoked", ErrNodeEnrollmentNotAllowed)
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE nodes SET certificate_state=?,certificate_serial=?,revision=?,updated_at=? WHERE id=? AND revision=? AND enabled=1 AND certificate_state<>?`, domain.CertificateActive, certificate.Serial, revision+1, updatedAt, nodeID, revision, domain.CertificateRevoked)
	if err != nil {
		return Certificate{}, storageFailure("update renewed node certificate", err)
	}
	affected, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return Certificate{}, storageFailure("certificate renewal rows affected", affectedErr)
	}
	if affected != 1 {
		return Certificate{}, fmt.Errorf("%w: node state changed during certificate renewal", ErrNodeEnrollmentNotAllowed)
	}
	if err := insertAudit(ctx, tx, "system", "renew", "node_certificate", nodeID, revision+1, map[string]string{"serial": certificate.Serial}); err != nil {
		return Certificate{}, storageFailure("record renewal audit", err)
	}
	if err := tx.Commit(); err != nil {
		return Certificate{}, storageFailure("commit renewal", err)
	}
	return certificate, nil
}

func validateCSRIdentity(request *x509.CertificateRequest, nodeID string) error {
	if request == nil || request.Subject.CommonName != nodeID {
		return errors.New("CSR common name does not match node identity")
	}
	return nil
}

func signNodeCertificate(config Config, nodeID string, publicKey any) (Certificate, error) {
	caCert, caKey, err := readCA(config.CACertPath, config.CAKeyPath)
	if err != nil {
		return Certificate{}, err
	}
	caPEM, err := os.ReadFile(config.CACertPath)
	if err != nil {
		return Certificate{}, err
	}
	return signNodeCertificateWithCA(caCert, caKey, caPEM, nodeID, publicKey)
}

func signNodeCertificateWithCA(caCert *x509.Certificate, caKey crypto.Signer, caPEM []byte, nodeID string, publicKey any) (Certificate, error) {
	if caCert == nil || caKey == nil || len(caPEM) == 0 {
		return Certificate{}, errors.New("CA signing material is incomplete")
	}
	serial, err := randomSerial()
	if err != nil {
		return Certificate{}, err
	}
	now := time.Now().UTC()
	uri := domain.NodeIdentityURI(nodeID)
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: nodeID, Organization: []string{"AsterFerry"}}, URIs: []*url.URL{uri}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(NodeCertificateTTL), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, publicKey, caKey)
	if err != nil {
		return Certificate{}, err
	}
	serialText := serial.Text(16)
	return Certificate{CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), CAPEM: caPEM, Serial: serialText, NotBefore: template.NotBefore, NotAfter: template.NotAfter}, nil
}

func CertificateNeedsRotation(notAfter, now time.Time) bool {
	if notAfter.IsZero() {
		return true
	}
	if now.IsZero() {
		now = time.Now()
	}
	return !notAfter.After(now.Add(CertificateRotateBefore))
}

func GenerateNodeCSR(nodeID string) (csrDER, keyPEM []byte, err error) {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return nil, nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	request, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: nodeID, Organization: []string{"AsterFerry"}}}, private)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, err
	}
	return request, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), nil
}
