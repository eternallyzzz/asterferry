package update

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

const releaseManifestSchemaVersion = 1

// ReleaseArtifact is the signed digest entry for one release asset. The
// manifest contains names and digests, not URLs: release URLs
// come from the API response, while the signature authenticates the bytes
// that may be downloaded from those URLs.
type ReleaseArtifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// ReleaseManifest is the release metadata signed by the release key. The
// optional resources field is retained as raw JSON because it is descriptive
// release metadata, not part of the updater trust decision.
type ReleaseManifest struct {
	SchemaVersion     int               `json:"schema_version"`
	Version           string            `json:"version"`
	Tag               string            `json:"tag"`
	Commit            string            `json:"commit"`
	Protocol          string            `json:"protocol"`
	OptionalResources any               `json:"optional_resources,omitempty"`
	Artifacts         []ReleaseArtifact `json:"artifacts"`
}

// ManifestVerifier verifies a Cosign sign-blob detached signature over the
// exact bytes of a release manifest. It is injectable so release parsing can
// be tested without checking private test keys into the repository.
type ManifestVerifier interface {
	Verify(payload, signature []byte) error
}

// CosignBlobVerifier verifies the detached signature format emitted by
// `cosign sign-blob --output-signature`. Cosign stores the raw signature as
// base64 text; the public key remains a normal PEM-encoded PKIX key.
type CosignBlobVerifier struct {
	publicKey *ecdsa.PublicKey
}

func NewCosignBlobVerifier(publicKeyPEM []byte) (*CosignBlobVerifier, error) {
	block, rest := pem.Decode(publicKeyPEM)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("release public key is not a single PEM block")
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("unsupported release public key PEM type %q", block.Type)
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse release public key: %w", err)
	}
	ecdsaKey, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("unsupported release public key type %T", publicKey)
	}
	if ecdsaKey.Curve == nil || ecdsaKey.Curve.Params() == nil ||
		ecdsaKey.Curve.Params().Name != elliptic.P256().Params().Name {
		return nil, errors.New("release public key must use the ECDSA P-256 curve")
	}
	return &CosignBlobVerifier{publicKey: ecdsaKey}, nil
}

func (v *CosignBlobVerifier) Verify(payload, signature []byte) error {
	if v == nil || v.publicKey == nil {
		return errors.New("release signature verifier is not initialized")
	}
	encoded := strings.TrimSpace(string(signature))
	if encoded == "" {
		return errors.New("release signature is empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode release signature: %w", err)
	}
	hash := sha256.Sum256(payload)
	if !ecdsa.VerifyASN1(v.publicKey, hash[:], decoded) {
		return errors.New("release ECDSA P-256 signature verification failed")
	}
	return nil
}

func (m ReleaseManifest) Validate() error {
	if m.SchemaVersion != releaseManifestSchemaVersion {
		return fmt.Errorf("release manifest schema version %d is unsupported", m.SchemaVersion)
	}
	version := strings.TrimSpace(strings.TrimPrefix(m.Version, "v"))
	if version != m.Version || !IsStable(version) {
		return fmt.Errorf("release manifest version %q is not a stable release", m.Version)
	}
	if m.Tag != "v"+version {
		return fmt.Errorf("release manifest tag %q does not match version %q", m.Tag, m.Version)
	}
	if strings.TrimSpace(m.Commit) == "" || strings.ContainsAny(m.Commit, "\x00\r\n") {
		return errors.New("release manifest commit is invalid")
	}
	if len(m.Artifacts) == 0 {
		return errors.New("release manifest contains no artifacts")
	}
	seen := make(map[string]struct{}, len(m.Artifacts))
	for _, artifact := range m.Artifacts {
		if !safeAssetName(artifact.Name) {
			return fmt.Errorf("release manifest artifact name %q is invalid", artifact.Name)
		}
		if _, ok := seen[artifact.Name]; ok {
			return fmt.Errorf("release manifest contains duplicate artifact %q", artifact.Name)
		}
		seen[artifact.Name] = struct{}{}
		digest := strings.ToLower(strings.TrimSpace(artifact.SHA256))
		if len(digest) != sha256.Size*2 {
			return fmt.Errorf("release manifest artifact %q has an invalid SHA256 digest", artifact.Name)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return fmt.Errorf("release manifest artifact %q has an invalid SHA256 digest: %w", artifact.Name, err)
		}
	}
	return nil
}

func (m ReleaseManifest) Checksum(name string) (string, error) {
	for _, artifact := range m.Artifacts {
		if artifact.Name == name {
			return strings.ToLower(strings.TrimSpace(artifact.SHA256)), nil
		}
	}
	return "", fmt.Errorf("signed release manifest does not contain artifact %q", name)
}
