package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"asterferry/internal/domain"
	"asterferry/internal/update"
)

func TestControllerUpdateCheckAndNodeUpgradeAPIFlow(t *testing.T) {
	useBootstrapTestBuildVersion(t, "1.0.0")
	dir := t.TempDir()
	config := DefaultConfig(dir)
	config.GRPCAdvertise = "127.0.0.1:9443"
	config.ServiceMode = "systemd"
	config.ProcessArgs = []string{"controller", "run", "--config", filepath.Join(dir, "controller.json"), "--service-mode", "systemd"}
	repositories, err := openTestRepositories(filepath.Join(dir, "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	server, err := NewServer(config, repositories)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	version := "1.1.0"
	assetName := update.ArchiveName(version, "linux", "amd64")
	checksum := hex.EncodeToString(make([]byte, sha256.Size))
	var releaseServer *httptest.Server
	releaseServer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/eternallyzzz/asterferry/releases":
			_, _ = fmt.Fprintf(w, `[{"tag_name":"v%s","draft":false,"prerelease":false,"published_at":"2026-09-01T00:00:00Z","html_url":"https://github.com/eternallyzzz/asterferry/releases/tag/v%s","assets":[{"name":"SHA256SUMS","browser_download_url":"%s/checksums"},{"name":"%s","browser_download_url":"%s/node"}]}]`, version, version, releaseServer.URL, assetName, releaseServer.URL)
		case "/checksums":
			_, _ = fmt.Fprintf(w, "%s  %s\n", checksum, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseServer.Close()
	client := update.NewGitHubClient(update.DefaultRepository)
	client.APIBaseURL = releaseServer.URL
	client.HTTPClient = releaseServer.Client()
	server.update.client = client

	ctx := context.Background()
	admin, err := repositories.Resources.CreateUser(ctx, "update-admin", "a-very-long-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	authToken, _, err := repositories.Resources.CreateAPIToken(ctx, admin.ID, "update-api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Resources.CreateNode(ctx, domain.Node{ID: "node-upgrade", Name: "upgrade node", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	observed := domain.ObservedState{
		SchemaVersion: domain.CurrentControlProtocolVersion,
		NodeID:        "node-upgrade",
		Healthy:       true,
		SystemInfo: &domain.SystemInfo{
			OS:           "linux",
			Architecture: "amd64",
			NodeVersion:  "1.0.0",
			ServiceMode:  "systemd",
			CollectedAt:  now,
		},
		ObservedAt: now,
	}
	observedDocument, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Resources.SaveObserved(ctx, ObservedRecord{NodeID: observed.NodeID, Document: observedDocument, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	capabilityToken := repositories.Changes.SetNodeCapabilities(observed.NodeID, []string{"node-upgrade-v1"})
	defer repositories.Changes.ClearNodeCapabilities(observed.NodeID, capabilityToken)
	actions, unsubscribe := repositories.Changes.SubscribeActions(observed.NodeID)
	defer unsubscribe()

	checkRequest := httptest.NewRequest(http.MethodPost, "/api/v1/controller/update/check", nil)
	checkRequest.Header.Set("Authorization", "Bearer "+authToken)
	checkResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(checkResponse, checkRequest)
	if checkResponse.Code != http.StatusOK {
		t.Fatalf("Controller update check status = %d, body=%s", checkResponse.Code, checkResponse.Body.String())
	}
	var checkResult struct {
		Status ControllerUpdateState `json:"status"`
	}
	if err := json.Unmarshal(checkResponse.Body.Bytes(), &checkResult); err != nil {
		t.Fatal(err)
	}
	if checkResult.Status.State != UpdateStateAvailable || checkResult.Status.LatestVersion != version {
		t.Fatalf("Controller update check status = %#v", checkResult.Status)
	}

	statusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/controller/update", nil)
	statusRequest.Header.Set("Authorization", "Bearer "+authToken)
	statusResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"latest_version":"1.1.0"`) {
		t.Fatalf("Controller update status = %d, body=%s", statusResponse.Code, statusResponse.Body.String())
	}

	upgradeRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node-upgrade/actions/upgrade", nil)
	upgradeRequest.Header.Set("Authorization", "Bearer "+authToken)
	upgradeRequest.Header.Set("Idempotency-Key", "node-upgrade-once")
	upgradeResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(upgradeResponse, upgradeRequest)
	if upgradeResponse.Code != http.StatusAccepted {
		t.Fatalf("Node upgrade request status = %d, body=%s", upgradeResponse.Code, upgradeResponse.Body.String())
	}
	var action RuntimeAction
	select {
	case action = <-actions:
	case <-time.After(time.Second):
		t.Fatal("Node upgrade action was not delivered")
	}
	if action.Name != "node_upgrade" || action.ID == "" {
		t.Fatalf("delivered Node upgrade action = %#v", action)
	}
	var payload map[string]any
	if err := json.Unmarshal(action.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["version"] != version || payload["asset_name"] != assetName || payload["sha256"] != checksum {
		t.Fatalf("Node upgrade payload = %#v", payload)
	}
	state, err := repositories.Resources.GetNodeUpdateState(ctx, observed.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != UpdateStateApplying || state.ActionID != action.ID || state.TargetVersion != version {
		t.Fatalf("persisted Node update state = %#v", state)
	}

	nodeStatusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/node-upgrade/update", nil)
	nodeStatusRequest.Header.Set("Authorization", "Bearer "+authToken)
	nodeStatusResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(nodeStatusResponse, nodeStatusRequest)
	if nodeStatusResponse.Code != http.StatusOK || !strings.Contains(nodeStatusResponse.Body.String(), `"state":"applying"`) {
		t.Fatalf("Node update status = %d, body=%s", nodeStatusResponse.Code, nodeStatusResponse.Body.String())
	}
}
