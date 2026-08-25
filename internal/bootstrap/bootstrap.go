// Package bootstrap creates the self-contained configuration bundles used by
// the CLI init command. It deliberately uses the Go standard library for all
// key and certificate generation so a fresh installation does not depend on
// OpenSSL.
package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/protocol"
	"gopkg.in/yaml.v3"
)

type Profile string

const (
	ProfileDev  Profile = "dev"
	ProfileProd Profile = "prod"
)

type Options struct {
	Dir         string
	Profile     Profile
	AgentID     string
	GatewayHost string
	GatewayPort int
	Force       bool
}

type Result struct {
	Dir           string
	GatewayConfig string
	AgentConfig   string
	Profile       Profile
}

type manifest struct {
	Version int      `json:"version"`
	Files   []string `json:"files"`
}

const (
	manifestName    = ".asterferry-init.json"
	manifestVersion = 1
)

func Generate(opts Options) (Result, error) {
	opts.Profile = Profile(strings.ToLower(strings.TrimSpace(string(opts.Profile))))
	opts.Dir = strings.TrimSpace(opts.Dir)
	opts.AgentID = strings.TrimSpace(opts.AgentID)
	opts.GatewayHost = strings.TrimSpace(opts.GatewayHost)
	if opts.Profile != ProfileDev && opts.Profile != ProfileProd {
		return Result{}, fmt.Errorf("profile must be %q or %q", ProfileDev, ProfileProd)
	}
	if strings.TrimSpace(opts.Dir) == "" {
		opts.Dir = "asterferry"
	}
	if opts.AgentID == "" {
		opts.AgentID = "edge-a"
	}
	if opts.GatewayHost == "" {
		opts.GatewayHost = "127.0.0.1"
	}
	if opts.GatewayPort == 0 {
		opts.GatewayPort = 4433
	}
	if opts.GatewayPort < 1 || opts.GatewayPort > 65535 {
		return Result{}, errors.New("gateway port must be between 1 and 65535")
	}
	if err := validateIdentifier(opts.AgentID); err != nil {
		return Result{}, fmt.Errorf("agent id: %w", err)
	}
	if err := validateHost(opts.GatewayHost); err != nil {
		return Result{}, fmt.Errorf("gateway host: %w", err)
	}

	target, err := filepath.Abs(filepath.Clean(opts.Dir))
	if err != nil {
		return Result{}, fmt.Errorf("resolve output directory: %w", err)
	}
	if err := prepareTarget(target, opts.Force); err != nil {
		return Result{}, err
	}

	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Result{}, fmt.Errorf("create output parent: %w", err)
	}
	temp, err := os.MkdirTemp(parent, ".asterferry-init-")
	if err != nil {
		return Result{}, fmt.Errorf("create temporary output: %w", err)
	}
	defer os.RemoveAll(temp)

	result, files, err := generateBundle(temp, opts)
	if err != nil {
		return Result{}, err
	}
	if err := writeJSON(filepath.Join(temp, manifestName), manifest{Version: manifestVersion, Files: files}, 0o600); err != nil {
		return Result{}, fmt.Errorf("write init manifest: %w", err)
	}
	files = append(files, manifestName)
	if _, err := os.Stat(target); err == nil {
		if !opts.Force {
			return Result{}, fmt.Errorf("output directory %q already exists; choose another directory or use --force for an existing init bundle", target)
		}
		if err := copyGeneratedFiles(temp, target, files); err != nil {
			return Result{}, err
		}
	} else if !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("inspect output directory: %w", err)
	} else if err := os.Rename(temp, target); err != nil {
		return Result{}, fmt.Errorf("publish init bundle: %w", err)
	}
	result.Dir = target
	result.GatewayConfig = filepath.Join(target, "config", "gateway.yaml")
	result.AgentConfig = filepath.Join(target, "config", "agent.yaml")
	return result, nil
}

func prepareTarget(target string, force bool) error {
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("output path %q is not a directory", target)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to use symlink output directory %q", target)
	}
	if !force {
		return fmt.Errorf("output directory %q already exists; choose another directory or use --force for an existing init bundle", target)
	}
	if _, err := readManifest(target); err != nil {
		return fmt.Errorf("refuse to overwrite %q: %w", target, err)
	}
	return nil
}

func generateBundle(root string, opts Options) (Result, []string, error) {
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o700); err != nil {
		return Result{}, nil, fmt.Errorf("create config directory: %w", err)
	}
	for _, role := range []string{"gateway", "agent"} {
		if err := os.MkdirAll(filepath.Join(root, "secrets", role), 0o700); err != nil {
			return Result{}, nil, fmt.Errorf("create %s secrets directory: %w", role, err)
		}
	}

	alpn, err := randomText("af-", 16)
	if err != nil {
		return Result{}, nil, fmt.Errorf("generate ALPN: %w", err)
	}
	agentToken, err := randomSecret()
	if err != nil {
		return Result{}, nil, fmt.Errorf("generate agent token: %w", err)
	}
	managementToken, err := randomSecret()
	if err != nil {
		return Result{}, nil, fmt.Errorf("generate management token: %w", err)
	}
	obfuscationKey, err := randomSecret()
	if err != nil {
		return Result{}, nil, fmt.Errorf("generate obfuscation key: %w", err)
	}

	gatewaySecrets := filepath.Join(root, "secrets", "gateway")
	agentSecrets := filepath.Join(root, "secrets", "agent")
	files := make([]string, 0, 20)
	writeSecret := func(role, name string, data []byte) error {
		path := filepath.Join(root, "secrets", role, name)
		if err := writeFile(path, data, 0o600); err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(filepath.Join("secrets", role, name)))
		return nil
	}
	for _, item := range []struct {
		role string
		name string
		data []byte
	}{
		{"gateway", opts.AgentID + ".token", agentToken},
		{"agent", opts.AgentID + ".token", agentToken},
		{"gateway", "management.token", managementToken},
		{"agent", "management.token", managementToken},
		{"gateway", "obfs.key", obfuscationKey},
		{"agent", "obfs.key", obfuscationKey},
	} {
		if err := writeSecret(item.role, item.name, item.data); err != nil {
			return Result{}, nil, fmt.Errorf("write %s secret: %w", item.name, err)
		}
	}

	if opts.Profile == ProfileDev {
		if err := generateDevCertificates(gatewaySecrets, agentSecrets, opts); err != nil {
			return Result{}, nil, err
		}
		for _, path := range []string{
			filepath.Join("secrets", "gateway", "ca.crt"),
			filepath.Join("secrets", "gateway", "server.crt"),
			filepath.Join("secrets", "gateway", "server.key"),
			filepath.Join("secrets", "gateway", "agents-ca.crt"),
			filepath.Join("secrets", "agent", "gateway-ca.crt"),
			filepath.Join("secrets", "agent", opts.AgentID+".crt"),
			filepath.Join("secrets", "agent", opts.AgentID+".key"),
		} {
			files = append(files, filepath.ToSlash(path))
		}
	} else {
		if err := generateProductionCSRs(gatewaySecrets, agentSecrets, opts); err != nil {
			return Result{}, nil, err
		}
		files = append(files,
			filepath.ToSlash(filepath.Join("secrets", "gateway", "server.key")),
			filepath.ToSlash(filepath.Join("secrets", "gateway", "server.csr")),
			filepath.ToSlash(filepath.Join("secrets", "agent", opts.AgentID+".key")),
			filepath.ToSlash(filepath.Join("secrets", "agent", opts.AgentID+".csr")),
		)
	}

	gatewayConfig := gatewayConfig(opts, string(alpn))
	agentConfig := agentConfig(opts, string(alpn))
	if err := gatewayConfig.Validate(); err != nil {
		return Result{}, nil, fmt.Errorf("generate gateway configuration: %w", err)
	}
	if err := agentConfig.Validate(); err != nil {
		return Result{}, nil, fmt.Errorf("generate agent configuration: %w", err)
	}
	portableConfigPaths(&gatewayConfig)
	portableConfigPaths(&agentConfig)
	if err := writeYAML(filepath.Join(root, "config", "gateway.yaml"), &gatewayConfig); err != nil {
		return Result{}, nil, fmt.Errorf("write gateway configuration: %w", err)
	}
	if err := writeYAML(filepath.Join(root, "config", "agent.yaml"), &agentConfig); err != nil {
		return Result{}, nil, fmt.Errorf("write agent configuration: %w", err)
	}
	files = append(files, "config/gateway.yaml", "config/agent.yaml")
	if err := writeFile(filepath.Join(root, "README.md"), []byte(bundleREADME(opts)), 0o644); err != nil {
		return Result{}, nil, fmt.Errorf("write bundle README: %w", err)
	}
	files = append(files, "README.md")

	return Result{
		GatewayConfig: filepath.Join(root, "config", "gateway.yaml"),
		AgentConfig:   filepath.Join(root, "config", "agent.yaml"),
		Profile:       opts.Profile,
	}, files, nil
}

func gatewayConfig(opts Options, alpn string) config.Config {
	listenHost := "0.0.0.0"
	if opts.Profile == ProfileDev {
		listenHost = "127.0.0.1"
	}
	return config.Config{
		Version: protocol.Version,
		Role:    config.RoleGateway,
		Transport: config.TransportConfig{
			ALPN:                 alpn,
			MaxBidiRemoteStreams: 257,
			HandshakeTimeoutSec:  10,
			IdleTimeoutSec:       1800,
			KeepAliveSec:         20,
		},
		Management: config.ManagementConfig{Listen: "127.0.0.1:9090", AuthTokenFile: "../secrets/gateway/management.token", Web: config.ManagementWebConfig{Enabled: boolPtr(true)}},
		Shutdown:   config.ShutdownConfig{GracePeriodSec: 30},
		Limits: config.Limits{
			MaxFrameBytes:      16 << 20,
			MaxRecordBytes:     64 << 10,
			MaxWriteBatchBytes: 256 << 10,
			MaxUDPBytes:        64 << 10,
		},
		Obfuscation: config.ObfuscationConfig{
			ProxyProfile:    config.ProfileBalanced,
			ReverseProfile:  config.ProfileStandard,
			MaxPaddingBytes: 2048,
			Transport: config.TransportObfuscationConfig{
				Mode:               config.TransportObfuscationCamouflage,
				KeyFile:            "../secrets/gateway/obfs.key",
				HandshakeShaping:   boolPtr(true),
				MinFragmentBytes:   512,
				MaxFragmentBytes:   1200,
				MaxWirePacketBytes: 1280,
			},
		},
		Logging: defaultLogging(),
		Gateway: &config.GatewayConfig{
			Listen: net.JoinHostPort(listenHost, fmt.Sprint(opts.GatewayPort)),
			TLS: config.GatewayTLS{
				CertFile:     "../secrets/gateway/server.crt",
				KeyFile:      "../secrets/gateway/server.key",
				ClientCAFile: "../secrets/gateway/agents-ca.crt",
			},
			Agents: []config.GatewayAgent{{
				ID:        opts.AgentID,
				TokenFile: "../secrets/gateway/" + opts.AgentID + ".token",
				Reverse:   config.ReverseACL{TCPPorts: []string{"28080"}, UDPPorts: []string{"21003"}},
			}},
		},
	}
}

func agentConfig(opts Options, alpn string) config.Config {
	host := opts.GatewayHost
	return config.Config{
		Version: protocol.Version,
		Role:    config.RoleAgent,
		Transport: config.TransportConfig{
			ALPN:                 alpn,
			MaxBidiRemoteStreams: 257,
			HandshakeTimeoutSec:  10,
			IdleTimeoutSec:       1800,
			KeepAliveSec:         20,
		},
		Management: config.ManagementConfig{Listen: "127.0.0.1:9091", AuthTokenFile: "../secrets/agent/management.token", Web: config.ManagementWebConfig{Enabled: boolPtr(true)}},
		Shutdown:   config.ShutdownConfig{GracePeriodSec: 30},
		Limits: config.Limits{
			MaxFrameBytes:      16 << 20,
			MaxRecordBytes:     64 << 10,
			MaxWriteBatchBytes: 256 << 10,
			MaxUDPBytes:        64 << 10,
		},
		Obfuscation: config.ObfuscationConfig{
			ProxyProfile:    config.ProfileBalanced,
			ReverseProfile:  config.ProfileStandard,
			MaxPaddingBytes: 2048,
			Transport: config.TransportObfuscationConfig{
				Mode:               config.TransportObfuscationCamouflage,
				KeyFile:            "../secrets/agent/obfs.key",
				HandshakeShaping:   boolPtr(true),
				MinFragmentBytes:   512,
				MaxFragmentBytes:   1200,
				MaxWirePacketBytes: 1280,
			},
		},
		Logging: defaultLogging(),
		Agent: &config.AgentConfig{
			ID:        opts.AgentID,
			Server:    net.JoinHostPort(host, fmt.Sprint(opts.GatewayPort)),
			TokenFile: "../secrets/agent/" + opts.AgentID + ".token",
			TLS: config.AgentTLS{
				CAFile:     "../secrets/agent/gateway-ca.crt",
				CertFile:   "../secrets/agent/" + opts.AgentID + ".crt",
				KeyFile:    "../secrets/agent/" + opts.AgentID + ".key",
				ServerName: host,
			},
			Proxy: config.ProxyConfig{
				DefaultRoute: config.RouteGateway,
				Inbounds: []config.Inbound{
					{Tag: "socks", Protocol: "socks5", Listen: "127.0.0.1:1080", User: "proxy-user", Password: "change-this-password"},
					{Tag: "http", Protocol: "http", Listen: "127.0.0.1:8080", User: "proxy-user", Password: "change-this-password"},
				},
				Sniff: config.SniffConfig{Enabled: boolPtr(true), MaxBytes: 16 << 10, TimeoutMillis: 250},
			},
			Reverse: []config.Tunnel{
				{Name: "web", Protocol: "tcp", Local: "127.0.0.1:8081", GatewayPort: 28080},
				{Name: "dns", Protocol: "udp", Local: "127.0.0.1:53", GatewayPort: 21003},
			},
		},
	}
}

func defaultLogging() config.LoggingConfig {
	enabled := true
	return config.LoggingConfig{
		Level:  "info",
		Format: "text",
		Sampling: config.SamplingConfig{
			Enabled:            &enabled,
			RatePerSecond:      5,
			Burst:              20,
			SummaryIntervalSec: 60,
			MaxKeys:            4096,
		},
	}
}

func generateDevCertificates(gatewayDir, agentDir string, opts Options) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate development CA key: %w", err)
	}
	caTemplate, err := certificateTemplate("AsterFerry Development CA", true, false, nil, nil)
	if err != nil {
		return fmt.Errorf("create development CA: %w", err)
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create development CA: %w", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	for _, path := range []string{filepath.Join(gatewayDir, "ca.crt"), filepath.Join(gatewayDir, "agents-ca.crt"), filepath.Join(agentDir, "gateway-ca.crt")} {
		if err := writeFile(path, caPEM, 0o644); err != nil {
			return fmt.Errorf("write development CA: %w", err)
		}
	}

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return fmt.Errorf("parse development CA: %w", err)
	}
	serverKey, serverDER, err := createLeaf(caCert, caKey, opts.GatewayHost, false, opts.GatewayHost)
	if err != nil {
		return fmt.Errorf("create development server certificate: %w", err)
	}
	if err := writeKeyPair(filepath.Join(gatewayDir, "server.crt"), filepath.Join(gatewayDir, "server.key"), serverDER, serverKey); err != nil {
		return fmt.Errorf("write development server certificate: %w", err)
	}
	clientKey, clientDER, err := createLeaf(caCert, caKey, opts.AgentID, true, opts.AgentID)
	if err != nil {
		return fmt.Errorf("create development agent certificate: %w", err)
	}
	if err := writeKeyPair(filepath.Join(agentDir, opts.AgentID+".crt"), filepath.Join(agentDir, opts.AgentID+".key"), clientDER, clientKey); err != nil {
		return fmt.Errorf("write development agent certificate: %w", err)
	}
	return nil
}

func generateProductionCSRs(gatewayDir, agentDir string, opts Options) error {
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate gateway key: %w", err)
	}
	serverRequest := &x509.CertificateRequest{Subject: pkix.Name{CommonName: opts.GatewayHost}, DNSNames: dnsNames(opts.GatewayHost), IPAddresses: ipNames(opts.GatewayHost), ExtraExtensions: []pkix.Extension{extendedKeyUsageExtension(serverAuthOID)}}
	serverCSR, err := x509.CreateCertificateRequest(rand.Reader, serverRequest, serverKey)
	if err != nil {
		return fmt.Errorf("create gateway CSR: %w", err)
	}
	if err := writePrivateKey(filepath.Join(gatewayDir, "server.key"), serverKey); err != nil {
		return fmt.Errorf("write gateway key: %w", err)
	}
	if err := writeFile(filepath.Join(gatewayDir, "server.csr"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: serverCSR}), 0o644); err != nil {
		return fmt.Errorf("write gateway CSR: %w", err)
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate agent key: %w", err)
	}
	identity, err := url.Parse("urn:asterferry:agent:" + opts.AgentID)
	if err != nil {
		return fmt.Errorf("create agent identity: %w", err)
	}
	clientRequest := &x509.CertificateRequest{Subject: pkix.Name{CommonName: opts.AgentID}, URIs: []*url.URL{identity}, ExtraExtensions: []pkix.Extension{extendedKeyUsageExtension(clientAuthOID)}}
	clientCSR, err := x509.CreateCertificateRequest(rand.Reader, clientRequest, clientKey)
	if err != nil {
		return fmt.Errorf("create agent CSR: %w", err)
	}
	if err := writePrivateKey(filepath.Join(agentDir, opts.AgentID+".key"), clientKey); err != nil {
		return fmt.Errorf("write agent key: %w", err)
	}
	if err := writeFile(filepath.Join(agentDir, opts.AgentID+".csr"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: clientCSR}), 0o644); err != nil {
		return fmt.Errorf("write agent CSR: %w", err)
	}
	return nil
}

func certificateTemplate(commonName string, ca, client bool, dns []string, ips []net.IP) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	if ca {
		template.IsCA = true
		template.KeyUsage |= x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		return template, nil
	}
	if client {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	return template, nil
}

func createLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, commonName string, client bool, identity string) (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	dns := dnsNames(identity)
	ips := ipNames(identity)
	template, err := certificateTemplate(commonName, false, client, dns, ips)
	if err != nil {
		return nil, nil, err
	}
	if client {
		uri, err := url.Parse("urn:asterferry:agent:" + identity)
		if err != nil {
			return nil, nil, err
		}
		template.URIs = []*url.URL{uri}
		template.DNSNames = nil
		template.IPAddresses = nil
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	return key, der, err
}

func writeKeyPair(certPath, keyPath string, certDER []byte, key *ecdsa.PrivateKey) error {
	if err := writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0o644); err != nil {
		return err
	}
	return writePrivateKey(keyPath, key)
}

func writePrivateKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writeFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func randomSecret() ([]byte, error) { return randomText("", 64) }

func randomText(prefix string, hexChars int) ([]byte, error) {
	buf := make([]byte, (hexChars+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	value := prefix + hex.EncodeToString(buf)
	return []byte(value[:len(prefix)+hexChars]), nil
}

func writeYAML(path string, value any) error {
	b, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return writeFile(path, b, 0o644)
}

func portableConfigPaths(c *config.Config) {
	if c == nil {
		return
	}
	toSlash := func(path string) string { return filepath.ToSlash(path) }
	c.Management.AuthTokenFile = toSlash(c.Management.AuthTokenFile)
	c.Obfuscation.Transport.KeyFile = toSlash(c.Obfuscation.Transport.KeyFile)
	c.Obfuscation.Transport.PreviousKeyFile = toSlash(c.Obfuscation.Transport.PreviousKeyFile)
	if c.Gateway != nil {
		c.Gateway.TLS.CertFile = toSlash(c.Gateway.TLS.CertFile)
		c.Gateway.TLS.KeyFile = toSlash(c.Gateway.TLS.KeyFile)
		c.Gateway.TLS.ClientCAFile = toSlash(c.Gateway.TLS.ClientCAFile)
		for i := range c.Gateway.Agents {
			c.Gateway.Agents[i].TokenFile = toSlash(c.Gateway.Agents[i].TokenFile)
		}
	}
	if c.Agent != nil {
		c.Agent.TokenFile = toSlash(c.Agent.TokenFile)
		c.Agent.TLS.CAFile = toSlash(c.Agent.TLS.CAFile)
		c.Agent.TLS.CertFile = toSlash(c.Agent.TLS.CertFile)
		c.Agent.TLS.KeyFile = toSlash(c.Agent.TLS.KeyFile)
	}
}

func writeJSON(path string, value any, mode os.FileMode) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeFile(path, b, mode)
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return nil
}

func copyGeneratedFiles(source, target string, files []string) error {
	for _, relative := range files {
		if filepath.IsAbs(relative) || filepath.Clean(relative) == "." || strings.HasPrefix(filepath.Clean(relative), ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid init manifest path %q", relative)
		}
		from := filepath.Join(source, filepath.FromSlash(relative))
		to := filepath.Join(target, filepath.FromSlash(relative))
		if err := rejectSymlinkParents(target, to); err != nil {
			return err
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return fmt.Errorf("read generated file %q: %w", relative, err)
		}
		info, err := os.Stat(from)
		if err != nil {
			return fmt.Errorf("stat generated file %q: %w", relative, err)
		}
		if err := writeFile(to, data, info.Mode().Perm()); err != nil {
			return fmt.Errorf("replace generated file %q: %w", relative, err)
		}
	}
	return nil
}

func rejectSymlinkParents(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("generated path %q escapes output directory", path)
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return fmt.Errorf("inspect generated path %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refuse to write through symlink %q", current)
		}
	}
	return nil
}

func readManifest(root string) (manifest, error) {
	b, err := os.ReadFile(filepath.Join(root, manifestName))
	if err != nil {
		return manifest{}, errors.New("missing init manifest")
	}
	var value manifest
	if err := json.Unmarshal(b, &value); err != nil || value.Version != manifestVersion || len(value.Files) == 0 {
		return manifest{}, errors.New("invalid init manifest")
	}
	return value, nil
}

func validateIdentifier(value string) error {
	if value == "" || len(value) > 128 {
		return errors.New("must be between 1 and 128 bytes")
	}
	for i := range value {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || (i > 0 && (c == '-' || c == '_' || c == '.')) {
			continue
		}
		return errors.New("contains an invalid character")
	}
	return nil
}

func validateHost(host string) error {
	if strings.TrimSpace(host) == "" || strings.ContainsAny(host, "\r\n/\\") {
		return errors.New("must be a hostname or IP address")
	}
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	if strings.Contains(host, ":") {
		return errors.New("must not include a port")
	}
	return nil
}

func dnsNames(host string) []string {
	if net.ParseIP(host) != nil {
		return nil
	}
	return []string{host}
}

func ipNames(host string) []net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}
	}
	return nil
}

var (
	extendedKeyUsageOID = asn1.ObjectIdentifier{2, 5, 29, 37}
	serverAuthOID       = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}
	clientAuthOID       = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}
)

func extendedKeyUsageExtension(usage asn1.ObjectIdentifier) pkix.Extension {
	value, _ := asn1.Marshal([]asn1.ObjectIdentifier{usage})
	return pkix.Extension{Id: extendedKeyUsageOID, Value: value}
}

func boolPtr(value bool) *bool { return &value }

func bundleREADME(opts Options) string {
	profileWarning := ""
	if opts.Profile == ProfileDev {
		profileWarning = "This bundle uses self-signed development certificates and is not suitable for production."
	} else {
		profileWarning = "Production certificates and CA files must be issued and installed by your PKI before startup."
	}
	return fmt.Sprintf(`# AsterFerry initialization bundle

Profile: %s
Agent: %s
Gateway endpoint: %s:%d

%s

Validate the configuration:

    asterferry validate --config config/gateway.yaml
    asterferry validate --config config/agent.yaml
    asterferry doctor --config config/gateway.yaml
    asterferry doctor --config config/agent.yaml

Start locally:

    asterferry gateway --config config/gateway.yaml
    asterferry agent --config config/agent.yaml

The Agent proxy listens on 127.0.0.1:1080 (SOCKS5) and 127.0.0.1:8080
(HTTP). Management endpoints remain on loopback by default and require the
generated management token for status, metrics, and configuration management.
The embedded Dashboard can be disabled with management.web.enabled: false.
Keep every file in secrets/ private.
`, opts.Profile, opts.AgentID, opts.GatewayHost, opts.GatewayPort, profileWarning)
}
