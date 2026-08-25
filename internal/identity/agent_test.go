package identity

import (
	"net/url"
	"strings"
	"testing"
)

func TestAgentIdentityRoundTrip(t *testing.T) {
	uri, err := AgentIdentityURL("edge-a")
	if err != nil {
		t.Fatal(err)
	}
	if uri.String() != AgentIdentityPrefix+"edge-a" {
		t.Fatalf("URI = %q", uri)
	}
	if got, ok := ParseAgentIdentityURI(uri); !ok || got != "edge-a" {
		t.Fatalf("parsed identity = %q, %v", got, ok)
	}
}

func TestAgentIdentityURIRejectsInvalidAgentIDs(t *testing.T) {
	for _, value := range []string{"", "-edge", "edge/a", "edge a", "edge\n-a", strings.Repeat("x", MaxAgentIDBytes+1)} {
		if _, err := AgentIdentityURI(value); err == nil {
			t.Fatalf("invalid Agent ID accepted: %q", value)
		}
		if _, err := AgentIdentityURL(value); err == nil {
			t.Fatalf("invalid Agent ID URL accepted: %q", value)
		}
	}
}

func TestParseAgentIdentityURIRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "urn:other:edge-a", AgentIdentityPrefix, AgentIdentityPrefix + "edge/a", AgentIdentityPrefix + "edge a", AgentIdentityPrefix + "edge\n-a", AgentIdentityPrefix + strings.Repeat("x", MaxAgentIDBytes+1)} {
		uri, err := url.Parse(value)
		if err != nil {
			if value == "urn:asterferry:agent:edge\n-a" {
				continue
			}
			t.Fatal(err)
		}
		if _, ok := ParseAgentIdentityURI(uri); ok {
			t.Fatalf("invalid identity accepted: %q", value)
		}
	}
	if _, ok := ParseAgentIdentityURI(nil); ok {
		t.Fatal("nil identity accepted")
	}
}
