package managementclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"asterferry/internal/config"
)

func TestClientSelectsScopeTokenAndParsesAPIErrors(t *testing.T) {
	root := t.TempDir()
	adminPath := filepath.Join(root, "admin.token")
	viewerPath := filepath.Join(root, "viewer.token")
	admin := strings.Repeat("a", 32)
	viewer := strings.Repeat("v", 32)
	if err := os.WriteFile(adminPath, []byte(admin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(viewerPath, []byte(viewer+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/viewer":
			if r.Header.Get("Authorization") != "Bearer "+viewer {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"scope": "viewer"})
		case "/admin":
			if r.Header.Get("Authorization") != "Bearer "+admin {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"scope": "admin"})
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"busy","message":"try again"}}`))
		}
	}))
	defer server.Close()

	c := &config.Config{Management: config.ManagementConfig{
		Listen: server.Listener.Addr().String(),
		Auth:   config.ManagementAuthConfig{AdminTokenFile: adminPath, ViewerTokenFile: viewerPath},
	}}
	viewerClient, err := New(c, Viewer, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := viewerClient.JSON(context.Background(), http.MethodGet, "/viewer", nil, &result); err != nil {
		t.Fatal(err)
	}
	if result["scope"] != "viewer" {
		t.Fatalf("viewer response = %#v", result)
	}

	adminClient, err := New(c, Admin, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result = nil
	if err := adminClient.JSON(context.Background(), http.MethodGet, "/admin", nil, &result); err != nil {
		t.Fatal(err)
	}
	if result["scope"] != "admin" {
		t.Fatalf("admin response = %#v", result)
	}

	err = adminClient.JSON(context.Background(), http.MethodGet, "/error", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || !strings.Contains(apiErr.Error(), "busy") {
		t.Fatalf("API error = %v", err)
	}
}
