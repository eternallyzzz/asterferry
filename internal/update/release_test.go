package update

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		{"1.0.0-rc.2", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.2", 1},
		{"1.0.1", "1.0.0", 1},
		{"2.0.0", "10.0.0", -1},
		{"1.0.0-rc.3", "1.0.0-rc.2", 1},
		{"1.0.0", "1.0.0", 0},
	}
	for _, test := range cases {
		got, err := CompareVersions(test.left, test.right)
		if err != nil {
			t.Fatalf("CompareVersions(%q,%q): %v", test.left, test.right, err)
		}
		if got != test.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestStableVersionAndArchiveName(t *testing.T) {
	if IsStable("1.0.0-rc.1") {
		t.Fatal("release candidate was classified as stable")
	}
	if !IsStable("1.0.0") {
		t.Fatal("stable release was not classified as stable")
	}
	if got := ArchiveName("v1.2.3", "windows", "amd64"); got != "asterferry_1.2.3_windows_amd64.zip" {
		t.Fatalf("Windows archive = %q", got)
	}
	if got := ArchiveName("1.2.3", "linux", "arm64"); got != "asterferry_1.2.3_linux_arm64.tar.gz" {
		t.Fatalf("Linux archive = %q", got)
	}
}

func TestLatestStableIgnoresPreReleases(t *testing.T) {
	archiveName := ArchiveName("1.2.0", "linux", "amd64")
	checksum := strings.Repeat("a", sha256.Size*2)
	manifest := fmt.Sprintf(`{"schema_version":1,"version":"1.2.0","tag":"v1.2.0","commit":"abc123","protocol":"v3","artifacts":[{"name":"%s","sha256":"%s"}]}`, archiveName, checksum)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/asterferry/releases":
			_, _ = fmt.Fprintf(response, `[
{"tag_name":"v9.0.0","draft":false,"prerelease":true,"assets":[{"name":"SHA256SUMS","browser_download_url":"%s/checksums"}]},
{"tag_name":"v1.2.0-rc.2","draft":false,"prerelease":false,"assets":[]},
{"tag_name":"v1.1.0","draft":false,"prerelease":false,"published_at":"2026-01-01T00:00:00Z","assets":[]},

{"tag_name":"v1.2.0","draft":false,"prerelease":false,"published_at":"2026-02-01T00:00:00Z","html_url":"https://github.com/acme/asterferry/releases/tag/v1.2.0","assets":[{"name":"SHA256SUMS","browser_download_url":"%s/checksums"},{"name":"release-manifest.json","browser_download_url":"%s/manifest"},{"name":"release-manifest.json.sig","browser_download_url":"%s/signature"},{"name":"%s","browser_download_url":"%s/archive"}]}
]`, server.URL, server.URL, server.URL, server.URL, archiveName, server.URL)
		case "/checksums":
			_, _ = fmt.Fprintf(response, "%s  %s\n", checksum, archiveName)
		case "/manifest":
			_, _ = response.Write([]byte(manifest))
		case "/signature":
			_, _ = response.Write([]byte("test-signature"))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client := NewGitHubClient("acme/asterferry")
	client.APIBaseURL = server.URL
	client.HTTPClient = server.Client()
	client.ManifestVerifier = acceptingManifestVerifier{}
	release, err := client.LatestStable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "1.2.0" {
		t.Fatalf("latest stable version = %q, want 1.2.0", release.Version)
	}
	if _, err := release.Asset(archiveName); err != nil {
		t.Fatalf("latest stable archive missing: %v", err)
	}
	if got, err := release.Checksum(archiveName); err != nil || got != checksum {
		t.Fatalf("latest stable checksum = %q, %v; want %q", got, err, checksum)
	}
	if !release.ManifestVerified || release.ManifestURL == "" || release.ManifestSignatureURL == "" {
		t.Fatalf("signed release manifest was not retained: %#v", release)
	}
}

type acceptingManifestVerifier struct{}

func (acceptingManifestVerifier) Verify(payload, signature []byte) error {
	if len(payload) == 0 || !bytes.Equal(signature, []byte("test-signature")) {
		return fmt.Errorf("unexpected test manifest signature")
	}
	return nil
}

func TestCosignBlobVerifierRejectsUnsupportedKeys(t *testing.T) {
	tests := []struct {
		name string
		key  any
	}{
		{name: "RSA", key: mustGenerateRSAKey(t)},
		{name: "Ed25519", key: mustGenerateEd25519Key(t)},
		{name: "ECDSA P-384", key: mustGenerateECDSAKey(t, elliptic.P384())},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			publicKeyBytes, err := x509.MarshalPKIXPublicKey(test.key)
			if err != nil {
				t.Fatal(err)
			}
			_, err = NewCosignBlobVerifier(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicKeyBytes}))
			if err == nil {
				t.Fatal("unsupported release public key was accepted")
			}
		})
	}
}

func mustGenerateRSAKey(t *testing.T) *rsa.PublicKey {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &privateKey.PublicKey
}

func mustGenerateEd25519Key(t *testing.T) ed25519.PublicKey {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

func mustGenerateECDSAKey(t *testing.T, curve elliptic.Curve) *ecdsa.PublicKey {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &privateKey.PublicKey
}

func TestCosignBlobVerifierVerifiesECDSASignature(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyBytes, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewCosignBlobVerifier(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicKeyBytes}))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("cosign-compatible ECDSA payload")
	digest := sha256.Sum256(payload)
	signature, err := ecdsa.SignASN1(rand.Reader, privateKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(payload, []byte(base64.StdEncoding.EncodeToString(signature))); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify([]byte("tampered"), []byte(base64.StdEncoding.EncodeToString(signature))); err == nil {
		t.Fatal("tampered manifest was accepted")
	}
	if err := verifier.Verify(payload, []byte("not-base64")); err == nil {
		t.Fatal("malformed signature was accepted")
	}
}

func TestEmbeddedReleasePublicKeyParses(t *testing.T) {
	if _, err := defaultManifestVerifier(); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadVerifiesSHA256OverHTTPS(t *testing.T) {
	payload := []byte("verified release payload")
	digest := sha256.Sum256(payload)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/release" {
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write(payload)
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "archive.bin")
	client := server.Client()
	err := Download(t.Context(), client, Asset{Name: "archive.bin", URL: server.URL + "/release"}, destination, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, destination); got != string(payload) {
		t.Fatalf("downloaded payload = %q, want %q", got, payload)
	}
	if err := Download(t.Context(), client, Asset{Name: "archive.bin", URL: server.URL + "/release"}, destination, strings.Repeat("0", sha256.Size*2)); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
