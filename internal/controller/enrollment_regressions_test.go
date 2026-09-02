package controller

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"testing"
	"time"

	"asterferry/internal/domain"
	nodepkg "asterferry/internal/node"
)

func TestGenericEnrollmentTokenCanReenrollConfiguredNode(t *testing.T) {
	config, store := openEnrollmentTestController(t)
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "gateway", Name: "gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutNodeSpec(ctx, domain.NewGatewayNodeSpec(domain.GatewaySpec{
		NodeID: "gateway", PublicEndpoints: []string{"gateway.example:4433"},
	}), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateEnrollmentToken(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	csr, _, err := nodepkg.GenerateCSR("gateway")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := store.IssueNodeCertificate(ctx, config, token, "gateway", csr)
	if err != nil {
		t.Fatalf("generic re-enrollment failed: %v", err)
	}
	leaf := parseEnrollmentCertificate(t, certificate.CertificatePEM)
	if len(leaf.Subject.Organization) != 1 || leaf.Subject.Organization[0] != "AsterFerry" {
		t.Fatalf("certificate organization = %#v, want generic identity", leaf.Subject.Organization)
	}
	node, err := store.GetNode(ctx, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if node.CertificateState != domain.CertificateActive || node.CertificateSerial == "" {
		t.Fatalf("node certificate state = %#v", node)
	}
}

func TestGenericEnrollmentTokenIsNotBoundToSpecKind(t *testing.T) {
	config, store := openEnrollmentTestController(t)
	defer store.Close()
	ctx := context.Background()
	for _, node := range []domain.Node{
		{ID: "agent", Name: "agent", Enabled: true},
		{ID: "gateway", Name: "gateway", Enabled: true},
	} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutNodeSpec(ctx, domain.NewAgentNodeSpec(domain.AgentSpec{NodeID: "agent"}), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutNodeSpec(ctx, domain.NewGatewayNodeSpec(domain.GatewaySpec{NodeID: "gateway", PublicEndpoints: []string{"gateway.example:4433"}}), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateEnrollmentToken(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{"agent", "gateway"} {
		csr, _, err := nodepkg.GenerateCSR(nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.IssueNodeCertificate(ctx, config, token, nodeID, csr); err != nil {
			t.Fatalf("generic token could not enroll %s: %v", nodeID, err)
		}
		if nodeID == "agent" {
			token, _, err = store.CreateEnrollmentToken(ctx, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func openEnrollmentTestController(t *testing.T) (Config, *Store) {
	t.Helper()
	result, err := Init(context.Background(), InitOptions{
		Dir:           filepath.Join(t.TempDir(), "controller"),
		GRPCAdvertise: "127.0.0.1:9443",
		Password:      "a-very-long-admin-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := LoadOrCreateMasterKey(result.Config.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(result.Config.DatabasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	return result.Config, store
}

func parseEnrollmentCertificate(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("certificate is not PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}
