package node

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"asterferry/internal/buildinfo"
	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/update"
)

func TestStartNodeUpgradeValidationReportsFailure(t *testing.T) {
	oldVersion := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = oldVersion })

	validPayload := func(version string) []byte {
		data, err := json.Marshal(nodeUpgradeRequest{Version: version, AssetName: "release.tar.gz", AssetURL: "https://example.com/release.tar.gz", SHA256: strings.Repeat("a", sha256.Size*2)})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	tests := []struct {
		name          string
		serviceMode   string
		processArgs   []string
		payload       []byte
		wantErrorText string
	}{
		{name: "invalid-json", serviceMode: "wsl", processArgs: []string{"node", "run"}, payload: []byte("{"), wantErrorText: "upgrade payload is invalid"},
		{name: "prerelease", serviceMode: "wsl", processArgs: []string{"node", "run"}, payload: validPayload("1.1.0-rc.1"), wantErrorText: "stable verified release"},
		{name: "container", serviceMode: "container", processArgs: []string{"node", "run"}, payload: validPayload("1.1.0"), wantErrorText: "Windows service"},
		{name: "missing-restart-args", serviceMode: "systemd", payload: validPayload("1.1.0"), wantErrorText: "restart arguments"},
		{name: "development-build", serviceMode: "wsl", processArgs: []string{"node", "run"}, payload: validPayload("1.1.0"), wantErrorText: "development Node"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			version := buildinfo.Version
			if test.name == "development-build" {
				buildinfo.Version = "dev"
				t.Cleanup(func() { buildinfo.Version = version })
			}
			runtime := &Runtime{runtimeOpts: RuntimeOptions{ServiceMode: test.serviceMode, ProcessArgs: test.processArgs}}
			capture, send := captureNodeUpdateMessage()
			action := &v1.Action{Id: "validation-action", PayloadJson: test.payload}
			err := runtime.startNodeUpgrade(context.Background(), action, send)
			if err != nil {
				t.Fatalf("validation returned error: %v", err)
			}
			if capture.message == nil {
				t.Fatal("validation did not report a failure event")
			}
			attributes := decodeNodeUpdateAttributes(t, capture.message)
			if attributes["state"] != nodeUpdateStateFailed || !strings.Contains(attributes["error"], test.wantErrorText) {
				t.Fatalf("failure attributes = %#v", attributes)
			}
		})
	}
}

func TestStartNodeUpgradeDownloadsVerifiesAndStagesRelease(t *testing.T) {
	oldVersion := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	root := t.TempDir()
	binaryName := filepath.Base(mustExecutable(t))
	archive, archiveName := makeNodeUpgradeArchive(t, binaryName)
	digest := sha256.Sum256(archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer server.Close()

	var helperBinary string
	var helperArgs []string
	runtime := &Runtime{
		runtimeOpts: RuntimeOptions{
			CachePath:        filepath.Join(root, "snapshot.cache"),
			UpdateStatusPath: filepath.Join(root, "node-update.json"),
			ServiceMode:      "wsl",
			ServiceName:      "AsterFerry-Node",
			ProcessArgs:      []string{"node", "run", "--service-mode", "wsl"},
		},
		updateHTTPClient: server.Client(),
		startUpdateHelper: func(binary string, args []string) (int, error) {
			helperBinary = binary
			helperArgs = append([]string(nil), args...)
			return 12345, nil
		},
	}
	actionPayload, err := json.Marshal(nodeUpgradeRequest{
		Version:              "1.1.0",
		AssetName:            archiveName,
		AssetURL:             server.URL + "/release",
		SHA256:               hex.EncodeToString(digest[:]),
		HealthTimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	capture, send := captureNodeUpdateMessage()
	err = runtime.startNodeUpgrade(context.Background(), &v1.Action{Id: "upgrade-action", PayloadJson: actionPayload}, send)
	if !errors.Is(err, ErrNodeUpdateRestart) {
		t.Fatalf("startNodeUpgrade error = %v, want restart sentinel", err)
	}
	if helperBinary == "" || len(helperArgs) == 0 {
		t.Fatal("Node update helper was not launched")
	}
	if helperArgs[0] != "node" || helperArgs[1] != "update-helper" || !containsArgs(helperArgs, "--mode", "wsl") || !containsArgs(helperArgs, "--action-id", "upgrade-action") {
		t.Fatalf("Node update helper args = %#v", helperArgs)
	}
	if capture.message == nil {
		t.Fatal("Node upgrade did not report applying")
	}
	attributes := decodeNodeUpdateAttributes(t, capture.message)
	if attributes["state"] != nodeUpdateStateApplying || attributes["version"] != "1.1.0" || attributes["action_id"] != "upgrade-action" {
		t.Fatalf("applying attributes = %#v", attributes)
	}
	staged := filepath.Join(root, "updates", ".asterferry-1.1.0-staged-")
	matches, err := filepath.Glob(staged + "*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("staged Node executable matches = %#v, err=%v", matches, err)
	}
	if data, err := os.ReadFile(matches[0]); err != nil || string(data) != "node binary" {
		t.Fatalf("staged Node executable = %q, err=%v", data, err)
	}
}

func TestSendPendingNodeUpdateReportsHealthyOnce(t *testing.T) {
	oldVersion := buildinfo.Version
	buildinfo.Version = "1.2.3"
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	path := filepath.Join(t.TempDir(), "node-update.json")
	update.WriteReplacementResult(path, update.ReplacementResult{ActionID: "pending-action", Version: "1.2.3", State: nodeUpdateStateWaiting})
	runtime := &Runtime{runtimeOpts: RuntimeOptions{UpdateStatusPath: path, ServiceMode: "wsl"}}
	capture, send := captureNodeUpdateMessage()
	if err := runtime.sendPendingNodeUpdate(send); err != nil {
		t.Fatal(err)
	}
	if capture.message == nil {
		t.Fatal("pending Node update was not reported")
	}
	attributes := decodeNodeUpdateAttributes(t, capture.message)
	if attributes["state"] != nodeUpdateStateUpToDate || attributes["action_id"] != "pending-action" {
		t.Fatalf("pending update attributes = %#v", attributes)
	}
	if err := runtime.sendPendingNodeUpdate(func(*v1.NodeMessage) error { t.Fatal("pending update was reported twice"); return nil }); err != nil {
		t.Fatal(err)
	}
	result, err := update.ReadReplacementResult(path)
	if err != nil || result.State != "healthy" {
		t.Fatalf("pending status file = %#v, err=%v", result, err)
	}
}

type nodeMessageCapture struct {
	message *v1.NodeMessage
}

func captureNodeUpdateMessage() (*nodeMessageCapture, func(*v1.NodeMessage) error) {
	capture := &nodeMessageCapture{}
	return capture, func(value *v1.NodeMessage) error {
		capture.message = value
		return nil
	}
}

func decodeNodeUpdateAttributes(t *testing.T, message *v1.NodeMessage) map[string]string {
	t.Helper()
	if message == nil || message.GetEventBatch() == nil || len(message.GetEventBatch().GetEvents()) != 1 {
		t.Fatalf("unexpected Node event message = %#v", message)
	}
	event := message.GetEventBatch().GetEvents()[0]
	if event.GetType() != "node_upgrade" {
		t.Fatalf("Node event type = %q", event.GetType())
	}
	var attributes map[string]string
	if err := json.Unmarshal(event.GetAttributesJson(), &attributes); err != nil {
		t.Fatal(err)
	}
	return attributes
}

func makeNodeUpgradeArchive(t *testing.T, binaryName string) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	archiveName := update.ArchiveName("1.1.0", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		writer := zip.NewWriter(&buffer)
		entry, err := writer.Create(binaryName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("node binary")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes(), archiveName
	}
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	data := []byte("node binary")
	if err := writer.WriteHeader(&tar.Header{Name: binaryName, Mode: 0o755, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes(), archiveName
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func containsArgs(args []string, name, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name && args[index+1] == value {
			return true
		}
	}
	return false
}
