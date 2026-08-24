package cluster

import (
	"context"
	"strings"
	"testing"
)

func TestResolveNodeID(t *testing.T) {
	if got, err := ResolveNodeID(" edge-a "); err != nil || got != "edge-a" {
		t.Fatalf("configured node id = %q, err=%v", got, err)
	}
	if err := ValidateNodeID(""); err == nil {
		t.Fatal("empty node id should fail validation")
	}
	for _, value := range []string{"", strings.Repeat("a", maxNodeIDLength+1), "edge/node", "edge node", ".edge", "edge-"} {
		if value == "" {
			continue
		}
		if err := ValidateNodeID(value); err == nil {
			t.Fatalf("ValidateNodeID(%q) unexpectedly succeeded", value)
		}
	}
}

func TestLocalOwnerStoreReplacesAndIgnoresStaleRelease(t *testing.T) {
	store := NewLocalOwnerStore()
	first := Owner{AgentID: "edge", SessionID: "first", NodeID: "gw-a"}
	second := Owner{AgentID: "edge", SessionID: "second", NodeID: "gw-b"}
	if previous, replaced, err := store.Claim(context.Background(), first); err != nil || replaced || previous != (Owner{}) {
		t.Fatalf("first claim: previous=%+v replaced=%v err=%v", previous, replaced, err)
	}
	if previous, replaced, err := store.Claim(context.Background(), second); err != nil || !replaced || previous != first {
		t.Fatalf("replacement: previous=%+v replaced=%v err=%v", previous, replaced, err)
	}
	if err := store.Release(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if owner, ok, err := store.Lookup(context.Background(), "edge"); err != nil || !ok || owner != second {
		t.Fatalf("stale release removed replacement: owner=%+v ok=%v err=%v", owner, ok, err)
	}
	if err := store.Release(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Lookup(context.Background(), "edge"); err != nil || ok {
		t.Fatalf("owner remains after release: ok=%v err=%v", ok, err)
	}
}

func TestLocalOwnerStoreRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := NewLocalOwnerStore().Claim(ctx, Owner{AgentID: "edge", SessionID: "s", NodeID: "gw"})
	if err == nil {
		t.Fatal("canceled claim should fail")
	}
}
