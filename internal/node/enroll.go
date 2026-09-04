package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"asterferry/internal/atomicfile"
	controlwire "asterferry/internal/controlwire"
	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
	"asterferry/internal/jsonutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	controllerDialTimeout    = 10 * time.Second
	controllerRequestTimeout = 15 * time.Second
)

type Bootstrap struct {
	SchemaVersion     int    `json:"schema_version"`
	ControllerAddress string `json:"controller_address"`
	// ControllerServerName is the certificate name used for the control-plane
	// TLS endpoint. It is persisted separately from the dial address so a node
	// can enroll through an IP/NAT endpoint while still validating a public DNS
	// certificate on every reconnect.
	ControllerServerName string `json:"controller_server_name,omitempty"`
	NodeID               string `json:"node_id"`
	CertificatePEM       string `json:"certificate_pem"`
	PrivateKeyPEM        string `json:"private_key_pem"`
	CAPEM                string `json:"ca_pem"`
	CachePath            string `json:"cache_path"`
	LogLevel             string `json:"log_level"`
}

type EnrollOptions struct {
	ControllerAddress  string
	Token              string
	NodeID             string
	CAPEM              []byte
	CAPath             string
	ServerName         string
	InsecureSkipVerify bool
	CachePath          string
	OutputPath         string
}

func GenerateCSR(nodeID string) (der []byte, privateKeyPEM []byte, err error) {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return nil, nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err = createCSR(nodeID, private)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, err
	}
	return der, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), nil
}

// GenerateCSRWithPrivateKey is used during certificate rotation. The caller
// keeps the key in the encrypted bootstrap and only replaces the signed
// certificate after the Controller has authenticated the renewal request.
func GenerateCSRWithPrivateKey(nodeID string, privateKeyPEM []byte) ([]byte, error) {
	if err := domain.ValidateID(nodeID, "node_id"); err != nil {
		return nil, err
	}
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, errors.New("private key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	private, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not Ed25519")
	}
	return createCSR(nodeID, private)
}

func createCSR(nodeID string, private ed25519.PrivateKey) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: nodeID, Organization: []string{"AsterFerry"}}}, private)
}

func Enroll(ctx context.Context, options EnrollOptions) (Bootstrap, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(options.ControllerAddress) == "" || strings.TrimSpace(options.Token) == "" {
		return Bootstrap{}, errors.New("controller address and enrollment token are required")
	}
	if err := domain.ValidateID(options.NodeID, "node_id"); err != nil {
		return Bootstrap{}, err
	}
	caPEM := options.CAPEM
	if options.CAPath != "" {
		var err error
		caPEM, err = os.ReadFile(options.CAPath)
		if err != nil {
			return Bootstrap{}, err
		}
	}
	if len(caPEM) == 0 && !options.InsecureSkipVerify {
		return Bootstrap{}, errors.New("controller CA is required unless insecure verification is explicitly enabled")
	}
	csr, privateKey, err := GenerateCSR(options.NodeID)
	if err != nil {
		return Bootstrap{}, err
	}
	serverName := strings.TrimSpace(options.ServerName)
	if serverName == "" && !options.InsecureSkipVerify {
		// A CA pool authenticates the issuer but does not authenticate the
		// endpoint. Derive the host from the bootstrap address when the CLI did
		// not provide an explicit SNI name, so enrollment cannot silently accept
		// a certificate for an unrelated Controller host.
		if host, _, splitErr := net.SplitHostPort(options.ControllerAddress); splitErr == nil {
			serverName = strings.Trim(host, "[]")
		}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: serverName, InsecureSkipVerify: options.InsecureSkipVerify, NextProtos: []string{"h2", controlwire.ControlALPN}} // #nosec G402 -- explicit bootstrap opt-in
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return Bootstrap{}, errors.New("controller CA is invalid")
		}
		tlsConfig.RootCAs = pool
	}
	dialCtx, cancelDial := boundedControllerContext(ctx, controllerDialTimeout)
	conn, err := grpc.DialContext(dialCtx, options.ControllerAddress, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithBlock())
	cancelDial()
	if err != nil {
		return Bootstrap{}, fmt.Errorf("connect Controller: %w", err)
	}
	defer conn.Close()
	client := v1.NewControlClient(conn)
	requestCtx, cancelRequest := boundedControllerContext(ctx, controllerRequestTimeout)
	response, err := client.Enroll(requestCtx, &v1.EnrollRequest{Token: options.Token, NodeId: options.NodeID, CsrDer: csr})
	cancelRequest()
	if err != nil {
		return Bootstrap{}, err
	}
	if response.GetSchemaVersion() != domain.SchemaVersion {
		return Bootstrap{}, errors.New("controller returned an unsupported control schema version")
	}
	if response.GetCertificate() == nil || len(response.GetCertificate().GetCertificateDer()) == 0 {
		return Bootstrap{}, errors.New("controller returned no node certificate")
	}
	if len(caPEM) == 0 {
		if len(response.GetCertificate().GetCaCertificateDer()) == 0 {
			return Bootstrap{}, errors.New("controller returned no CA certificate")
		}
		caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: response.GetCertificate().GetCaCertificateDer()})
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: response.GetCertificate().GetCertificateDer()})
	bootstrap := Bootstrap{SchemaVersion: domain.SchemaVersion, ControllerAddress: options.ControllerAddress, ControllerServerName: serverName, NodeID: options.NodeID, CertificatePEM: string(certificatePEM), PrivateKeyPEM: string(privateKey), CAPEM: string(caPEM), CachePath: options.CachePath, LogLevel: "info"}
	if err := validateBootstrap(bootstrap); err != nil {
		return Bootstrap{}, fmt.Errorf("controller returned invalid bootstrap identity: %w", err)
	}
	if options.OutputPath != "" {
		if err := WriteBootstrap(options.OutputPath, bootstrap); err != nil {
			return Bootstrap{}, err
		}
	}
	return bootstrap, nil
}

func WriteBootstrap(path string, bootstrap Bootstrap) error {
	if bootstrap.SchemaVersion == 0 {
		bootstrap.SchemaVersion = domain.SchemaVersion
	}
	if err := validateBootstrap(bootstrap); err != nil {
		return err
	}
	data, err := json.MarshalIndent(bootstrap, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.AtomicWrite(path, append(data, '\n'), 0o600)
}

func LoadBootstrap(path string) (Bootstrap, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Bootstrap{}, err
	}
	var bootstrap Bootstrap
	if err := jsonutil.DecodeStrict(data, &bootstrap); err != nil {
		if errors.Is(err, jsonutil.ErrTrailingJSON) {
			return Bootstrap{}, errors.New("node bootstrap contains trailing JSON")
		}
		return Bootstrap{}, err
	}
	if err := validateBootstrap(bootstrap); err != nil {
		return Bootstrap{}, err
	}
	if bootstrap.CachePath != "" && !filepath.IsAbs(bootstrap.CachePath) {
		bootstrap.CachePath = filepath.Join(filepath.Dir(filepath.Clean(path)), bootstrap.CachePath)
	}
	return bootstrap, nil
}

func validateBootstrap(bootstrap Bootstrap) error {
	if bootstrap.SchemaVersion != domain.SchemaVersion || strings.TrimSpace(bootstrap.ControllerAddress) == "" || bootstrap.NodeID == "" || bootstrap.CertificatePEM == "" || bootstrap.PrivateKeyPEM == "" || bootstrap.CAPEM == "" {
		return errors.New("invalid node bootstrap")
	}
	if _, _, err := net.SplitHostPort(bootstrap.ControllerAddress); err != nil {
		return errors.New("bootstrap controller address must be host:port")
	}
	if len(bootstrap.ControllerServerName) > 253 || strings.ContainsAny(bootstrap.ControllerServerName, "\x00\r\n") || strings.TrimSpace(bootstrap.ControllerServerName) != bootstrap.ControllerServerName {
		return errors.New("bootstrap controller server name is invalid")
	}
	if err := domain.ValidateID(bootstrap.NodeID, "node_id"); err != nil {
		return err
	}
	certificate, err := tls.X509KeyPair([]byte(bootstrap.CertificatePEM), []byte(bootstrap.PrivateKeyPEM))
	if err != nil || len(certificate.Certificate) == 0 {
		return errors.New("bootstrap certificate and private key do not match")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return errors.New("bootstrap certificate is invalid")
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return errors.New("bootstrap certificate is expired or not yet valid")
	}
	if leaf.Subject.CommonName != bootstrap.NodeID {
		return errors.New("bootstrap certificate identity does not match node")
	}
	if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		return errors.New("bootstrap certificate key must be Ed25519")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(bootstrap.CAPEM)) {
		return errors.New("bootstrap CA is invalid")
	}
	var ca *x509.Certificate
	for rest := []byte(bootstrap.CAPEM); len(rest) > 0; {
		block, remainder := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remainder
		if block.Type == "CERTIFICATE" {
			ca, err = x509.ParseCertificate(block.Bytes)
			if err == nil {
				break
			}
		}
	}
	if ca == nil || !ca.IsCA {
		return errors.New("bootstrap CA is not a CA certificate")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: now}); err != nil {
		return errors.New("bootstrap certificate is not signed by the configured CA")
	}
	return nil
}

func Dial(ctx context.Context, bootstrap Bootstrap) (*grpc.ClientConn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateBootstrap(bootstrap); err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair([]byte(bootstrap.CertificatePEM), []byte(bootstrap.PrivateKeyPEM))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(bootstrap.CAPEM)) {
		return nil, errors.New("bootstrap CA is invalid")
	}
	serverName := strings.TrimSpace(bootstrap.ControllerServerName)
	if serverName == "" {
		serverName = bootstrap.ControllerAddress
		if host, _, splitErr := net.SplitHostPort(serverName); splitErr == nil {
			serverName = host
		}
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: serverName, RootCAs: pool, Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", controlwire.ControlALPN}}
	dialCtx, cancelDial := boundedControllerContext(ctx, controllerDialTimeout)
	conn, err := grpc.DialContext(dialCtx, bootstrap.ControllerAddress, grpc.WithTransportCredentials(credentials.NewTLS(config)), grpc.WithBlock())
	cancelDial()
	return conn, err
}

// boundedControllerContext prevents a command or reconnect attempt from
// waiting forever when the Controller endpoint accepts no connections or
// stops responding. A caller-provided shorter deadline remains authoritative.
func boundedControllerContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= timeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func ControlClient(ctx context.Context, bootstrap Bootstrap) (v1.ControlClient, *grpc.ClientConn, error) {
	conn, err := Dial(ctx, bootstrap)
	if err != nil {
		return nil, nil, err
	}
	return v1.NewControlClient(conn), conn, nil
}
