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
	bootstrapInstallerUnix    = "install-node.sh"
	bootstrapInstallerWindows = "install-node.ps1"
)

// NodeBootstrapRequest contains the platform and the source of the one-line
// installer script. Node behavior is configured later through /spec after
// enrollment.
type NodeBootstrapRequest struct {
	Platform     string `json:"platform"`
	Arch         string `json:"arch"`
	ScriptSource string `json:"script_source"`
}

// NodeInstallationRequest creates a pending installation intent. It is kept
// separate from NodeBootstrapRequest so the API cannot accidentally confuse a
// command for an existing identity with the install-first lifecycle.
type NodeInstallationRequest struct {
	NodeID       string            `json:"node_id"`
	Name         string            `json:"name"`
	Labels       map[string]string `json:"labels,omitempty"`
	Enabled      *bool             `json:"enabled,omitempty"`
	Platform     string            `json:"platform"`
	Arch         string            `json:"arch"`
	ScriptSource string            `json:"script_source"`
}

type NodeBootstrapResponse struct {
	InstallationID string `json:"installation_id,omitempty"`
	State          string `json:"state,omitempty"`
	NodeID         string `json:"node_id"`
	Platform       string `json:"platform"`
	Arch           string `json:"arch"`
	Version        string `json:"version"`
	ScriptSource   string `json:"script_source"`
	InstallerURL   string `json:"installer_url"`
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
	base, suffix, hasSuffix := strings.Cut(value, "-")
	if hasSuffix && !strings.HasPrefix(suffix, "rc.") {
		return false
	}
	if hasSuffix {
		rcNumber := strings.TrimPrefix(suffix, "rc.")
		if rcNumber == "" {
			return false
		}
		for _, r := range rcNumber {
			if r < '0' || r > '9' {
				return false
			}
		}
		if _, err := strconv.ParseUint(rcNumber, 10, 32); err != nil {
			return false
		}
	}
	parts := strings.Split(base, ".")
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

func validateBootstrapConfiguration(config Config, scriptSource string) (NodeReleaseMetadata, []byte, error) {
	if err := validateAdvertisedAddress(config.GRPCAdvertise, "grpc_advertise"); err != nil {
		return NodeReleaseMetadata{}, nil, errors.New("controller grpc_advertise must be a reachable host:port before generating node installation commands")
	}
	if _, err := controllerHTTPSURL(config); err != nil {
		return NodeReleaseMetadata{}, nil, err
	}
	source, err := normalizeNodeInstallScriptSource(scriptSource)
	if err != nil {
		return NodeReleaseMetadata{}, nil, err
	}
	metadata, err := loadNodeReleaseMetadata(config)
	if err != nil {
		return NodeReleaseMetadata{}, nil, err
	}
	// Published Controller and Node assets are released together. Development
	// builds may point at a locally supplied metadata file, but a published
	// Controller must never silently target a different Node release.
	built := strings.TrimPrefix(strings.TrimSpace(buildinfo.Version), "v")
	if built != "" && built != "dev" && validReleaseVersion(built) && built != strings.TrimPrefix(metadata.Version, "v") {
		return NodeReleaseMetadata{}, nil, fmt.Errorf("Node release metadata version %q does not match Controller binary version %q", metadata.Version, buildinfo.Version)
	}
	if source == NodeInstallScriptSourceController {
		for _, installer := range []string{bootstrapInstallerUnix, bootstrapInstallerWindows} {
			info, statErr := os.Stat(nodeInstallerPath(config, installer))
			if statErr != nil {
				return NodeReleaseMetadata{}, nil, fmt.Errorf("Controller Node installer is unavailable: %w", statErr)
			}
			if info.IsDir() {
				return NodeReleaseMetadata{}, nil, fmt.Errorf("Controller Node installer path is a directory: %s", nodeInstallerPath(config, installer))
			}
		}
	}
	caPEM, err := os.ReadFile(config.CACertPath)
	if err != nil {
		return NodeReleaseMetadata{}, nil, fmt.Errorf("read Controller CA: %w", err)
	}
	if len(caPEM) == 0 {
		return NodeReleaseMetadata{}, nil, errors.New("Controller CA is empty")
	}
	return metadata, caPEM, nil
}

func buildNodeInstallCommand(config Config, node domain.Node, platform, arch, scriptSource, token string, caPEM []byte) (NodeBootstrapResponse, error) {
	platform, arch, err := normalizeBootstrapPlatform(platform, arch)
	if err != nil {
		return NodeBootstrapResponse{}, err
	}
	scriptSource, err = normalizeNodeInstallScriptSource(scriptSource)
	if err != nil {
		return NodeBootstrapResponse{}, err
	}
	metadata, configuredCA, err := validateBootstrapConfiguration(config, scriptSource)
	if err != nil {
		return NodeBootstrapResponse{}, err
	}
	if _, err := metadata.Artifact(platform, arch); err != nil {
		return NodeBootstrapResponse{}, err
	}
	if len(caPEM) == 0 {
		caPEM = configuredCA
	}
	if token == "" {
		return NodeBootstrapResponse{}, errors.New("node enrollment token is required")
	}
	bootstrapURL, err := controllerHTTPSURL(config)
	if err != nil {
		return NodeBootstrapResponse{}, err
	}
	installer := bootstrapInstallerUnix
	if platform == "windows" {
		installer = bootstrapInstallerWindows
	}
	installerURL := bootstrapURL + nodeInstallersRoute + installer
	if scriptSource == NodeInstallScriptSourceGitHub {
		installerURL = nodeReleaseInstallerURL(metadata, installer)
	}
	caB64 := base64.StdEncoding.EncodeToString(caPEM)
	replacement := node.CertificateState == domain.CertificateDecommissioned
	forceUnix, forceWindows := "", ""
	if replacement {
		forceUnix, forceWindows = " --force", " -Force"
	}
	argsUnix := fmt.Sprintf("--node-id %s --controller %s --bootstrap-url %s --token \"$enrollment_token\" --ca-pem-b64 \"$ca_b64\"%s",
		shellQuote(node.ID), shellQuote(config.GRPCAdvertise), shellQuote(bootstrapURL), forceUnix)
	argsWindows := fmt.Sprintf("-NodeId %s -Controller %s -BootstrapUrl %s -Token $enrollmentToken -CAPemB64 $caB64%s",
		powerShellQuote(node.ID), powerShellQuote(config.GRPCAdvertise), powerShellQuote(bootstrapURL), forceWindows)
	var command string
	if platform == "windows" {
		command = buildPowerShellInstallerCommand(installerURL, scriptSource, caB64, token, argsWindows)
	} else {
		command = buildShellInstallerCommand(installerURL, scriptSource, caB64, token, argsUnix)
	}
	state := "install"
	if replacement {
		state = "replacement"
	}
	return NodeBootstrapResponse{State: state, NodeID: node.ID, Platform: platform, Arch: arch, Version: metadata.Version, ScriptSource: scriptSource, InstallerURL: installerURL, Command: command}, nil
}

func buildShellInstallerCommand(installerURL, source, caB64, token, args string) string {
	common := "curl --disable --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.3 --retry 5 --retry-delay 1 --connect-timeout 10 --max-time 300"
	// Use printenv instead of Bash's indirect expansion here. The latter is
	// rejected by some Bash versions when nounset is enabled, which made the
	// generated one-line installer exit before it could download anything.
	sanitizeProxy := `for _asterferry_proxy_name in http_proxy https_proxy all_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY; do _asterferry_proxy_value="$(printenv "$_asterferry_proxy_name" 2>/dev/null || true)"; if [[ "$_asterferry_proxy_value" =~ [[:space:]] ]]; then unset "$_asterferry_proxy_name"; fi; done; `
	if source == NodeInstallScriptSourceController {
		noProxy := ""
		if host := installerURLHostname(installerURL); host != "" {
			noProxy = " --noproxy " + shellQuote(host)
		}
		return fmt.Sprintf("(set -euo pipefail; %ssudo -v; ca_b64=%s; enrollment_token=%s; tmp_dir=\"$(mktemp -d -t asterferry-bootstrap.XXXXXX)\"; trap 'rm -rf \"$tmp_dir\"' EXIT; ca_file=\"$tmp_dir/controller-ca.crt\"; script_file=\"$tmp_dir/install-node.sh\"; printf '%%s' \"$ca_b64\" | base64 --decode > \"$ca_file\"; %s%s --cacert \"$ca_file\" --header \"%s: $enrollment_token\" --output \"$script_file\" %s; test -s \"$script_file\" || { echo 'AsterFerry Node installer download returned an empty script' >&2; exit 1; }; sudo bash \"$script_file\" %s)",
			sanitizeProxy, shellQuote(caB64), shellQuote(token), common, noProxy, nodeEnrollmentTokenHeader, shellQuote(installerURL), args)
	}
	return fmt.Sprintf("(set -euo pipefail; %ssudo -v; ca_b64=%s; enrollment_token=%s; tmp_dir=\"$(mktemp -d -t asterferry-bootstrap.XXXXXX)\"; trap 'rm -rf \"$tmp_dir\"' EXIT; script_file=\"$tmp_dir/install-node.sh\"; %s --output \"$script_file\" %s; test -s \"$script_file\" || { echo 'AsterFerry Node installer download returned an empty script' >&2; exit 1; }; sudo bash \"$script_file\" %s)",
		sanitizeProxy, shellQuote(caB64), shellQuote(token), common, shellQuote(installerURL), args)
}

func buildPowerShellInstallerCommand(installerURL, source, caB64, token, args string) string {
	urlPart := fmt.Sprintf("curl.exe --disable --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.3 --ssl-no-revoke --retry 5 --retry-delay 1 --connect-timeout 10 --max-time 300 --output $scriptPath %s", powerShellQuote(installerURL))
	if source == NodeInstallScriptSourceController {
		noProxy := ""
		if host := installerURLHostname(installerURL); host != "" {
			noProxy = " --noproxy " + powerShellQuote(host)
		}
		urlPart = fmt.Sprintf("curl.exe --disable --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.3 --ssl-no-revoke --retry 5 --retry-delay 1 --connect-timeout 10 --max-time 300%s --cacert $caPath --header (%s + $enrollmentToken) --output $scriptPath %s",
			noProxy, powerShellQuote(nodeEnrollmentTokenHeader+": "), powerShellQuote(installerURL))
	}
	return fmt.Sprintf("$ErrorActionPreference='Stop'; foreach ($proxyName in @('http_proxy','https_proxy','all_proxy','HTTP_PROXY','HTTPS_PROXY','ALL_PROXY')) { $proxyValue=[Environment]::GetEnvironmentVariable($proxyName,[EnvironmentVariableTarget]::Process); if ($proxyValue -and $proxyValue -match '\\s') { [Environment]::SetEnvironmentVariable($proxyName,$null,[EnvironmentVariableTarget]::Process) } }; $caB64=%s; $enrollmentToken=%s; $caPath=[IO.Path]::GetTempFileName(); $scriptPath=[IO.Path]::GetTempFileName(); try { [IO.File]::WriteAllBytes($caPath, [Convert]::FromBase64String($caB64)); %s; if ($LASTEXITCODE -ne 0) { throw ('AsterFerry Node installer download failed with curl exit code ' + $LASTEXITCODE) }; if (-not (Test-Path -LiteralPath $scriptPath) -or (Get-Item -LiteralPath $scriptPath).Length -eq 0) { throw 'AsterFerry Node installer download returned an empty script' }; $script=[IO.File]::ReadAllText($scriptPath, [Text.Encoding]::UTF8); & ([scriptblock]::Create($script)) %s } finally { Remove-Item -LiteralPath $caPath,$scriptPath -Force -ErrorAction SilentlyContinue }",
		powerShellQuote(caB64), powerShellQuote(token), urlPart, args)
}

func installerURLHostname(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func powerShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
