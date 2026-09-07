package node

import (
	"asterferry/internal/atomicfile"
	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func renewalRequest(bootstrap Bootstrap) ([]byte, error) {
	certificate, err := tls.X509KeyPair([]byte(bootstrap.CertificatePEM), []byte(bootstrap.PrivateKeyPEM))
	if err != nil {
		return nil, err
	}
	if len(certificate.Certificate) == 0 {
		return nil, errors.New("node certificate is empty")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, err
	}
	if leaf.NotAfter.After(time.Now().UTC().Add(7 * 24 * time.Hour)) {
		return nil, nil
	}
	return GenerateCSRWithPrivateKey(bootstrap.NodeID, []byte(bootstrap.PrivateKeyPEM))
}

func (r *Runtime) acceptCertificate(bundle *v1.CertificateBundle) error {
	if bundle == nil || len(bundle.CertificateDer) == 0 {
		return errors.New("controller returned an empty certificate bundle")
	}
	leaf, err := x509.ParseCertificate(bundle.CertificateDer)
	if err != nil {
		return fmt.Errorf("controller returned an invalid certificate: %w", err)
	}
	if strings.TrimSpace(bundle.Serial) == "" || !strings.EqualFold(leaf.SerialNumber.Text(16), strings.TrimSpace(bundle.Serial)) {
		return errors.New("controller certificate serial does not match the bundle")
	}
	now := time.Now().UTC()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return errors.New("controller certificate is expired or not yet valid")
	}
	bootstrap := r.bootstrapSnapshot()
	if leaf.Subject.CommonName != bootstrap.NodeID {
		return errors.New("controller certificate identity does not match the node")
	}
	keyPair, err := tls.X509KeyPair([]byte(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.CertificateDer})), []byte(bootstrap.PrivateKeyPEM))
	if err != nil || len(keyPair.Certificate) == 0 {
		return errors.New("controller certificate does not match the node private key")
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.CertificateDer})
	caPEM := []byte(bootstrap.CAPEM)
	if len(bundle.CaCertificateDer) > 0 {
		caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.CaCertificateDer})
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("controller certificate bundle has no valid CA")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: x509.NewCertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: now}); err != nil {
		return fmt.Errorf("controller certificate is not signed by the configured CA: %w", err)
	}
	updated := bootstrap
	updated.CertificatePEM = string(certificatePEM)
	if len(caPEM) > 0 {
		updated.CAPEM = string(caPEM)
	}
	if r.bootstrapPath != "" {
		if err := WriteBootstrap(r.bootstrapPath, updated); err != nil {
			return err
		}
	}
	dataPlane := r.DataPlane()
	if dataPlane != nil {
		if err := dataPlane.UpdateBootstrap(updated); err != nil {
			var rollbackErrs []error
			if r.bootstrapPath != "" {
				if rollbackErr := WriteBootstrap(r.bootstrapPath, bootstrap); rollbackErr != nil {
					r.logger.Error("node bootstrap rollback failed", "error", rollbackErr)
					rollbackErrs = append(rollbackErrs, rollbackErr)
				}
			}
			if rollbackErr := dataPlane.UpdateBootstrap(bootstrap); rollbackErr != nil {
				r.logger.Error("data-plane bootstrap rollback failed", "error", rollbackErr)
				rollbackErrs = append(rollbackErrs, rollbackErr)
			}
			return errors.Join(err, errors.Join(rollbackErrs...))
		}
	}
	r.bootstrapMu.Lock()
	r.bootstrap = updated
	r.bootstrapMu.Unlock()
	return nil
}

func (r *Runtime) bootstrapSnapshot() Bootstrap {
	r.bootstrapMu.RLock()
	defer r.bootstrapMu.RUnlock()
	return r.bootstrap
}

func applyError(err error) *domain.ApplyError {
	var value *domain.ApplyError
	if errors.As(err, &value) {
		return value
	}
	return &domain.ApplyError{Code: "invalid_snapshot", Message: err.Error()}
}

func loadOrCreateNodeKey(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		if len(data) != 32 {
			return nil, errors.New("node cache key must contain 32 bytes")
		}
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	tmpPath, err := atomicfile.WriteTemp(path, ".node-key-*", key, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmpPath) }()
	if err := os.Rename(tmpPath, path); err != nil {
		if existing, readErr := os.ReadFile(path); readErr == nil && len(existing) == 32 {
			return existing, nil
		}
		return nil, fmt.Errorf("publish node cache key: %w", err)
	}
	return key, nil
}
