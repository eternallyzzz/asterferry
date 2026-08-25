// Package identity owns the canonical identities embedded in AsterFerry
// certificates.
package identity

import (
	"net/url"
	"strings"
)

const agentIdentityPrefix = "urn:asterferry:agent:"

// AgentIdentityURI returns the canonical URI SAN value for an Agent ID.
func AgentIdentityURI(agentID string) string {
	return agentIdentityPrefix + agentID
}

// AgentIdentityURL parses the canonical Agent identity URI for certificate
// generation.
func AgentIdentityURL(agentID string) (*url.URL, error) {
	return url.Parse(AgentIdentityURI(agentID))
}

// ParseAgentIdentityURI extracts an Agent ID from a canonical URI SAN.
func ParseAgentIdentityURI(uri *url.URL) (string, bool) {
	if uri == nil || !strings.HasPrefix(uri.String(), agentIdentityPrefix) {
		return "", false
	}
	candidate := strings.TrimPrefix(uri.String(), agentIdentityPrefix)
	if candidate == "" || strings.ContainsAny(candidate, "\r\n") {
		return "", false
	}
	return candidate, true
}
