package observability

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"asterferry/internal/bootstrap"
	"asterferry/internal/config"
	"asterferry/internal/configstore"
)

func TestManagementConfigurationAPIAndOptionalDashboard(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := bootstrap.Generate(bootstrap.Options{Dir: root, Profile: bootstrap.ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := configstore.New(result.AgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(result.AgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	token, err := config.ReadToken(c.Management.Auth.AdminTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	var restart atomic.Bool
	server, err := Start("127.0.0.1:0", nil, &dashboardTestProvider{ready: true}, token, ServerOptions{
		Config:  manager,
		Restart: func() bool { return restart.CompareAndSwap(false, true) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	base := "http://" + server.listener.Addr().String()
	client := &http.Client{Timeout: time.Second}

	response, err := client.Get(base + "/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized config status = %d", response.StatusCode)
	}
	_ = response.Body.Close()

	request, err := http.NewRequest(http.MethodGet, base+"/v1/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || strings.Contains(string(body), "change-this-password") {
		t.Fatalf("config snapshot = %d %q", response.StatusCode, body)
	}
	var snapshot configstore.Snapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.YAML, configstore.RedactedValue) {
		t.Fatal("config snapshot did not redact secret values")
	}

	idx := strings.LastIndex(snapshot.YAML, "enabled: true")
	if idx < 0 {
		t.Fatal("snapshot does not contain an enabled flag")
	}
	candidate := snapshot.YAML[:idx] + "enabled: false" + snapshot.YAML[idx+len("enabled: true"):]
	validationBody, _ := json.Marshal(map[string]string{"base_revision": snapshot.Revision, "yaml": candidate})
	response = authorizedJSON(t, client, http.MethodPost, base+"/v1/config/validate", token, validationBody)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("config validation status = %d %q", response.StatusCode, data)
	}
	_ = response.Body.Close()
	response = authorizedJSON(t, client, http.MethodPost, base+"/v1/config/apply", token, validationBody)
	if response.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("config apply status = %d %q", response.StatusCode, data)
	}
	_ = response.Body.Close()
	for attempt := 0; attempt < 20 && !restart.Load(); attempt++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !restart.Load() {
		t.Fatal("config apply did not request restart")
	}

	response, err = client.Get(base + "/dashboard/")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("dashboard should be absent when no handler is configured: %d", response.StatusCode)
	}
	_ = response.Body.Close()
}

func TestManagementServerTLS(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := bootstrap.Generate(bootstrap.Options{Dir: root, Profile: bootstrap.ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(result.GatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	token, err := config.ReadToken(c.Management.Auth.AdminTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	server, err := Start("127.0.0.1:0", nil, nil, token, ServerOptions{TLS: &TLSServerOptions{CertFile: c.Gateway.TLS.CertFile, KeyFile: c.Gateway.TLS.KeyFile}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if server.httpServer.TLSConfig == nil || server.httpServer.TLSConfig.MinVersion != tls.VersionTLS13 {
		t.Fatal("management TLS must require TLS 1.3")
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // generated test certificate
	response, err := client.Get("https://" + server.listener.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("TLS health status = %d", response.StatusCode)
	}
	_ = response.Body.Close()
}

func TestDecodeConfigRequestRequiresRevisionAndSingleJSONValue(t *testing.T) {
	for _, body := range []string{
		`{"config":{"role":"agent"}}`,
		`{"base_revision":"abc","config":{"role":"agent"}} {"extra":true}`,
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/config/validate", strings.NewReader(body))
		if _, err := decodeConfigRequest(recorder, request, true); err == nil {
			t.Fatalf("invalid configuration request was accepted: %s", body)
		}
	}
}

func authorizedJSON(t *testing.T, client *http.Client, method, url string, token, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
