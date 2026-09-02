package controller

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"asterferry/internal/domain"
	nodepkg "asterferry/internal/node"
)

func TestRolelessEnrollmentTokenUsesConfiguredNodeRole(t *testing.T) {
	config, store := openEnrollmentTestController(t)
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "gateway", Role: domain.RoleGateway, Name: "gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateEnrollmentToken(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	csr, _, err := nodepkg.GenerateCSR("gateway", "")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := store.IssueNodeCertificate(ctx, config, token, "", "gateway", csr)
	if err != nil {
		t.Fatalf("roleless re-enrollment failed: %v", err)
	}
	leaf := parseEnrollmentCertificate(t, certificate.CertificatePEM)
	organization := make(map[string]struct{}, len(leaf.Subject.Organization))
	for _, value := range leaf.Subject.Organization {
		organization[value] = struct{}{}
	}
	if _, ok := organization["AsterFerry"]; !ok {
		t.Fatalf("certificate organization = %#v, missing generic marker", leaf.Subject.Organization)
	}
	if _, ok := organization[domain.RoleGateway]; !ok {
		t.Fatalf("certificate organization = %#v, missing gateway marker", leaf.Subject.Organization)
	}
	node, err := store.GetNode(ctx, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if node.CertificateState != domain.CertificateActive || node.CertificateSerial == "" {
		t.Fatalf("node certificate state = %#v", node)
	}
}

func TestRolelessEnrollmentUsesBoundTokenRoleAndRejectsMismatch(t *testing.T) {
	config, store := openEnrollmentTestController(t)
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "agent", Role: domain.RoleAgent, Name: "agent", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	csr, _, err := nodepkg.GenerateCSR("agent", "")
	if err != nil {
		t.Fatal(err)
	}
	boundToken, _, err := store.CreateEnrollmentToken(ctx, domain.RoleAgent, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueNodeCertificate(ctx, config, boundToken, "", "agent", csr); err != nil {
		t.Fatalf("roleless request did not use the configured Node role: %v", err)
	}
	mismatchedToken, _, err := store.CreateEnrollmentToken(ctx, domain.RoleGateway, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueNodeCertificate(ctx, config, mismatchedToken, "", "agent", csr); !errors.Is(err, ErrEnrollmentRoleMismatch) {
		t.Fatalf("mismatched bound token error = %v, want ErrEnrollmentRoleMismatch", err)
	}
}

func TestGenericCSRCanRenewConfiguredNodeCertificate(t *testing.T) {
	config, store := openEnrollmentTestController(t)
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "agent", Role: domain.RoleAgent, Name: "agent", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateEnrollmentToken(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	csr, _, err := nodepkg.GenerateCSR("agent", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueNodeCertificate(ctx, config, token, "", "agent", csr); err != nil {
		t.Fatal(err)
	}
	renewedCSR, _, err := nodepkg.GenerateCSR("agent", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RenewNodeCertificate(ctx, config, "agent", renewedCSR); err != nil {
		t.Fatalf("generic CSR renewal failed: %v", err)
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
