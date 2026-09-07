package update

import _ "embed"

// releasePublicKey is the immutable trust root used by both Controller and
// Node. The matching private key is held by the release workflow secret and
// is never stored in the repository.
//
//go:embed release-public-key.pem
var releasePublicKey []byte

func defaultManifestVerifier() (ManifestVerifier, error) {
	return NewCosignBlobVerifier(releasePublicKey)
}
