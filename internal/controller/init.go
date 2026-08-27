package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"asterferry/internal/atomicfile"
)

type InitOptions struct {
	Dir        string
	HTTPListen string
	GRPCListen string
	Username   string
	Password   string
	Force      bool
	Now        func() time.Time
}

type InitResult struct {
	ConfigPath string
	Config     Config
	Admin      User
}

func Init(ctx context.Context, options InitOptions) (InitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dir := filepath.Clean(strings.TrimSpace(options.Dir))
	if dir == "." || dir == "" {
		return InitResult{}, errors.New("controller directory is required")
	}
	var absErr error
	dir, absErr = filepath.Abs(dir)
	if absErr != nil {
		return InitResult{}, fmt.Errorf("resolve controller directory: %w", absErr)
	}
	if options.Username == "" {
		options.Username = "admin"
	}
	if options.Password == "" {
		return InitResult{}, errors.New("initial admin password is required")
	}
	config := DefaultConfig(dir)
	if options.HTTPListen != "" {
		config.HTTPListen = options.HTTPListen
	}
	if options.GRPCListen != "" {
		config.GRPCListen = options.GRPCListen
	}
	if err := config.Validate(); err != nil {
		return InitResult{}, err
	}
	if _, err := os.Stat(dir); err == nil && !options.Force {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return InitResult{}, readErr
		}
		if len(entries) > 0 {
			return InitResult{}, fmt.Errorf("controller directory %q is not empty; use force to replace it", dir)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return InitResult{}, err
	}
	for _, path := range []string{dir, filepath.Dir(config.CAKeyPath), filepath.Dir(config.CACertPath), filepath.Dir(config.TLSKeyPath), filepath.Dir(config.TLSCertPath)} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return InitResult{}, err
		}
	}
	if options.Force {
		// --force is an explicit destructive re-initialization. Remove only the
		// known Controller SQLite files; unrelated files in the directory are
		// left untouched.
		for _, path := range []string{config.DatabasePath, config.DatabasePath + "-wal", config.DatabasePath + "-shm"} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return InitResult{}, fmt.Errorf("remove existing controller database %q: %w", path, err)
			}
		}
	}
	if err := writeCA(config.CAKeyPath, config.CACertPath, options.Now); err != nil {
		return InitResult{}, err
	}
	if err := writeServerCertificate(config, options.Now); err != nil {
		return InitResult{}, err
	}
	if _, err := LoadOrCreateMasterKey(config.MasterKeyPath); err != nil {
		return InitResult{}, err
	}
	store, err := OpenStore(config.DatabasePath)
	if err != nil {
		return InitResult{}, err
	}
	defer store.Close()
	admin, err := store.CreateUser(ctx, options.Username, options.Password, RoleAdmin)
	if err != nil {
		return InitResult{}, err
	}
	configPath := filepath.Join(dir, "controller.json")
	if err := SaveConfig(configPath, config); err != nil {
		return InitResult{}, err
	}
	config.SourcePath = configPath
	return InitResult{ConfigPath: configPath, Config: config, Admin: admin}, nil
}

func writeCA(keyPath, certPath string, nowFn func() time.Time) error {
	if nowFn == nil {
		nowFn = time.Now
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	now := nowFn().UTC()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "AsterFerry Controller CA", Organization: []string{"AsterFerry"}}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature, MaxPathLen: 1}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return err
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	if err := writeSecure(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})); err != nil {
		return err
	}
	return writeSecure(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func writeServerCertificate(config Config, nowFn func() time.Time) error {
	if nowFn == nil {
		nowFn = time.Now
	}
	caCert, caKey, err := readCA(config.CACertPath, config.CAKeyPath)
	if err != nil {
		return err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	now := nowFn().UTC()
	dnsNames := []string{"localhost"}
	ipAddresses := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, listen := range []string{config.HTTPListen, config.GRPCListen} {
		host, _, splitErr := net.SplitHostPort(listen)
		if splitErr != nil || host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
			continue
		}
		if parsed := net.ParseIP(host); parsed != nil {
			ipAddresses = append(ipAddresses, parsed)
		} else if !containsString(dnsNames, host) {
			dnsNames = append(dnsNames, host)
		}
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "asterferry-controller"}, DNSNames: dnsNames, IPAddresses: ipAddresses, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(2, 0, 0), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, public, caKey)
	if err != nil {
		return err
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	if err := writeSecure(config.TLSKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})); err != nil {
		return err
	}
	return writeSecure(config.TLSCertPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func readCA(certPath, keyPath string) (*x509.Certificate, ed25519.PrivateKey, error) {
	certBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(certBytes)
	if block == nil {
		return nil, nil, errors.New("CA certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyBytes)
	if keyBlock == nil {
		return nil, nil, errors.New("CA key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	private, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, errors.New("CA key is not Ed25519")
	}
	return cert, private, nil
}

func writeSecure(path string, data []byte) error {
	return atomicfile.AtomicWrite(path, data, 0o600)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	if serial.Sign() == 0 {
		return big.NewInt(1), nil
	}
	return serial, nil
}
