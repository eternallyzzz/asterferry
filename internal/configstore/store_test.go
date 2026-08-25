package configstore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"asterferry/internal/bootstrap"
	"asterferry/internal/config"
)

func TestManagerRedactsAppliesAndRollsBack(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := bootstrap.Generate(bootstrap.Options{Dir: root, Profile: bootstrap.ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(result.AgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Writable || snapshot.Role != config.RoleAgent || !strings.Contains(snapshot.YAML, "<redacted>") {
		t.Fatalf("snapshot = %#v", snapshot)
	}

	idx := strings.LastIndex(snapshot.YAML, "enabled: true")
	if idx < 0 {
		t.Fatal("generated configuration did not contain an enabled flag")
	}
	candidate := snapshot.YAML[:idx] + "enabled: false" + snapshot.YAML[idx+len("enabled: true"):]
	validation, err := manager.Validate(snapshot.Revision, []byte(candidate))
	if err != nil || !validation.Changed {
		t.Fatalf("validation = %#v, err=%v", validation, err)
	}
	if _, err := manager.Validate("stale", []byte(candidate)); err != ErrRevisionConflict {
		t.Fatalf("stale validation error = %v", err)
	}
	resultApply, err := manager.Apply(snapshot.Revision, []byte(candidate))
	if err != nil {
		t.Fatal(err)
	}
	if !resultApply.Backup || !fileExists(result.AgentConfig+backupSuffix) {
		t.Fatalf("apply result = %#v", resultApply)
	}
	loaded, err := config.Load(result.AgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agent.Proxy.Sniff.Enabled == nil || *loaded.Agent.Proxy.Sniff.Enabled {
		t.Fatal("structured configuration change was not persisted")
	}
	if loaded.Agent.Proxy.Inbounds[0].Password != "change-this-password" {
		t.Fatalf("redacted password was not preserved: %q", loaded.Agent.Proxy.Inbounds[0].Password)
	}

	current, err := manager.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rollback(current.Revision); err != nil {
		t.Fatal(err)
	}
	restored, err := config.Load(result.AgentConfig)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Agent.Proxy.Sniff.Enabled == nil || !*restored.Agent.Proxy.Sniff.Enabled {
		t.Fatal("rollback did not restore previous configuration")
	}
}

func TestJSONToYAMLAndSecretProtection(t *testing.T) {
	data, err := JSONToYAML([]byte(`{"version":6,"role":"agent","agent":{"proxy":{"inbounds":[{"password":"changed"}]}}}`))
	if err != nil || !strings.Contains(string(data), "password: changed") {
		t.Fatalf("JSONToYAML = %q, err=%v", data, err)
	}
	current := []byte("agent:\n  proxy:\n    inbounds:\n      - password: original\n")
	candidate, err := restoreRedactedSecrets([]byte("agent:\n  proxy:\n    inbounds:\n      - password: <redacted>\n"), current)
	if err != nil || !strings.Contains(string(candidate), "password: original") {
		t.Fatalf("restore redacted secret = %q, err=%v", candidate, err)
	}
	if _, err := restoreRedactedSecrets([]byte("agent:\n  proxy:\n    inbounds:\n      - password: changed\n"), current); err != ErrSecretField {
		t.Fatalf("secret mutation error = %v", err)
	}
	reordered := []byte("agent:\n  proxy:\n    inbounds:\n      - tag: second\n        password: <redacted>\n      - tag: first\n        password: <redacted>\n")
	base := []byte("agent:\n  proxy:\n    inbounds:\n      - tag: first\n        password: first-secret\n      - tag: second\n        password: second-secret\n")
	restored, err := restoreRedactedSecrets(reordered, base)
	if err != nil || !strings.Contains(string(restored), "password: second-secret") || !strings.Contains(string(restored), "password: first-secret") {
		t.Fatalf("reordered redacted secrets = %q, err=%v", restored, err)
	}
	renamed := []byte("agent:\n  proxy:\n    inbounds:\n      - tag: renamed\n        password: <redacted>\n")
	if _, err := restoreRedactedSecrets(renamed, base); !errors.Is(err, ErrSecretField) {
		t.Fatalf("renamed redacted secret error = %v", err)
	}
	redacted, err := redactYAML([]byte("# token-comment-secret\npassword: original # inline-secret\n"))
	if err != nil || strings.Contains(string(redacted), "secret") || strings.Contains(string(redacted), "original") || !strings.Contains(string(redacted), RedactedValue) {
		t.Fatalf("redacted YAML leaked secret material: %q, err=%v", redacted, err)
	}
}
