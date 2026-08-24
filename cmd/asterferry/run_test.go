package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunValidationAndCommandErrors(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("empty command should return usage error")
	}
	if err := run([]string{"unknown"}); err == nil {
		t.Fatal("unknown command should fail")
	}
	if err := run([]string{"validate", "--bad"}); err == nil {
		t.Fatal("unknown flag should fail")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	data := []byte(`version: 5
role: gateway
transport:
  alpn: af-test-123456
management:
  auth_token_file: management.token
obfuscation:
  transport:
    mode: standard
gateway:
  listen: 127.0.0.1:4433
  tls:
    cert_file: cert
    key_file: key
    client_ca_file: ca
  agents:
    - id: edge
      token_file: token
      reverse:
        tcp_ports: ["28080"]
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"validate", "-c", path}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"validate", "-c", filepath.Join(dir, "missing.yaml")}); err == nil {
		t.Fatal("missing validation config should fail")
	}
}
