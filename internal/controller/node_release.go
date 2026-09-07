package controller

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"asterferry/internal/domain"
	"asterferry/internal/jsonutil"
)

const (
	nodeReleaseSchemaVersion  = 1
	nodeReleaseMetadataName   = "node-release.json"
	nodeInstallersRoute       = "/bootstrap/node/"
	nodeEnrollmentTokenHeader = "X-AsterFerry-Enrollment-Token"
)

const (
	NodeInstallScriptSourceController = "controller"
	NodeInstallScriptSourceGitHub     = "github"
)

var releaseArtifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// NodeReleaseMetadata is the small release descriptor downloaded alongside
// the Controller. The Controller serves it to a Node installer; the Node
// binary itself remains a GitHub release asset and is downloaded on demand.
type NodeReleaseMetadata struct {
	SchemaVersion  int               `json:"schema_version"`
	Version        string            `json:"version"`
	ReleaseBaseURL string            `json:"release_base_url"`
	Artifacts      map[string]string `json:"artifacts"`
}

func normalizeNodeInstallScriptSource(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return NodeInstallScriptSourceController, nil
	}
	switch value {
	case NodeInstallScriptSourceController, NodeInstallScriptSourceGitHub:
		return value, nil
	default:
		return "", &domain.ApplyError{Code: "invalid_script_source", Path: "script_source", Message: "script_source must be controller or github"}
	}
}

func (m NodeReleaseMetadata) Validate() error {
	if m.SchemaVersion != nodeReleaseSchemaVersion {
		return fmt.Errorf("node release metadata schema version %d is unsupported", m.SchemaVersion)
	}
	if m.Version != strings.TrimSpace(m.Version) || strings.HasPrefix(m.Version, "v") || !validReleaseVersion(m.Version) || m.Version == "dev" {
		return errors.New("node release metadata version must be a published semantic release version")
	}
	baseURL := strings.TrimRight(m.ReleaseBaseURL, "/")
	if baseURL != strings.TrimSpace(baseURL) {
		return errors.New("node release metadata release_base_url must be an absolute HTTPS URL without query or fragment")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("node release metadata release_base_url must be an absolute HTTPS URL without query or fragment")
	}
	if len(m.Artifacts) == 0 {
		return errors.New("node release metadata contains no Node artifacts")
	}
	for key, artifact := range m.Artifacts {
		parts := strings.Split(key, "/")
		if len(parts) != 2 {
			return fmt.Errorf("node release metadata artifact key %q must be OS/arch", key)
		}
		platform, arch, err := normalizeBootstrapPlatform(parts[0], parts[1])
		if err != nil {
			return err
		}
		if key != platform+"/"+arch {
			return fmt.Errorf("node release metadata artifact key %q must use lowercase canonical OS/arch spelling", key)
		}
		if !releaseArtifactNamePattern.MatchString(artifact) {
			return fmt.Errorf("node release metadata artifact %q has an unsafe name", key)
		}
	}
	return nil
}

func (m NodeReleaseMetadata) Artifact(platform, arch string) (string, error) {
	platform, arch, err := normalizeBootstrapPlatform(platform, arch)
	if err != nil {
		return "", err
	}
	artifact, ok := m.Artifacts[platform+"/"+arch]
	if !ok || strings.TrimSpace(artifact) == "" {
		return "", fmt.Errorf("node release does not contain an artifact for %s/%s", platform, arch)
	}
	return artifact, nil
}

func loadNodeReleaseMetadata(config Config) (NodeReleaseMetadata, error) {
	path := filepath.Join(config.NodeInstallersDir, nodeReleaseMetadataName)
	data, err := os.ReadFile(path)
	if err != nil {
		return NodeReleaseMetadata{}, fmt.Errorf("read Node release metadata %s: %w", path, err)
	}
	var metadata NodeReleaseMetadata
	if err := jsonutil.DecodeStrict(data, &metadata); err != nil {
		if errors.Is(err, jsonutil.ErrTrailingJSON) {
			return NodeReleaseMetadata{}, errors.New("node release metadata contains trailing JSON")
		}
		return NodeReleaseMetadata{}, fmt.Errorf("decode Node release metadata: %w", err)
	}
	if err := metadata.Validate(); err != nil {
		return NodeReleaseMetadata{}, err
	}
	return metadata, nil
}

func controllerHTTPSURL(config Config) (string, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(config.GRPCAdvertise))
	if err != nil || strings.TrimSpace(host) == "" {
		return "", errors.New("controller grpc_advertise must be a reachable host:port")
	}
	_, portText, err := net.SplitHostPort(strings.TrimSpace(config.HTTPListen))
	if err != nil || strings.TrimSpace(portText) == "" || portText == "0" {
		return "", errors.New("controller http_listen must expose a non-zero HTTPS port for Node installation")
	}
	return "https://" + net.JoinHostPort(strings.Trim(host, "[]"), portText), nil
}

func nodeInstallerPath(config Config, installer string) string {
	return filepath.Join(config.NodeInstallersDir, filepath.Base(installer))
}

func nodeReleaseInstallerURL(metadata NodeReleaseMetadata, installer string) string {
	return strings.TrimRight(metadata.ReleaseBaseURL, "/") + "/v" + strings.TrimPrefix(metadata.Version, "v") + "/" + installer
}
