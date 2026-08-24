package diagnostics

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"asterferry/internal/bootstrap"
	"asterferry/internal/config"
)

func TestCheckGeneratedDevBundles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := bootstrap.Generate(bootstrap.Options{Dir: root, Profile: bootstrap.ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{result.GatewayConfig, result.AgentConfig} {
		c, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		report := Check(c, true)
		if report.Errors() != 0 {
			t.Fatalf("doctor report for %s has errors: %#v", path, report.Findings)
		}
	}
	agent, err := config.Load(result.AgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	agent.Obfuscation.Transport.Mode = config.TransportObfuscationStandard
	agent.Agent.Proxy.Inbounds = nil
	report := Check(agent, true)
	if report.Errors() != 0 || report.Warnings() == 0 {
		t.Fatalf("standard/no-inbound diagnostics = %#v", report.Findings)
	}
}

func TestCheckReportsIdentityAndMissingMaterial(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := bootstrap.Generate(bootstrap.Options{Dir: root, Profile: bootstrap.ProfileDev, AgentID: "edge-a"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(result.AgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	c.Agent.ID = "different-agent"
	report := Check(c, true)
	if report.Errors() < 1 {
		t.Fatalf("expected identity error, got %#v", report.Findings)
	}
	c.Agent.TLS.CertFile = filepath.Join(root, "secrets", "agent", "missing.crt")
	report = Check(c, true)
	if report.Errors() < 1 {
		t.Fatalf("expected missing material error, got %#v", report.Findings)
	}
}

func TestCheckPortAvailabilityAndSkip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := bootstrap.Generate(bootstrap.Options{Dir: root, Profile: bootstrap.ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(result.GatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenPacket("udp", c.Gateway.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	report := Check(c, false)
	found := false
	for _, finding := range report.Findings {
		if finding.Code == "port.unavailable" && finding.Path == "gateway.listen" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected occupied port finding, got %#v", report.Findings)
	}
	if skipped := Check(c, true); skipped.Errors() != 0 {
		t.Fatalf("skip ports should remove listener error: %#v", skipped.Findings)
	}
}

func TestReportHelpersAndCertificateUtilities(t *testing.T) {
	report := Report{}
	report.add(SeverityWarn, "warning", "field", "message", "hint")
	report.add(SeverityError, "error", "field", "message", "hint")
	if report.Errors() != 1 || report.Warnings() != 1 {
		t.Fatalf("unexpected report counts: %#v", report)
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageAny} {
		if usageName(usage) == "" {
			t.Fatal("usage name should not be empty")
		}
	}
	if hasUsage(&x509.Certificate{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}, x509.ExtKeyUsageClientAuth) == false {
		t.Fatal("any EKU should satisfy a requested usage")
	}
	if hasUsage(&x509.Certificate{}, x509.ExtKeyUsageClientAuth) {
		t.Fatal("empty EKU should not satisfy a requested usage")
	}
	if _, err := ParsePEMCertificate([]byte("not pem")); err == nil {
		t.Fatal("invalid PEM should be rejected")
	}
	certDER := []byte{1, 2, 3}
	if _, err := ParsePEMCertificate(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})); err == nil {
		t.Fatal("invalid DER should be rejected")
	}
	if err := VerifyAgentCertificate(nil, nil, "edge-a"); err == nil {
		t.Fatal("nil Agent certificate should be rejected")
	}
	var expiry Report
	checkExpiry(&expiry, "cert", &x509.Certificate{NotBefore: time.Now().Add(time.Hour), NotAfter: time.Now().Add(48 * time.Hour)})
	checkExpiry(&expiry, "cert", &x509.Certificate{NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(-time.Minute)})
	checkExpiry(&expiry, "cert", &x509.Certificate{NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)})
	if expiry.Errors() != 2 || expiry.Warnings() != 1 {
		t.Fatalf("unexpected expiry findings: %#v", expiry.Findings)
	}
	if !strings.Contains(expiry.Findings[0].Code, "not_yet_valid") {
		t.Fatalf("unexpected first expiry finding: %#v", expiry.Findings[0])
	}
}
