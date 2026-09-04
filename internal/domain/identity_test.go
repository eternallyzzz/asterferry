package domain

import "testing"

func TestNodeIdentityURI(t *testing.T) {
	if got := NodeIdentityURI("node-1").String(); got != "spiffe://asterferry/node/node-1" {
		t.Fatalf("unexpected node identity URI %q", got)
	}
}
