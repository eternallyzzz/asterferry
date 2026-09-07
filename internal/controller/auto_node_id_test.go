package controller

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"asterferry/internal/domain"
)

func TestPendingNodeBootstrapGeneratesStableIDAndPreservesScriptSource(t *testing.T) {
	store, err := openTestStore(t.TempDir() + "/controller.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	request := WriteOptions{Actor: "test", IdempotencyKey: "auto-node-id-once"}
	plain, pending, err := store.CreatePendingNodeBootstrapWithSource(ctx, domain.Node{Name: "auto", Enabled: true}, "linux", "amd64", NodeInstallScriptSourceGitHub, request)
	if err != nil {
		t.Fatalf("create generated bootstrap: %v", err)
	}
	if plain == "" || !regexp.MustCompile(`^node-[0-9a-f]{32}$`).MatchString(pending.NodeID) {
		t.Fatalf("generated node ID = %q", pending.NodeID)
	}
	if pending.ScriptSource != NodeInstallScriptSourceGitHub {
		t.Fatalf("script source = %q", pending.ScriptSource)
	}
	if _, err := store.GetNode(ctx, pending.NodeID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("generated node lookup before enrollment = %v", err)
	}
	_, replay, err := store.CreatePendingNodeBootstrapWithSource(ctx, domain.Node{Name: "auto", Enabled: true}, "linux", "amd64", NodeInstallScriptSourceGitHub, request)
	if !errors.Is(err, ErrSecretAlreadyCreated) {
		t.Fatalf("idempotent retry error = %v", err)
	}
	if replay.NodeID != pending.NodeID || replay.ScriptSource != pending.ScriptSource {
		t.Fatalf("idempotent replay = %#v, want %#v", replay, pending)
	}
	_, second, err := store.CreatePendingNodeBootstrapWithSource(ctx, domain.Node{Name: "auto", Enabled: true}, "linux", "amd64", NodeInstallScriptSourceController, WriteOptions{Actor: "test"})
	if err != nil {
		t.Fatalf("create second generated bootstrap: %v", err)
	}
	if second.NodeID == pending.NodeID || !regexp.MustCompile(`^node-[0-9a-f]{32}$`).MatchString(second.NodeID) {
		t.Fatalf("second generated node ID = %q", second.NodeID)
	}
	if items, err := store.ListPendingNodeBootstraps(ctx); err != nil || len(items) != 2 {
		t.Fatalf("pending bootstraps = %d, err=%v", len(items), err)
	}
}
