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
	Dir            string
	HTTPListen     string
	GRPCListen     string
	GRPCAdvertise  string
	ReleaseBaseURL string
	ReleaseVersion string
	Username       string
	Password       string
	Force          bool
	Now            func() time.Time
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
	if strings.TrimSpace(options.GRPCAdvertise) == "" {
		return InitResult{}, errors.New("controller grpc_advertise is required during initialization; pass a reachable host:port")
	}
	config := DefaultConfig(dir)
	if options.HTTPListen != "" {
		config.HTTPListen = options.HTTPListen
	}
	if options.GRPCListen != "" {
		config.GRPCListen = options.GRPCListen
	}
	config.GRPCAdvertise = strings.TrimSpace(options.GRPCAdvertise)
	if options.ReleaseBaseURL != "" {
		config.ReleaseBaseURL = options.ReleaseBaseURL
	}
	if options.ReleaseVersion != "" {
		config.ReleaseVersion = options.ReleaseVersion
	}
	if err := config.Validate(); err != nil {
		return InitResult{}, err
	}
	targetExists := false
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return InitResult{}, fmt.Errorf("controller path %q is not a directory", dir)
		}
		targetExists = true
		if !options.Force {
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				return InitResult{}, readErr
			}
			if len(entries) > 0 {
				return InitResult{}, fmt.Errorf("controller directory %q is not empty; use force to replace it", dir)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return InitResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return InitResult{}, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".init-*")
	if err != nil {
		return InitResult{}, fmt.Errorf("create initialization staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	stagedConfig := DefaultConfig(staging)
	stagedConfig.HTTPListen = config.HTTPListen
	stagedConfig.GRPCListen = config.GRPCListen
	stagedConfig.GRPCAdvertise = config.GRPCAdvertise
	stagedConfig.ReleaseBaseURL = config.ReleaseBaseURL
	stagedConfig.ReleaseVersion = config.ReleaseVersion
	stagedConfig.DashboardEnable = config.DashboardEnable
	stagedConfig.LogLevel = config.LogLevel
	for _, path := range []string{staging, filepath.Dir(stagedConfig.CAKeyPath), filepath.Dir(stagedConfig.CACertPath), filepath.Dir(stagedConfig.TLSKeyPath), filepath.Dir(stagedConfig.TLSCertPath)} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return InitResult{}, err
		}
	}
	if err := writeCA(stagedConfig.CAKeyPath, stagedConfig.CACertPath, options.Now); err != nil {
		return InitResult{}, err
	}
	if err := writeServerCertificate(stagedConfig, options.Now); err != nil {
		return InitResult{}, err
	}
	masterKey, err := LoadOrCreateMasterKey(stagedConfig.MasterKeyPath)
	if err != nil {
		return InitResult{}, err
	}
	store, err := OpenStore(stagedConfig.DatabasePath, masterKey)
	if err != nil {
		return InitResult{}, err
	}
	defer store.Close()
	admin, err := store.CreateUser(ctx, options.Username, options.Password, RoleAdmin)
	if err != nil {
		return InitResult{}, err
	}
	// Windows keeps SQLite's database handle open across directory renames.
	// Close the staging store before publishing the containing directory; the
	// deferred close still covers every earlier error path.
	if err := store.Close(); err != nil {
		return InitResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return InitResult{}, err
	}
	// The generated config points at the eventual target directory, not the
	// staging directory. It becomes correct as soon as the directory rename
	// completes, while every referenced file has already been fsynced in the
	// staging tree.
	stagedConfigPath := filepath.Join(staging, "controller.json")
	if err := SaveConfig(stagedConfigPath, config); err != nil {
		return InitResult{}, err
	}
	if err := publishInitializedDirectory(staging, dir, targetExists, options.Force); err != nil {
		return InitResult{}, err
	}
	published = true
	configPath := filepath.Join(dir, "controller.json")
	config.SourcePath = configPath
	return InitResult{ConfigPath: configPath, Config: config, Admin: admin}, nil
}

type initMovedEntry struct {
	from string
	to   string
}

// publishInitializedDirectory makes initialization all-or-nothing for a new
// directory. Force mode keeps unrelated files from the old directory and
// leaves the replaced tree in a same-parent rollback backup.
func publishInitializedDirectory(staging, target string, targetExists, force bool) error {
	if !targetExists {
		if err := os.Rename(staging, target); err != nil {
			return fmt.Errorf("publish initialized Controller directory: %w", err)
		}
		return nil
	}
	if !force {
		entries, err := os.ReadDir(target)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("controller directory %q became non-empty during initialization", target)
		}
		// Renaming the empty directory away is more reliable on Windows than
		// removing it and immediately renaming another directory into the same
		// pathname. It also gives the publish step an explicit rollback target.
		backup, err := reserveInitDirectory(target, "."+filepath.Base(target)+".pre-init-*")
		if err != nil {
			return fmt.Errorf("reserve empty Controller rollback directory: %w", err)
		}
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("move empty Controller directory: %w", err)
		}
		if err := os.Rename(staging, target); err != nil {
			_ = os.Rename(backup, target)
			return fmt.Errorf("publish initialized Controller directory: %w", err)
		}
		_ = os.Remove(backup)
		return nil
	}

	backup, err := reserveInitDirectory(target, "."+filepath.Base(target)+".pre-init-*")
	if err != nil {
		return fmt.Errorf("reserve initialization rollback directory: %w", err)
	}
	if err := os.Rename(target, backup); err != nil {
		return fmt.Errorf("move previous Controller directory: %w", err)
	}
	moved, err := preserveInitExtras(backup, staging)
	if err != nil {
		rollbackInitDirectory(backup, target, moved)
		return fmt.Errorf("preserve unrelated Controller files: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		rollbackInitDirectory(backup, target, moved)
		return fmt.Errorf("publish initialized Controller directory: %w", err)
	}
	return nil
}

func reserveInitDirectory(path, pattern string) (string, error) {
	name, err := os.MkdirTemp(filepath.Dir(path), pattern)
	if err != nil {
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func preserveInitExtras(previous, staging string) ([]initMovedEntry, error) {
	moved := []initMovedEntry{}
	knownTop := map[string]struct{}{
		"controller.db": {}, "controller.db-wal": {}, "controller.db-shm": {}, "controller.db-journal": {},
		"controller.json": {}, "master.key": {}, "ca": {}, "tls": {},
	}
	entries, err := os.ReadDir(previous)
	if err != nil {
		return nil, err
	}
	move := func(from, to string) error {
		if err := os.Rename(from, to); err != nil {
			return err
		}
		moved = append(moved, initMovedEntry{from: from, to: to})
		return nil
	}
	for _, entry := range entries {
		if _, known := knownTop[entry.Name()]; known {
			continue
		}
		if err := move(filepath.Join(previous, entry.Name()), filepath.Join(staging, entry.Name())); err != nil {
			return moved, err
		}
	}
	for _, item := range []struct {
		directory string
		known     map[string]struct{}
	}{
		{directory: "ca", known: map[string]struct{}{"ca.key": {}, "ca.crt": {}}},
		{directory: "tls", known: map[string]struct{}{"controller.key": {}, "controller.crt": {}}},
	} {
		fromDir := filepath.Join(previous, item.directory)
		entries, readErr := os.ReadDir(fromDir)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return moved, readErr
		}
		for _, entry := range entries {
			if _, known := item.known[entry.Name()]; known {
				continue
			}
			if err := move(filepath.Join(fromDir, entry.Name()), filepath.Join(staging, item.directory, entry.Name())); err != nil {
				return moved, err
			}
		}
	}
	return moved, nil
}

func rollbackInitDirectory(previous, target string, moved []initMovedEntry) {
	for index := len(moved) - 1; index >= 0; index-- {
		_ = os.Rename(moved[index].to, moved[index].from)
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		_ = os.Rename(previous, target)
	}
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
	certPEM, keyPEM, err := serverCertificatePEM(config, nowFn)
	if err != nil {
		return err
	}
	if err := writeSecure(config.TLSKeyPath, keyPEM); err != nil {
		return err
	}
	return writeSecure(config.TLSCertPath, certPEM)
}

func serverCertificatePEM(config Config, nowFn func() time.Time) ([]byte, []byte, error) {
	if nowFn == nil {
		nowFn = time.Now
	}
	caCert, caKey, err := readCA(config.CACertPath, config.CAKeyPath)
	if err != nil {
		return nil, nil, err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := nowFn().UTC()
	dnsNames := []string{"localhost"}
	ipAddresses := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	// Nodes dial GRPCAdvertise, which may be a public DNS name or address
	// different from the local bind address. Include it in the certificate SAN
	// at initialization time or generated one-line enrollment commands would
	// fail TLS verification on a correctly configured reverse/NAT deployment.
	for _, listen := range []string{config.HTTPListen, config.GRPCListen, config.GRPCAdvertise} {
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
		return nil, nil, err
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), nil
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
