package bootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"asterferry/internal/config"
	"asterferry/internal/identity"
	"asterferry/internal/transport"
	"gopkg.in/yaml.v3"
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
	if !filepath.IsAbs(gateway.Management.Auth.AdminTokenFile) || !filepath.IsAbs(gateway.Management.Auth.ViewerTokenFile) || !filepath.IsAbs(agent.Agent.TokenFile) {
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
	if len(request.URIs) != 1 || request.URIs[0].String() != identity.AgentIdentityURI("edge-prod") {
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

func TestGenerateWritesMinimalRoleConfigs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "minimal")
	result, err := Generate(Options{Dir: root, Profile: ProfileProd, GatewayHost: "gateway.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := os.ReadFile(result.GatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := os.ReadFile(result.AgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"gateway": gateway, "agent": agent} {
		text := string(data)
		if strings.Contains(text, "auth_token_file:") || strings.Contains(text, "initial_stream_receive_window_bytes:") {
			t.Fatalf("%s config contains migration/zero-value fields:\n%s", name, text)
		}
		if strings.Contains(text, "expose_domain_at_debug:") || strings.Count(text, "enabled: false") != 1 {
			t.Fatalf("%s config contains an unexpected default field:\n%s", name, text)
		}
	}
	if !strings.Contains(string(gateway), "enabled: false") {
		t.Fatal("production gateway config must explicitly disable the Dashboard")
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

func TestPruneGeneratedYAMLPreservesExplicitFalseFields(t *testing.T) {
	var document yaml.Node
	raw := "management:\n  web:\n    enabled: false\nagent:\n  proxy:\n    sniff:\n      enabled: false\nlogging:\n  sampling:\n    enabled: false\ngateway:\n  agents:\n    - egress:\n        enabled: false\n"
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		t.Fatal(err)
	}
	if !pruneGeneratedYAML(&document) {
		t.Fatal("configuration was pruned completely")
	}
	data, err := yaml.Marshal(document.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Count(text, "enabled: false") != 3 {
		t.Fatalf("explicit false fields = %q", text)
	}
	if strings.Contains(text, "egress:") {
		t.Fatalf("ordinary zero-value false field was retained: %q", text)
	}
}

func TestRandomTextSupportsEvenAndOddLengths(t *testing.T) {
	for _, test := range []struct {
		prefix string
		length int
	}{
		{prefix: "", length: 16},
		{prefix: "af-", length: 15},
	} {
		value, err := randomText(test.prefix, test.length)
		if err != nil {
			t.Fatal(err)
		}
		if len(value) != len(test.prefix)+test.length || !strings.HasPrefix(string(value), test.prefix) {
			t.Fatalf("random text = %q, want prefix %q and length %d", value, test.prefix, len(test.prefix)+test.length)
		}
	}
	if _, err := randomText("", -1); err == nil {
		t.Fatal("negative random text length should fail")
	}
}

func TestGenerateCanRefreshItsOwnBundle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	if _, err := Generate(Options{Dir: root, Profile: ProfileDev, AgentID: "edge-a"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "operator-notes.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(Options{Dir: root, Profile: ProfileDev, AgentID: "edge-b", Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "gateway", "edge-b.token")); err != nil {
		t.Fatal(err)
	}
	backup := root + ".bak"
	if _, err := os.Stat(filepath.Join(backup, "secrets", "gateway", "edge-a.token")); err != nil {
		t.Fatal("refresh should preserve the complete old bundle in its backup")
	}
	if data, err := os.ReadFile(filepath.Join(backup, "operator-notes.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("backup lost operator data: %q, %v", data, err)
	}
	if _, err := Generate(Options{Dir: root, Profile: ProfileDev, AgentID: "edge-c", Force: true}); err == nil {
		t.Fatal("force refresh must refuse to overwrite an existing backup")
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
