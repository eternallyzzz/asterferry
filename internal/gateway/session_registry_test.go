package gateway

import "testing"

func TestSessionRegistryReplaceAndLimit(t *testing.T) {
	r := newSessionRegistry()
	first := &Session{agentID: "edge"}
	second := &Session{agentID: "edge"}
	if old, ok := r.Add(first, 1); !ok || old != nil {
		t.Fatalf("first add: old=%p ok=%v", old, ok)
	}
	if old, ok := r.Add(&Session{agentID: "other"}, 1); ok || old != nil {
		t.Fatal("different agent should be rejected at max capacity")
	}
	if old, ok := r.Add(second, 1); !ok || old != first {
		t.Fatalf("replacement: old=%p ok=%v", old, ok)
	}
	if r.Count() != 1 || len(r.Snapshot()) != 1 {
		t.Fatal("registry count/snapshot mismatch")
	}
	r.Remove(first)
	if r.Count() != 1 {
		t.Fatal("removing stale session must not remove replacement")
	}
	r.Remove(second)
	if r.Count() != 0 {
		t.Fatal("session was not removed")
	}
}
