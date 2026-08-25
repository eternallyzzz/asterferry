// Package diagnostics contains read-only checks used by the doctor command.
// It intentionally does not start a Gateway or Agent and never includes
// secret contents in a finding.
package diagnostics

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/transport"
)

type Severity string

const (
	SeverityError Severity = "error"
	SeverityWarn  Severity = "warning"
	SeverityOK    Severity = "ok"
)

type Finding struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Path     string   `json:"path,omitempty"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`
}

type Report struct {
	Role     string    `json:"role"`
	Findings []Finding `json:"findings"`
}

func (r *Report) add(severity Severity, code, path, message, hint string) {
	if r == nil {
		return
	}
	r.Findings = append(r.Findings, Finding{Severity: severity, Code: code, Path: path, Message: message, Hint: hint})
}

func (r Report) Errors() int {
	count := 0
	for _, finding := range r.Findings {
		if finding.Severity == SeverityError {
			count++
		}
	}
	return count
}

func (r Report) Warnings() int {
	count := 0
	for _, finding := range r.Findings {
		if finding.Severity == SeverityWarn {
			count++
		}
	}
	return count
}

func Check(c *config.Config, skipPorts bool) Report {
	report := Report{}
	if c == nil {
		report.add(SeverityError, "config.nil", "config", "configuration is required", "provide a YAML configuration file")
		return report
	}
	report.Role = c.Role
	adminTokenPath := c.Management.Auth.AdminTokenFile
	viewerTokenPath := c.Management.Auth.ViewerTokenFile
	checkSecret(&report, "management.auth.admin_token_file", adminTokenPath, true)
	if viewerTokenPath != adminTokenPath {
		checkSecret(&report, "management.auth.viewer_token_file", viewerTokenPath, true)
	}
	checkManagementTLS(&report, c)
	checkObfuscation(&report, c)
	if c.Role == config.RoleGateway && c.Gateway != nil {
		checkGateway(&report, c, skipPorts)
	}
	if c.Role == config.RoleAgent && c.Agent != nil {
		checkAgent(&report, c, skipPorts)
	}
	return report
}

func checkManagementTLS(report *Report, c *config.Config) {
	if c == nil {
		return
	}
	certPath := c.Management.TLS.CertFile
	keyPath := c.Management.TLS.KeyFile
	if certPath == "" && keyPath == "" {
		return
	}
	checkCertificatePair(report, "management.tls", certPath, keyPath, x509.ExtKeyUsageServerAuth, "")
	if c.Management.TLS.CAFile != "" {
		checkCA(report, "management.tls.ca_file", c.Management.TLS.CAFile)
	}
}

func checkObfuscation(report *Report, c *config.Config) {
	if c.Obfuscation.Transport.Mode != config.TransportObfuscationCamouflage {
		report.add(SeverityOK, "obfuscation.mode", "obfuscation.transport.mode", "transport obfuscation is explicitly disabled", "use camouflage mode unless raw QUIC is intentional")
		return
	}
	if checkSecret(report, "obfuscation.transport.key_file", c.Obfuscation.Transport.KeyFile, false) {
		report.add(SeverityOK, "obfuscation.key", "obfuscation.transport.key_file", "transport obfuscation key is readable", "")
	}
}

func checkGateway(report *Report, c *config.Config, skipPorts bool) {
	g := c.Gateway
	checkCertificatePair(report, "gateway.tls", g.TLS.CertFile, g.TLS.KeyFile, x509.ExtKeyUsageServerAuth, "")
	checkCA(report, "gateway.tls.client_ca_file", g.TLS.ClientCAFile)
	if !skipPorts {
		checkPort(report, "gateway.listen", "udp", g.Listen)
		checkPort(report, "management.listen", "tcp", c.Management.Listen)
	}
	for i, agent := range g.Agents {
		path := fmt.Sprintf("gateway.agents[%d].token_file", i)
		if checkSecret(report, path, agent.TokenFile, true) {
			report.add(SeverityOK, "agent.token", path, fmt.Sprintf("token for agent %q is readable", agent.ID), "")
		}
	}
}

func checkAgent(report *Report, c *config.Config, skipPorts bool) {
	a := c.Agent
	roots := checkCA(report, "agent.tls.ca_file", a.TLS.CAFile)
	leaf := checkCertificatePair(report, "agent.tls", a.TLS.CertFile, a.TLS.KeyFile, x509.ExtKeyUsageClientAuth, a.ID)
	if leaf != nil && roots != nil {
		if err := VerifyAgentCertificate(leaf, roots, a.ID); err != nil {
			report.add(SeverityError, "certificate.agent_chain", "agent.tls", fmt.Sprintf("Agent certificate is not signed by the configured CA: %v", err), "install the matching client CA certificate")
		} else {
			report.add(SeverityOK, "certificate.agent_chain", "agent.tls", "Agent certificate chains to the configured CA", "")
		}
	}
	if checkSecret(report, "agent.token_file", a.TokenFile, true) {
		report.add(SeverityOK, "agent.token", "agent.token_file", "Agent token is readable", "")
	}
	if len(a.Proxy.Inbounds) == 0 {
		report.add(SeverityWarn, "proxy.inbounds.empty", "agent.proxy.inbounds", "no local proxy inbound is configured", "add a SOCKS5 or HTTP inbound if this Agent should serve local applications")
	}
	if !skipPorts {
		checkPort(report, "management.listen", "tcp", c.Management.Listen)
		for i, inbound := range a.Proxy.Inbounds {
			checkPort(report, fmt.Sprintf("agent.proxy.inbounds[%d].listen", i), "tcp", inbound.Listen)
		}
	}
	if a.Server != "" {
		report.add(SeverityOK, "agent.server.syntax", "agent.server", "Gateway endpoint is syntactically valid", "")
	}
}

func checkSecret(report *Report, field, path string, token bool) bool {
	if strings.TrimSpace(path) == "" {
		report.add(SeverityError, "secret.path.empty", field, "secret path is empty", "set the path or run asterferry init")
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		report.add(SeverityError, "secret.file.missing", field, fmt.Sprintf("cannot read %s", path), "create the file or correct the configuration path")
		return false
	}
	if !info.Mode().IsRegular() {
		report.add(SeverityError, "secret.file.type", field, "secret path is not a regular file", "point the setting at a regular secret file")
		return false
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		report.add(SeverityError, "secret.file.permissions", field, "secret file is writable by group or other users", "chmod 600 the secret file")
		return false
	}
	var readErr error
	if token {
		_, readErr = config.ReadToken(path)
	} else {
		_, readErr = config.ReadSecret(path)
	}
	if readErr != nil {
		report.add(SeverityError, "secret.file.invalid", field, readErr.Error(), "regenerate the secret with at least 32 bytes")
		return false
	}
	return true
}

func checkCA(report *Report, field, path string) *x509.CertPool {
	if strings.TrimSpace(path) == "" {
		report.add(SeverityError, "certificate.ca.empty", field, "CA path is empty", "set the CA file generated by your PKI or asterferry init")
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		report.add(SeverityError, "certificate.ca.read", field, fmt.Sprintf("read CA file: %v", err), "check the path and file permissions")
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		report.add(SeverityError, "certificate.ca.parse", field, "CA file contains no PEM certificate", "install a PEM encoded CA certificate")
		return nil
	}
	report.add(SeverityOK, "certificate.ca", field, "CA certificate is readable", "")
	return pool
}

func checkCertificatePair(report *Report, prefix, certPath, keyPath string, usage x509.ExtKeyUsage, expectedAgentID string) *x509.Certificate {
	if strings.TrimSpace(certPath) == "" || strings.TrimSpace(keyPath) == "" {
		report.add(SeverityError, "certificate.path.empty", prefix, "certificate and key paths are required", "set both TLS file paths")
		return nil
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		report.add(SeverityError, "certificate.keypair", prefix, fmt.Sprintf("load certificate and key: %v", err), "verify that the key matches the certificate and both files are PEM encoded")
		return nil
	}
	if len(cert.Certificate) == 0 {
		report.add(SeverityError, "certificate.empty", prefix, "certificate chain is empty", "install a valid leaf certificate")
		return nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		report.add(SeverityError, "certificate.parse", prefix, fmt.Sprintf("parse leaf certificate: %v", err), "install a valid X.509 certificate")
		return nil
	}
	checkExpiry(report, prefix, leaf)
	if !hasUsage(leaf, usage) {
		report.add(SeverityWarn, "certificate.eku", prefix, fmt.Sprintf("certificate does not explicitly contain %s", usageName(usage)), "issue the certificate with the required TLS extended key usage")
	}
	if expectedAgentID != "" {
		identity, ok := transport.CertificateAgentID(leaf)
		if !ok || identity != expectedAgentID {
			report.add(SeverityError, "certificate.agent_identity", prefix, "Agent certificate does not contain exactly one AsterFerry URI identity", "add URI SAN urn:asterferry:agent:<agent-id>")
		} else {
			report.add(SeverityOK, "certificate.agent_identity", prefix, fmt.Sprintf("Agent certificate identity is %q", identity), "")
		}
	} else if len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 {
		report.add(SeverityWarn, "certificate.server_san", prefix, "Gateway certificate has no DNS or IP SAN", "include every Agent server_name in the certificate SAN list")
	} else {
		report.add(SeverityOK, "certificate.server_san", prefix, "Gateway certificate contains server SANs", "")
	}
	report.add(SeverityOK, "certificate.keypair.valid", prefix, "TLS certificate and private key are readable and match", "")
	return leaf
}

func checkExpiry(report *Report, field string, cert *x509.Certificate) {
	now := time.Now()
	if now.Before(cert.NotBefore) {
		report.add(SeverityError, "certificate.not_yet_valid", field, "certificate is not valid yet", "check system time or issue a currently valid certificate")
		return
	}
	if !now.Before(cert.NotAfter) {
		report.add(SeverityError, "certificate.expired", field, "certificate has expired", "renew the certificate before restarting the service")
		return
	}
	if time.Until(cert.NotAfter) < 30*24*time.Hour {
		report.add(SeverityWarn, "certificate.expiring", field, fmt.Sprintf("certificate expires on %s", cert.NotAfter.Format(time.RFC3339)), "renew the certificate soon")
		return
	}
	report.add(SeverityOK, "certificate.expiry", field, fmt.Sprintf("certificate is valid until %s", cert.NotAfter.Format(time.RFC3339)), "")
}

func checkPort(report *Report, field, network, address string) {
	if strings.TrimSpace(address) == "" {
		return
	}
	var (
		listener net.Listener
		err      error
	)
	if network == "udp" {
		var packet net.PacketConn
		packet, err = net.ListenPacket("udp", address)
		if err == nil {
			_ = packet.Close()
		}
	} else {
		listener, err = net.Listen(network, address)
		if err == nil {
			_ = listener.Close()
		}
	}
	if err != nil {
		report.add(SeverityError, "port.unavailable", field, fmt.Sprintf("cannot bind %s %s: %v", network, address, err), "stop the conflicting process or use another port; use --skip-ports for a running instance")
		return
	}
	report.add(SeverityOK, "port.available", field, fmt.Sprintf("%s %s is available", network, address), "")
}

func hasUsage(cert *x509.Certificate, wanted x509.ExtKeyUsage) bool {
	if cert == nil || len(cert.ExtKeyUsage) == 0 {
		return false
	}
	for _, usage := range cert.ExtKeyUsage {
		if usage == wanted || usage == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

func usageName(usage x509.ExtKeyUsage) string {
	if usage == x509.ExtKeyUsageServerAuth {
		return "serverAuth"
	}
	if usage == x509.ExtKeyUsageClientAuth {
		return "clientAuth"
	}
	return "required TLS usage"
}

// VerifyAgentCertificate is kept separate for tests and for callers that
// already loaded an Agent certificate and CA pool.
func VerifyAgentCertificate(cert *x509.Certificate, roots *x509.CertPool, agentID string) error {
	if cert == nil {
		return errors.New("agent certificate is nil")
	}
	identity, ok := transport.CertificateAgentID(cert)
	if !ok || identity != agentID {
		return fmt.Errorf("agent certificate identity must be urn:asterferry:agent:%s", agentID)
	}
	if roots == nil {
		return errors.New("agent CA pool is required")
	}
	_, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	return err
}

func ParsePEMCertificate(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("PEM certificate is required")
	}
	return x509.ParseCertificate(block.Bytes)
}
