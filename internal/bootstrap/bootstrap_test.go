package bootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"asterferry/internal/config"
	"asterferry/internal/transport"
)

func TestGenerateDevBundleIsRunnableAndPortable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := Generate(Options{Dir: root, Profile: ProfileDev, AgentID: "edge-a", GatewayHost: "localhost", GatewayPort: 14433})
	if err != nil {
		t.Fatal(err)
	}
	if result.Dir != root || result.GatewayConfig != filepath.Join(root, "config", "gateway.yaml") {
		t.Fatalf("unexpected result paths: %#v", result)
	}
	gateway, err := config.Load(result.GatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.ResolveGateway(); err != nil {
		t.Fatalf("resolve gateway: %v", err)
	}
	agent, err := config.Load(result.AgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ResolveAgent(); err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	if !filepath.IsAbs(gateway.Management.AuthTokenFile) || !filepath.IsAbs(agent.Agent.TokenFile) {
		t.Fatal("loaded relative secret paths must be anchored to the configuration directory")
	}

	cert, err := tls.LoadX509KeyPair(filepath.Join(root, "secrets", "agent", "edge-a.crt"), filepath.Join(root, "secrets", "agent", "edge-a.key"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if identity, ok := transport.CertificateAgentID(leaf); !ok || identity != "edge-a" {
		t.Fatalf("agent identity = %q, %v", identity, ok)
	}
	server, err := tls.LoadX509KeyPair(filepath.Join(root, "secrets", "gateway", "server.crt"), filepath.Join(root, "secrets", "gateway", "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	serverLeaf, err := x509.ParseCertificate(server.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := serverLeaf.VerifyHostname("localhost"); err != nil {
		t.Fatalf("development server SAN does not cover localhost: %v", err)
	}
}

func TestGenerateProductionBundleCreatesCSRsWithoutCertificates(t *testing.T) {
	root := filepath.Join(t.TempDir(), "prod")
	result, err := Generate(Options{Dir: root, Profile: ProfileProd, AgentID: "edge-prod", GatewayHost: "gateway.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "gateway", "server.csr")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "agent", "edge-prod.csr")); err != nil {
		t.Fatal(err)
	}
	csrBytes, err := os.ReadFile(filepath.Join(root, "secrets", "agent", "edge-prod.csr"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(csrBytes)
	if block == nil {
		t.Fatal("agent CSR is not PEM encoded")
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.URIs) != 1 || request.URIs[0].String() != "urn:asterferry:agent:edge-prod" {
		t.Fatalf("unexpected agent CSR URI SANs: %#v", request.URIs)
	}
	for _, path := range []string{
		filepath.Join(root, "secrets", "gateway", "server.crt"),
		filepath.Join(root, "secrets", "gateway", "agents-ca.crt"),
		filepath.Join(root, "secrets", "agent", "gateway-ca.crt"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("production bundle unexpectedly created certificate %s", path)
		}
	}
	if _, err := config.Load(result.GatewayConfig); err != nil {
		t.Fatalf("production config should remain structurally valid: %v", err)
	}
}

func TestGenerateProtectsExistingDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "important.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(Options{Dir: root, Profile: ProfileDev}); err == nil {
		t.Fatal("existing directory without force should be rejected")
	}
	if _, err := Generate(Options{Dir: root, Profile: ProfileDev, Force: true}); err == nil {
		t.Fatal("force must not overwrite an unrelated directory")
	}
	data, err := os.ReadFile(filepath.Join(root, "important.txt"))
	if err != nil || string(data) != "keep" {
		t.Fatalf("unrelated file changed: %q, %v", data, err)
	}
}

func TestGenerateCanRefreshItsOwnBundle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	if _, err := Generate(Options{Dir: root, Profile: ProfileDev, AgentID: "edge-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(Options{Dir: root, Profile: ProfileDev, AgentID: "edge-b", Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "gateway", "edge-b.token")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "gateway", "edge-a.token")); err != nil {
		t.Fatal("refresh should preserve old generated files rather than deleting user data")
	}
}

func TestGenerateRejectsInvalidInputs(t *testing.T) {
	for _, opts := range []Options{
		{Dir: filepath.Join(t.TempDir(), "x"), Profile: "unknown"},
		{Dir: filepath.Join(t.TempDir(), "x"), Profile: ProfileDev, AgentID: "../bad"},
		{Dir: filepath.Join(t.TempDir(), "x"), Profile: ProfileDev, GatewayHost: "bad/host"},
		{Dir: filepath.Join(t.TempDir(), "x"), Profile: ProfileDev, GatewayPort: 70000},
	} {
		if _, err := Generate(opts); err == nil {
			t.Fatalf("invalid options were accepted: %#v", opts)
		}
	}
}
