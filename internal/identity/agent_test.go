package identity

import (
	"net/url"
	"testing"
)

func TestAgentIdentityRoundTrip(t *testing.T) {
	uri, err := AgentIdentityURL("edge-a")
	if err != nil {
		t.Fatal(err)
	}
	if uri.String() != "urn:asterferry:agent:edge-a" {
		t.Fatalf("URI = %q", uri)
	}
	if got, ok := ParseAgentIdentityURI(uri); !ok || got != "edge-a" {
		t.Fatalf("parsed identity = %q, %v", got, ok)
	}
}

func TestParseAgentIdentityURIRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "urn:other:edge-a", "urn:asterferry:agent:", "urn:asterferry:agent:edge\n-a"} {
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
