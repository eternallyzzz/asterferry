package controller

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"asterferry/internal/atomicfile"
)

// ConfigureOptions describes an in-place Controller configuration update.
// The update intentionally keeps the existing CA, database and master key;
// only the advertised address and the Controller server certificate change.
type ConfigureOptions struct {
	ConfigPath     string
	GRPCAdvertise  string
	ReleaseBaseURL string
	ReleaseVersion string
	Now            func() time.Time
}

// Configure updates the Controller's advertised gRPC address and reissues its
// server certificate with a matching SAN. It is intended to run while the
// Controller is stopped. The three files are published individually with
// rollback on failure so an error does not leave a partially updated setup.
func Configure(ctx context.Context, options ConfigureOptions) (Config, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := filepath.Clean(strings.TrimSpace(options.ConfigPath))
	if path == "." || path == "" {
		return Config{}, errors.New("controller config path is required")
	}
	config, err := LoadConfig(path)
	if err != nil {
		return Config{}, err
	}
	advertise := strings.TrimSpace(options.GRPCAdvertise)
	if err := validateAdvertisedAddress(advertise, "grpc_advertise"); err != nil {
		return Config{}, err
	}
	config.GRPCAdvertise = advertise
	if strings.TrimSpace(options.ReleaseBaseURL) != "" {
		config.ReleaseBaseURL = strings.TrimSpace(options.ReleaseBaseURL)
	}
	if strings.TrimSpace(options.ReleaseVersion) != "" {
		config.ReleaseVersion = strings.TrimSpace(options.ReleaseVersion)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	if err := ctx.Err(); err != nil {
		return Config{}, err
	}

	certPEM, keyPEM, err := serverCertificatePEM(config, options.Now)
	if err != nil {
		return Config{}, fmt.Errorf("generate Controller server certificate: %w", err)
	}
	configPEM, err := marshalConfig(config)
	if err != nil {
		return Config{}, fmt.Errorf("encode Controller config: %w", err)
	}
	oldConfig, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read Controller config for rollback: %w", err)
	}
	oldKey, err := os.ReadFile(config.TLSKeyPath)
	if err != nil {
		return Config{}, fmt.Errorf("read Controller TLS key for rollback: %w", err)
	}
	oldCert, err := os.ReadFile(config.TLSCertPath)
	if err != nil {
		return Config{}, fmt.Errorf("read Controller TLS certificate for rollback: %w", err)
	}

	updates := []configFileUpdate{
		{path: config.TLSKeyPath, data: keyPEM, old: oldKey},
		{path: config.TLSCertPath, data: certPEM, old: oldCert},
		{path: path, data: configPEM, old: oldConfig},
	}
	for index, update := range updates {
		if err := ctx.Err(); err != nil {
			return Config{}, rollbackConfigFiles(updates[:index], err)
		}
		if err := atomicfile.AtomicWrite(update.path, update.data, 0o600); err != nil {
			rollbackErr := rollbackConfigFiles(updates[:index], err)
			return Config{}, rollbackErr
		}
	}
	if err := ctx.Err(); err != nil {
		return Config{}, rollbackConfigFiles(updates, err)
	}
	config.SourcePath = path
	return config, nil
}

type configFileUpdate struct {
	path string
	data []byte
	old  []byte
}

func rollbackConfigFiles(updates []configFileUpdate, cause error) error {
	var rollbackErr error
	for index := len(updates) - 1; index >= 0; index-- {
		if err := atomicfile.AtomicWrite(updates[index].path, updates[index].old, 0o600); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore %s: %w", updates[index].path, err))
		}
	}
	return errors.Join(cause, rollbackErr)
}

// validateControllerCertificate catches the common failure mode where an
// operator edits grpc_advertise in JSON but keeps a certificate generated for
// the previous address. The Controller cannot prove that a remote Node can
// reach an address, but it can guarantee that its own certificate matches it.
func validateControllerCertificate(config Config) error {
	if strings.TrimSpace(config.GRPCAdvertise) == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(config.GRPCAdvertise)
	if err != nil {
		return fmt.Errorf("validate grpc_advertise: %w", err)
	}
	certPEM, err := os.ReadFile(config.TLSCertPath)
	if err != nil {
		return fmt.Errorf("read Controller TLS certificate: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return errors.New("Controller TLS certificate is not PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse Controller TLS certificate: %w", err)
	}
	host = strings.Trim(host, "[]")
	if err := certificate.VerifyHostname(host); err != nil {
		return fmt.Errorf("Controller TLS certificate does not cover grpc_advertise %q; run controller configure to reissue it", config.GRPCAdvertise)
	}
	return nil
}
