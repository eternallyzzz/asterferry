package controller

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"asterferry/internal/buildinfo"
	"asterferry/internal/domain"
)

const (
	defaultReleaseBaseURL = "https://github.com/eternallyzzz/asterferry/releases/download"

	bootstrapInstallerUnix    = "install-node.sh"
	bootstrapInstallerWindows = "install-node.ps1"
)

// NodeBootstrapRequest contains the platform and optional initial spec used to
// provision an already-enrolled node. Existing specs are never replaced by a
// bootstrap request.
type NodeBootstrapRequest struct {
	Platform    string              `json:"platform"`
	Arch        string              `json:"arch"`
	GatewaySpec *domain.GatewaySpec `json:"gateway_spec,omitempty"`
	AgentSpec   *domain.AgentSpec   `json:"agent_spec,omitempty"`
}

// NodeInstallationRequest creates a pending installation intent. It is kept
// separate from NodeBootstrapRequest so the API cannot accidentally confuse a
// command for an existing identity with the install-first lifecycle.
type NodeInstallationRequest struct {
	NodeID      string              `json:"node_id"`
	Role        string              `json:"role"`
	Name        string              `json:"name"`
	Labels      map[string]string   `json:"labels,omitempty"`
	Enabled     *bool               `json:"enabled,omitempty"`
	Platform    string              `json:"platform"`
	Arch        string              `json:"arch"`
	GatewaySpec *domain.GatewaySpec `json:"gateway_spec,omitempty"`
	AgentSpec   *domain.AgentSpec   `json:"agent_spec,omitempty"`
}

type NodeBootstrapResponse struct {
	InstallationID string `json:"installation_id,omitempty"`
	State          string `json:"state,omitempty"`
	NodeID         string `json:"node_id"`
	Role           string `json:"role"`
	Platform       string `json:"platform"`
	Arch           string `json:"arch"`
	Version        string `json:"version"`
	ExpiresAt      string `json:"expires_at"`
	Command        string `json:"command"`
}

func normalizeBootstrapPlatform(platform, arch string) (string, string, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	arch = strings.ToLower(strings.TrimSpace(arch))
	if platform != "linux" && platform != "windows" {
		return "", "", &domain.ApplyError{Code: "invalid_platform", Path: "platform", Message: "platform must be linux or windows"}
	}
	if arch != "amd64" && arch != "arm64" {
		return "", "", &domain.ApplyError{Code: "invalid_architecture", Path: "arch", Message: "architecture must be amd64 or arm64"}
	}
	if platform == "windows" && arch == "arm64" {
		return "", "", &domain.ApplyError{Code: "unsupported_platform", Path: "arch", Message: "the current Windows release supports amd64 only"}
	}
	return platform, arch, nil
}

func validReleaseVersion(value string) bool {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return false
		}
	}
	return true
}

func releaseVersion(config Config) (string, error) {
	version := strings.TrimSpace(config.ReleaseVersion)
	if version == "" {
		version = strings.TrimSpace(buildinfo.Version)
	}
	version = strings.TrimPrefix(version, "v")
	if !validReleaseVersion(version) || version == "dev" {
		return "", errors.New("a published semantic release version is required for node installation commands")
	}
	return version, nil
}

func releaseBaseURL(config Config) string {
	base := strings.TrimRight(strings.TrimSpace(config.ReleaseBaseURL), "/")
	if base == "" {
		return defaultReleaseBaseURL
	}
	return base
}

func validateBootstrapConfiguration(config Config) (string, []byte, error) {
	if err := validateAdvertisedAddress(config.GRPCAdvertise, "grpc_advertise"); err != nil {
		return "", nil, errors.New("controller grpc_advertise must be a reachable host:port before generating node installation commands")
	}
	base := releaseBaseURL(config)
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, errors.New("controller release_base_url must be an absolute HTTPS URL without query or fragment")
	}
	version, err := releaseVersion(config)
	if err != nil {
		return "", nil, err
	}
	caPEM, err := os.ReadFile(config.CACertPath)
	if err != nil {
		return "", nil, fmt.Errorf("read Controller CA: %w", err)
	}
	if len(caPEM) == 0 {
		return "", nil, errors.New("Controller CA is empty")
	}
	return version, caPEM, nil
}

func buildNodeInstallCommand(config Config, node domain.Node, platform, arch, token string, caPEM []byte) (NodeBootstrapResponse, error) {
	platform, arch, err := normalizeBootstrapPlatform(platform, arch)
	if err != nil {
		return NodeBootstrapResponse{}, err
	}
	version, configuredCA, err := validateBootstrapConfiguration(config)
	if err != nil {
		return NodeBootstrapResponse{}, err
	}
	if len(caPEM) == 0 {
		caPEM = configuredCA
	}
	if token == "" {
		return NodeBootstrapResponse{}, errors.New("node enrollment token is required")
	}
	if len(caPEM) == 0 {
		return NodeBootstrapResponse{}, errors.New("Controller CA is empty")
	}
	caB64 := base64.RawStdEncoding.EncodeToString(caPEM)
	base := releaseBaseURL(config)
	installer := bootstrapInstallerUnix
	if platform == "windows" {
		installer = bootstrapInstallerWindows
	}
	installerURL := base + "/v" + version + "/" + installer
	var command string
	if platform == "windows" {
		command = fmt.Sprintf("$script=(Invoke-WebRequest -UseBasicParsing -Uri %s).Content; & ([scriptblock]::Create($script)) -Role %s -NodeId %s -Controller %s -Token %s -CAPemB64 %s -ReleaseBaseURL %s -Version %s -Arch %s",
			powerShellQuote(installerURL), powerShellQuote(node.Role), powerShellQuote(node.ID), powerShellQuote(config.GRPCAdvertise), powerShellQuote(token), powerShellQuote(caB64), powerShellQuote(base), powerShellQuote(version), powerShellQuote(arch))
	} else {
		command = fmt.Sprintf("curl --fail --silent --show-error --location --proto '=https' --tlsv1.3 %s | sudo bash -s -- --role %s --node-id %s --controller %s --token %s --ca-pem-b64 %s --release-base-url %s --version %s --arch %s",
			shellQuote(installerURL), shellQuote(node.Role), shellQuote(node.ID), shellQuote(config.GRPCAdvertise), shellQuote(token), shellQuote(caB64), shellQuote(base), shellQuote(version), shellQuote(arch))
	}
	return NodeBootstrapResponse{NodeID: node.ID, Role: node.Role, Platform: platform, Arch: arch, Version: version, Command: command}, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func powerShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
