package controller

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"asterferry/internal/update"
)

func TestApplyControllerStagesReleaseAndRequestsRestart(t *testing.T) {
	useBootstrapTestBuildVersion(t, "1.0.0")
	dir := t.TempDir()
	config := DefaultConfig(dir)
	config.HTTPListen = "127.0.0.1:18443"
	config.ServiceMode = "systemd"
	config.ProcessArgs = []string{"controller", "run", "--config", filepath.Join(dir, "controller.json"), "--service-mode", "systemd"}
	repositories, err := openTestRepositories(filepath.Join(dir, "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()

	activeBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binaryName := filepath.Base(activeBinary)
	archiveBytes := writeControllerTestArchive(t, dir, binaryName, []byte("staged-controller-binary"))
	checksum := sha256.Sum256(archiveBytes)
	version := "1.1.0"
	assetName := update.ArchiveName(version, runtime.GOOS, runtime.GOARCH)
	releaseServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/controller" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archiveBytes)
	}))
	defer releaseServer.Close()

	manager := NewUpdateManager(config, repositories.Resources)
	manager.client.HTTPClient = releaseServer.Client()
	manager.latest = &update.Release{
		Version: version,
		Assets: map[string]update.Asset{
			assetName: {Name: assetName, URL: releaseServer.URL + "/controller"},
		},
		Checksums: map[string]string{assetName: hex.EncodeToString(checksum[:])},
	}
	var startedBinary string
	var startedArgs []string
	started := make(chan struct{})
	manager.startUpdateHelper = func(binary string, args []string) error {
		startedBinary = binary
		startedArgs = append([]string(nil), args...)
		close(started)
		return nil
	}
	restarted := make(chan error, 1)
	manager.SetRestartCallback(func(err error) { restarted <- err })

	state, err := manager.ApplyController(context.Background(), "v"+version)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != UpdateStateApplying || state.TargetVersion != version || state.PreviousVersion != "1.0.0" {
		t.Fatalf("Controller applying state = %#v", state)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Controller update helper was not started")
	}
	if startedBinary != activeBinary || len(startedArgs) < 2 || startedArgs[0] != "controller" || startedArgs[1] != "update-helper" {
		t.Fatalf("Controller update helper invocation = %q %q", startedBinary, startedArgs)
	}
	select {
	case restartErr := <-restarted:
		if !errors.Is(restartErr, ErrControllerUpdateRestart) {
			t.Fatalf("Controller restart callback error = %v", restartErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Controller restart callback was not requested")
	}
	persisted, err := repositories.Resources.GetControllerUpdateState(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != UpdateStateApplying || persisted.TargetVersion != version || persisted.LatestVersion != version {
		t.Fatalf("persisted Controller applying state = %#v", persisted)
	}
}

func TestApplyControllerRejectsUnsupportedAndDevelopmentBuilds(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		serviceMode string
		state       string
		message     string
	}{
		{name: "unsupported container", version: "1.0.0", serviceMode: "container", state: UpdateStateUnsupported, message: "unsupported"},
		{name: "development build", version: "dev", serviceMode: "systemd", state: UpdateStateManualRequired, message: "development"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			useBootstrapTestBuildVersion(t, test.version)
			dir := t.TempDir()
			config := DefaultConfig(dir)
			config.ServiceMode = test.serviceMode
			repositories, err := openTestRepositories(filepath.Join(dir, "controller.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer repositories.Close()
			manager := NewUpdateManager(config, repositories.Resources)
			state, err := manager.ApplyController(context.Background(), "")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.message) {
				t.Fatalf("ApplyController error = %v", err)
			}
			if state.State != test.state {
				t.Fatalf("ApplyController state = %#v", state)
			}
		})
	}
}

func TestControllerAssetAndHealthURL(t *testing.T) {
	manager := &UpdateManager{}
	version := "1.2.3"
	name := update.ArchiveName(version, runtime.GOOS, runtime.GOARCH)
	wantChecksum := strings.Repeat("a", sha256.Size*2)
	asset, checksum, err := manager.ControllerAsset(update.Release{
		Version:   version,
		Assets:    map[string]update.Asset{name: {Name: name, URL: "https://example.test/" + name}},
		Checksums: map[string]string{name: wantChecksum},
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.Name != name || checksum != wantChecksum {
		t.Fatalf("Controller asset = %#v, checksum = %q", asset, checksum)
	}
	for _, test := range []struct {
		listen string
		want   string
	}{
		{listen: "127.0.0.1:8443", want: "https://127.0.0.1:8443/readyz"},
		{listen: "0.0.0.0:9443", want: "https://127.0.0.1:9443/readyz"},
	} {
		got, err := controllerHealthURL(test.listen)
		if err != nil || got != test.want {
			t.Fatalf("controllerHealthURL(%q) = %q, %v", test.listen, got, err)
		}
	}
	if _, err := controllerHealthURL(""); err == nil {
		t.Fatal("empty Controller HTTPS listen address was accepted")
	}
}

func writeControllerTestArchive(t *testing.T, dir, binaryName string, data []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	if runtime.GOOS == "windows" {
		writer := zip.NewWriter(&archive)
		entry, err := writer.Create("bin/" + binaryName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed := gzip.NewWriter(&archive)
		writer := tar.NewWriter(compressed)
		header := &tar.Header{Name: "bin/" + binaryName, Mode: 0o755, Size: int64(len(data))}
		if err := writer.WriteHeader(header); err != nil {
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
	}
	path := filepath.Join(dir, update.ArchiveName("1.1.0", runtime.GOOS, runtime.GOARCH))
	if err := os.WriteFile(path, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
