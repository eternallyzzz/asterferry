package update

import (
	"crypto/sha256"
	"encoding/hex"
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
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/asterferry/releases":
			_, _ = fmt.Fprintf(response, `[
{"tag_name":"v9.0.0","draft":false,"prerelease":true,"assets":[{"name":"SHA256SUMS","browser_download_url":"%s/checksums"}]},
{"tag_name":"v1.2.0-rc.2","draft":false,"prerelease":false,"assets":[]},
{"tag_name":"v1.1.0","draft":false,"prerelease":false,"published_at":"2026-01-01T00:00:00Z","assets":[]},
{"tag_name":"v1.2.0","draft":false,"prerelease":false,"published_at":"2026-02-01T00:00:00Z","html_url":"https://github.com/acme/asterferry/releases/tag/v1.2.0","assets":[{"name":"SHA256SUMS","browser_download_url":"%s/checksums"},{"name":"%s","browser_download_url":"%s/archive"}]}
]`, server.URL, server.URL, archiveName, server.URL)
		case "/checksums":
			_, _ = fmt.Fprintf(response, "%s  %s\n", checksum, archiveName)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client := NewGitHubClient("acme/asterferry")
	client.APIBaseURL = server.URL
	client.HTTPClient = server.Client()
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
