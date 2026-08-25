package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRuntimeSecretReadersAndFingerprint(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	secretPath := filepath.Join(dir, "secret")
	if err := os.WriteFile(tokenPath, []byte("  01234567890123456789012345678901\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("  0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := ReadToken(tokenPath)
	if err != nil || string(token) != "01234567890123456789012345678901" {
		t.Fatalf("read token = %q, err=%v", token, err)
	}
	secret, err := ReadSecret(secretPath)
	if err != nil || string(secret) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("read secret = %q, err=%v", secret, err)
	}
	fingerprint := TokenFingerprint(token)
	if len(fingerprint) != 16 || strings.Contains(fingerprint, string(token)) || fingerprint != TokenFingerprint(token) {
		t.Fatalf("unexpected token fingerprint %q", fingerprint)
	}

	if _, err := ReadToken(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing token should fail")
	}
	shortPath := filepath.Join(dir, "short")
	if err := os.WriteFile(shortPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadToken(shortPath); err == nil {
		t.Fatal("short token should fail")
	}
	longPath := filepath.Join(dir, "long")
	if err := os.WriteFile(longPath, []byte(strings.Repeat("x", 129)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(longPath); err == nil {
		t.Fatal("oversized secret should fail")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tokenPath, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadToken(tokenPath); err == nil {
			t.Fatal("owner-readable secret with group/other read permission should fail")
		}
	}
}

func TestResolveRuntimeRequiresMatchingRole(t *testing.T) {
	if _, err := (*Config)(nil).ResolveAgent(); err == nil {
		t.Fatal("nil agent config should fail")
	}
	if _, err := (&Config{Role: RoleGateway}).ResolveAgent(); err == nil {
		t.Fatal("gateway config cannot resolve as agent")
	}
	if _, err := (*Config)(nil).ResolveGateway(); err == nil {
		t.Fatal("nil gateway config should fail")
	}
	if _, err := (&Config{Role: RoleAgent}).ResolveGateway(); err == nil {
		t.Fatal("agent config cannot resolve as gateway")
	}
}

func TestLoadAndApplyEnvFromFile(t *testing.T) {
	dir := t.TempDir()
	data, err := yaml.Marshal(gatewayConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASTERFERRY_LOG_LEVEL", "debug")
	loaded, err := LoadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Logging.Level != "debug" {
		t.Fatalf("environment log level = %q", loaded.Logging.Level)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing config should fail")
	}
	if err := ApplyEnv(nil); err == nil {
		t.Fatal("nil config environment application should fail")
	}
}

func TestLoadRejectsTrailingYAMLDocument(t *testing.T) {
	dir := t.TempDir()
	data, err := yaml.Marshal(gatewayConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "multiple.yaml")
	data = append(data, []byte("\n---\nrole: agent\n")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("multiple YAML documents should be rejected")
	}
}
