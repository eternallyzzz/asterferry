package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"asterferry/internal/domain"
)

func TestGenericNodeAndBehaviorSpecLifecycle(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "node", Name: "generic node", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	node, err := store.GetNode(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if node.Role != "" || node.SpecKind != "" {
		t.Fatalf("new node is already configured: %#v", node)
	}
	if nodes, err := store.ListNodes(ctx, domain.RoleAgent); err != nil || len(nodes) != 0 {
		t.Fatalf("unconfigured node appeared in agent filter: nodes=%#v err=%v", nodes, err)
	}
	encoded, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || containsJSONField(encoded, "role") {
		t.Fatalf("Node API still exposes the compatibility role: %s", encoded)
	}
	if _, err := store.BuildDesiredSnapshot(ctx, "node"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unconfigured node snapshot error = %v, want sql.ErrNoRows", err)
	}

	spec := domain.NewAgentNodeSpec(domain.AgentSpec{NodeID: "node"})
	if err := store.PutNodeSpec(ctx, spec, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetNodeSpec(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Kind != domain.NodeSpecAgent || loaded.Revision != 1 || loaded.Agent == nil || loaded.Gateway != nil {
		t.Fatalf("stored agent behavior is wrong: %#v", loaded)
	}
	node, err = store.GetNode(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if node.Role != domain.RoleAgent || node.SpecKind != domain.NodeSpecAgent {
		t.Fatalf("configured Node projection is wrong: %#v", node)
	}
	if nodes, err := store.ListNodes(ctx, domain.RoleAgent); err != nil || len(nodes) != 1 || nodes[0].ID != "node" {
		t.Fatalf("agent kind filter is wrong: nodes=%#v err=%v", nodes, err)
	}
	snapshot, err := store.BuildDesiredSnapshot(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Agent == nil || snapshot.Gateway != nil || snapshot.NodeID != "node" {
		t.Fatalf("agent snapshot is wrong: %#v", snapshot)
	}
	previousRecord, err := store.EnsureDesiredSnapshot(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	previousGeneration := previousRecord.Generation

	if err := store.DeleteNodeSpec(ctx, "node", WriteOptions{IfMatch: loaded.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetNodeSpec(ctx, "node"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted spec lookup error = %v, want sql.ErrNoRows", err)
	}
	node, err = store.GetNode(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if node.Role != "" || node.SpecKind != "" {
		t.Fatalf("Node behavior projection was not cleared: %#v", node)
	}
	cleared, err := store.EnsureDesiredSnapshot(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	var clearedSnapshot domain.DesiredSnapshot
	if err := json.Unmarshal(cleared.Document, &clearedSnapshot); err != nil {
		t.Fatal(err)
	}
	if cleared.Generation <= previousGeneration || clearedSnapshot.Gateway != nil || clearedSnapshot.Agent != nil || len(clearedSnapshot.Services) != 0 || len(clearedSnapshot.Assignments) != 0 {
		t.Fatalf("spec deletion did not publish an empty fail-closed snapshot: record=%#v snapshot=%#v", cleared, clearedSnapshot)
	}
}

func containsJSONField(document []byte, field string) bool {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(document, &value); err != nil {
		return false
	}
	_, exists := value[field]
	return exists
}
