package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"asterferry/internal/domain"
)

const (
	EnrollmentTTL           = 15 * time.Minute
	NodeCertificateTTL      = 30 * 24 * time.Hour
	CertificateRotateBefore = 7 * 24 * time.Hour
)

type EnrollmentToken struct {
	ID        string     `json:"id"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type Certificate struct {
	CertificatePEM []byte    `json:"certificate_pem"`
	CAPEM          []byte    `json:"ca_pem"`
	Serial         string    `json:"serial"`
	NotBefore      time.Time `json:"not_before"`
	NotAfter       time.Time `json:"not_after"`
}

func (s *Store) CreateEnrollmentToken(ctx context.Context, ttl time.Duration) (string, EnrollmentToken, error) {
	return s.CreateEnrollmentTokenWithOptions(ctx, ttl, WriteOptions{Actor: "system"})
}

func (s *Store) CreateEnrollmentTokenWithOptions(ctx context.Context, ttl time.Duration, options WriteOptions) (string, EnrollmentToken, error) {
	return s.createEnrollmentTokenWithOptions(ctx, "", ttl, options)
}

// CreateNodeEnrollmentToken creates a short-lived enrollment credential that
// is cryptographically bound to one pre-created node. The binding is encoded
// in the one-time plaintext token and is covered by the stored token hash, so
// this feature does not require a schema change to the generic enrollment
// token table.
func (s *Store) CreateNodeEnrollmentToken(ctx context.Context, nodeID string, ttl time.Duration) (string, EnrollmentToken, error) {
	return s.CreateNodeEnrollmentTokenWithOptions(ctx, nodeID, ttl, WriteOptions{Actor: "system"})
}

func (s *Store) CreateNodeEnrollmentTokenWithOptions(ctx context.Context, nodeID string, ttl time.Duration, options WriteOptions) (string, EnrollmentToken, error) {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return "", EnrollmentToken{}, err
	}
	return s.createEnrollmentTokenWithOptions(ctx, nodeID, ttl, options)
}

func (s *Store) createEnrollmentTokenWithOptions(ctx context.Context, nodeID string, ttl time.Duration, options WriteOptions) (string, EnrollmentToken, error) {
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
	tx, err := s.db.BeginTx(ctx, nil)
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
		if err := tx.Commit(); err != nil {
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
	if err := tx.Commit(); err != nil {
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

func (s *Store) ListEnrollmentTokens(ctx context.Context) ([]EnrollmentToken, error) {
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

func (s *Store) IssueNodeCertificate(ctx context.Context, config Config, token, nodeID string, csrDER []byte) (Certificate, error) {
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
func (s *Store) RenewNodeCertificate(ctx context.Context, config Config, nodeID string, csrDER []byte) (Certificate, error) {
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

func signNodeCertificateWithCA(caCert *x509.Certificate, caKey ed25519.PrivateKey, caPEM []byte, nodeID string, publicKey any) (Certificate, error) {
	if caCert == nil || len(caKey) == 0 || len(caPEM) == 0 {
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
