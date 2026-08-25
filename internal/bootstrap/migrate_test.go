package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"asterferry/internal/config"
)

func TestMigrateLegacyManagementToken(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := Generate(Options{Dir: root, Profile: ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{result.GatewayConfig, result.AgentConfig} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		lines := strings.Split(text, "\n")
		filtered := lines[:0]
		for _, line := range lines {
			if strings.Contains(line, "viewer_token_file:") {
				continue
			}
			filtered = append(filtered, line)
		}
		text = strings.Join(filtered, "\n")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "secrets", roleFromConfig(path), "management-viewer.token")); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := Migrate(MigrateOptions{Dir: root, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changed) != 4 {
		t.Fatalf("dry-run changes = %#v", preview.Changed)
	}
	if _, err := Migrate(MigrateOptions{Dir: root}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{result.GatewayConfig, result.AgentConfig} {
		c, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if c.Management.Auth.ViewerTokenFile == "" || c.Management.Auth.ViewerTokenFile == c.Management.Auth.AdminTokenFile {
			t.Fatalf("viewer token was not migrated in %s: %#v", path, c.Management.Auth)
		}
		if _, err := os.Stat(path + ".bak"); err != nil {
			t.Fatalf("migration backup missing for %s: %v", path, err)
		}
	}
}

func TestMigrateFlatLegacyManagementToken(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := Generate(Options{Dir: root, Profile: ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{result.GatewayConfig, result.AgentConfig} {
		role := roleFromConfig(path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		old := "    auth:\n" +
			"        admin_token_file: ../secrets/" + role + "/management-admin.token\n" +
			"        viewer_token_file: ../secrets/" + role + "/management-viewer.token\n"
		newValue := "    auth_token_file: ../secrets/" + role + "/management-admin.token\n"
		if !strings.Contains(text, old) {
			t.Fatalf("current auth block missing from %s", path)
		}
		text = strings.Replace(text, old, newValue, 1)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "secrets", role, "management-viewer.token")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(MigrateOptions{Dir: root}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{result.GatewayConfig, result.AgentConfig} {
		c, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if c.Management.Auth.AdminTokenFile == c.Management.Auth.ViewerTokenFile {
			t.Fatalf("flat legacy token was not split in %s: %#v", path, c.Management.Auth)
		}
	}
}

func TestMigrateRemovesLegacyFieldsWhenViewerAlreadyExists(t *testing.T) {
	raw := []byte(`management:
  auth:
    admin_token_file: admin.token
    viewer_token_file: viewer.token
  auth_token_file: admin.token
  viewer_token_file: viewer.token
`)
	updated, changed, createViewer, err := migrateConfigDocument(raw, "generated-viewer.token")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || createViewer {
		t.Fatalf("legacy cleanup changed=%t createViewer=%t", changed, createViewer)
	}
	text := string(updated)
	if strings.Contains(text, "auth_token_file") || strings.Contains(text, "viewer_token_file: old-viewer.token") {
		t.Fatalf("legacy management fields remain: %s", text)
	}
	if !strings.Contains(text, "viewer_token_file: viewer.token") {
		t.Fatalf("nested viewer token was not retained: %s", text)
	}
}

func TestMigrateRejectsConflictingAdminFields(t *testing.T) {
	raw := []byte(`management:
  auth:
    admin_token_file: nested-admin.token
  auth_token_file: legacy-admin.token
`)
	updated, changed, createViewer, err := migrateConfigDocument(raw, "generated-viewer.token")
	if !errors.Is(err, ErrMigrationConflict) {
		t.Fatalf("conflicting admin fields error = %v", err)
	}
	if updated != nil || changed || createViewer {
		t.Fatalf("conflict must not produce a migration: updated=%q changed=%t createViewer=%t", updated, changed, createViewer)
	}
}

func TestMigrateRejectsConflictingViewerFields(t *testing.T) {
	raw := []byte(`management:
  auth:
    admin_token_file: admin.token
    viewer_token_file: nested-viewer.token
  viewer_token_file: legacy-viewer.token
`)
	_, changed, createViewer, err := migrateConfigDocument(raw, "generated-viewer.token")
	if !errors.Is(err, ErrMigrationConflict) {
		t.Fatalf("conflicting viewer fields error = %v", err)
	}
	if changed || createViewer {
		t.Fatalf("conflict must not change document: changed=%t createViewer=%t", changed, createViewer)
	}
}

func roleFromConfig(path string) string {
	if strings.HasSuffix(path, "agent.yaml") {
		return "agent"
	}
	return "gateway"
}
