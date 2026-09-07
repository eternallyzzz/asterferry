// Package update contains the release discovery and artifact verification
// primitives shared by the Controller and Node upgrade paths.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultRepository = "eternallyzzz/asterferry"
	DefaultCheckEvery = 6 * time.Hour
	DownloadTimeout   = 5 * time.Minute
	HealthTimeout     = 2 * time.Minute
)

var releaseVersionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-rc\.(\d+))?$`)

// Version is the normalized release version used for stable/prerelease
// ordering. Stable releases sort after release candidates for the same base
// version.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	RC         int
	Prerelease bool
}

func ParseVersion(value string) (Version, error) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "v"))
	matches := releaseVersionPattern.FindStringSubmatch(value)
	if matches == nil {
		return Version{}, fmt.Errorf("invalid release version %q", value)
	}
	parse := func(index int) int {
		value, _ := strconv.Atoi(matches[index])
		return value
	}
	result := Version{Major: parse(1), Minor: parse(2), Patch: parse(3)}
	if matches[4] != "" {
		result.Prerelease = true
		result.RC, _ = strconv.Atoi(matches[4])
	}
	return result, nil
}

func CompareVersions(left, right string) (int, error) {
	a, err := ParseVersion(left)
	if err != nil {
		return 0, err
	}
	b, err := ParseVersion(right)
	if err != nil {
		return 0, err
	}
	for _, pair := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if pair[0] < pair[1] {
			return -1, nil
		}
		if pair[0] > pair[1] {
			return 1, nil
		}
	}
	if a.Prerelease != b.Prerelease {
		if a.Prerelease {
			return -1, nil
		}
		return 1, nil
	}
	if a.RC < b.RC {
		return -1, nil
	}
	if a.RC > b.RC {
		return 1, nil
	}
	return 0, nil
}

func IsStable(value string) bool {
	parsed, err := ParseVersion(value)
	return err == nil && !parsed.Prerelease
}

func ArchiveName(version, goos, goarch string) string {
	extension := "tar.gz"
	if goos == "windows" {
		extension = "zip"
	}
	return fmt.Sprintf("asterferry_%s_%s_%s.%s", strings.TrimPrefix(version, "v"), goos, goarch, extension)
}

type Asset struct {
	Name string
	URL  string
}

type Release struct {
	Version              string
	Tag                  string
	PublishedAt          time.Time
	URL                  string
	ManifestURL          string
	ManifestSignatureURL string
	Manifest             ReleaseManifest
	ManifestVerified     bool
	Assets               map[string]Asset
	Checksums            map[string]string
}

func (r Release) Asset(name string) (Asset, error) {
	asset, ok := r.Assets[name]
	if !ok || strings.TrimSpace(asset.URL) == "" {
		return Asset{}, fmt.Errorf("release %s does not contain asset %q", r.Version, name)
	}
	return asset, nil
}

func (r Release) Checksum(name string) (string, error) {
	checksum := strings.ToLower(strings.TrimSpace(r.Checksums[name]))
	if len(checksum) != sha256.Size*2 {
		return "", fmt.Errorf("release %s does not contain a SHA256 checksum for %q", r.Version, name)
	}
	if _, err := hex.DecodeString(checksum); err != nil {
		return "", fmt.Errorf("release %s contains an invalid SHA256 checksum for %q", r.Version, name)
	}
	return checksum, nil
}

type Client struct {
	Repository string
	// APIBaseURL is empty for the public GitHub API. It is injectable for
	// integration tests and for installations fronted by an HTTPS GitHub API
	// mirror; production callers still receive the same HTTPS-only asset
	// validation below.
	APIBaseURL string
	HTTPClient *http.Client
	UserAgent  string
	// ManifestVerifier is injectable for tests and for operators that keep the
	// trust root in a separately managed build. Production callers should use
	// the embedded release key by leaving it nil.
	ManifestVerifier ManifestVerifier
}

func NewGitHubClient(repository string) Client {
	if strings.TrimSpace(repository) == "" {
		repository = DefaultRepository
	}
	return Client{
		Repository: repository,
		HTTPClient: &http.Client{Timeout: DownloadTimeout},
		UserAgent:  "asterferry-updater",
	}
}

func (c Client) LatestStable(ctx context.Context) (Release, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	repository := strings.TrimSpace(c.Repository)
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) {
		return Release{}, errors.New("GitHub repository is invalid")
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DownloadTimeout}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(c.APIBaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	parsedBase, err := url.Parse(baseURL)
	if err != nil || parsedBase.Host == "" || (parsedBase.Scheme != "https" && c.APIBaseURL != "") {
		return Release{}, errors.New("GitHub API base URL must use HTTPS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/repos/"+repository+"/releases?per_page=100", nil)
	if err != nil {
		return Release{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", c.UserAgent)
	response, err := client.Do(request)
	if err != nil {
		return Release{}, fmt.Errorf("query GitHub releases: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return Release{}, fmt.Errorf("query GitHub releases: HTTP %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var entries []struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		Draft       bool   `json:"draft"`
		Prerelease  bool   `json:"prerelease"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
		Assets      []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(response.Body).Decode(&entries); err != nil {
		return Release{}, fmt.Errorf("decode GitHub releases: %w", err)
	}
	var candidates []Release
	for _, entry := range entries {
		version := strings.TrimPrefix(strings.TrimSpace(entry.TagName), "v")
		if entry.Draft || entry.Prerelease || !IsStable(version) {
			continue
		}
		publishedAt := time.Time{}
		if strings.TrimSpace(entry.PublishedAt) != "" {
			publishedAt, err = time.Parse(time.RFC3339, entry.PublishedAt)
			if err != nil {
				continue
			}
		}
		assets := make(map[string]Asset, len(entry.Assets))
		for _, candidate := range entry.Assets {
			if !safeAssetName(candidate.Name) || !safeHTTPSURL(candidate.BrowserDownloadURL) {
				continue
			}
			assets[candidate.Name] = Asset{Name: candidate.Name, URL: candidate.BrowserDownloadURL}
		}
		candidates = append(candidates, Release{Version: version, Tag: entry.TagName, PublishedAt: publishedAt, URL: entry.HTMLURL, Assets: assets})
	}
	if len(candidates) == 0 {
		return Release{}, errors.New("no published stable GitHub release was found")
	}
	sort.Slice(candidates, func(i, j int) bool {
		comparison, compareErr := CompareVersions(candidates[i].Version, candidates[j].Version)
		return compareErr == nil && comparison > 0
	})
	latest := candidates[0]
	checksumsAsset, checksumErr := latest.Asset("SHA256SUMS")
	if checksumErr != nil {
		return Release{}, checksumErr
	}
	manifestAsset, manifestErr := latest.Asset("release-manifest.json")
	if manifestErr != nil {
		return Release{}, manifestErr
	}
	signatureAsset, signatureErr := latest.Asset("release-manifest.json.sig")
	if signatureErr != nil {
		return Release{}, signatureErr
	}
	checksums, err := c.fetchChecksums(ctx, checksumsAsset.URL)
	if err != nil {
		return Release{}, err
	}
	manifest, err := c.VerifyReleaseManifest(ctx, manifestAsset.URL, signatureAsset.URL, latest.Version, "", "")
	if err != nil {
		return Release{}, err
	}
	manifestChecksums := make(map[string]string, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		digest, digestErr := manifest.Checksum(artifact.Name)
		if digestErr != nil {
			return Release{}, digestErr
		}
		checksum, ok := checksums[artifact.Name]
		if !ok || !strings.EqualFold(strings.TrimSpace(checksum), digest) {
			return Release{}, fmt.Errorf("SHA256SUMS does not match signed release manifest for %q", artifact.Name)
		}
		manifestChecksums[artifact.Name] = digest
	}
	latest.ManifestURL = manifestAsset.URL
	latest.ManifestSignatureURL = signatureAsset.URL
	latest.Manifest = manifest
	latest.ManifestVerified = true
	latest.Checksums = manifestChecksums
	return latest, nil
}

// VerifyReleaseManifest downloads and verifies the signed release manifest.
// expectedAssetName and expectedChecksum are optional; Node uses them to bind
// the controller's action payload to the signed digest before downloading an
// archive.
func (c Client) VerifyReleaseManifest(ctx context.Context, manifestURL, signatureURL, expectedVersion, expectedAssetName, expectedChecksum string) (ReleaseManifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !safeHTTPSURL(manifestURL) || !safeHTTPSURL(signatureURL) {
		return ReleaseManifest{}, errors.New("signed release manifest URLs must use HTTPS")
	}
	manifestBytes, err := c.fetchReleaseBytes(ctx, manifestURL, 1<<20)
	if err != nil {
		return ReleaseManifest{}, fmt.Errorf("download signed release manifest: %w", err)
	}
	signatureBytes, err := c.fetchReleaseBytes(ctx, signatureURL, 64<<10)
	if err != nil {
		return ReleaseManifest{}, fmt.Errorf("download release manifest signature: %w", err)
	}
	verifier := c.ManifestVerifier
	if verifier == nil {
		verifier, err = defaultManifestVerifier()
		if err != nil {
			return ReleaseManifest{}, err
		}
	}
	if err := verifier.Verify(manifestBytes, signatureBytes); err != nil {
		return ReleaseManifest{}, fmt.Errorf("verify release manifest signature: %w", err)
	}
	var manifest ReleaseManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("decode release manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return ReleaseManifest{}, err
	}
	if expectedVersion != "" && strings.TrimPrefix(strings.TrimSpace(expectedVersion), "v") != strings.TrimPrefix(manifest.Version, "v") {
		return ReleaseManifest{}, fmt.Errorf("signed release manifest version %q does not match expected version %q", manifest.Version, expectedVersion)
	}
	if expectedAssetName != "" {
		digest, err := manifest.Checksum(expectedAssetName)
		if err != nil {
			return ReleaseManifest{}, err
		}
		if expectedChecksum != "" && !strings.EqualFold(strings.TrimSpace(expectedChecksum), digest) {
			return ReleaseManifest{}, fmt.Errorf("signed release manifest checksum does not match %q", expectedAssetName)
		}
	}
	return manifest, nil
}

func (c Client) fetchReleaseBytes(ctx context.Context, assetURL string, limit int64) ([]byte, error) {
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DownloadTimeout}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", c.UserAgent)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", response.Status)
	}
	limited := io.LimitReader(response.Body, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("release metadata exceeds size limit")
	}
	return data, nil
}

func (c Client) fetchChecksums(ctx context.Context, assetURL string) (map[string]string, error) {
	if !safeHTTPSURL(assetURL) {
		return nil, errors.New("release checksum URL must use HTTPS")
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DownloadTimeout}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", c.UserAgent)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download release checksums: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download release checksums: HTTP %s", response.Status)
	}
	result := make(map[string]string)
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 1<<20))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if safeAssetName(name) {
			result[name] = strings.ToLower(fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read release checksums: %w", err)
	}
	if len(result) == 0 {
		return nil, errors.New("release checksum file contains no valid entries")
	}
	return result, nil
}

func Download(ctx context.Context, client *http.Client, asset Asset, destination string, expectedChecksum string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !safeAssetName(asset.Name) || !safeHTTPSURL(asset.URL) {
		return errors.New("release asset URL or name is invalid")
	}
	expectedChecksum = strings.ToLower(strings.TrimSpace(expectedChecksum))
	if len(expectedChecksum) != sha256.Size*2 {
		return errors.New("release asset checksum is invalid")
	}
	if _, err := hex.DecodeString(expectedChecksum); err != nil {
		return errors.New("release asset checksum is invalid")
	}
	if client == nil {
		client = &http.Client{Timeout: DownloadTimeout}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".asterferry-download-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		temporary.Close()
		return err
	}
	request.Header.Set("User-Agent", "asterferry-updater")
	response, err := client.Do(request)
	if err != nil {
		temporary.Close()
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		temporary.Close()
		return fmt.Errorf("download %s: HTTP %s", asset.Name, response.Status)
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temporary, hasher), io.LimitReader(response.Body, 512<<20)); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != expectedChecksum {
		return fmt.Errorf("release checksum verification failed for %s", asset.Name)
	}
	if err := os.Chmod(temporaryPath, 0o700); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

func safeAssetName(value string) bool {
	if value == "" || filepath.Base(value) != value || strings.ContainsAny(value, "\\/\x00\r\n") {
		return false
	}
	for _, runeValue := range value {
		if !(runeValue == '.' || runeValue == '_' || runeValue == '-' || runeValue >= '0' && runeValue <= '9' || runeValue >= 'A' && runeValue <= 'Z' || runeValue >= 'a' && runeValue <= 'z') {
			return false
		}
	}
	return true
}

func safeHTTPSURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func CurrentPlatform() (string, string) { return runtime.GOOS, runtime.GOARCH }
