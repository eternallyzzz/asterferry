// Package identity owns the canonical identities embedded in AsterFerry
// certificates.
package identity

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	// AgentIdentityPrefix is the canonical URI prefix used in Agent
	// certificate URI SANs.
	AgentIdentityPrefix = "urn:asterferry:agent:"
	MaxAgentIDBytes     = 128
)

// ValidateAgentID validates the identifier embedded in an Agent identity URI.
// The rules intentionally match the configuration and wire protocol limits.
func ValidateAgentID(agentID string) error {
	if agentID == "" || len(agentID) > MaxAgentIDBytes {
		return fmt.Errorf("agent id must be between 1 and %d bytes", MaxAgentIDBytes)
	}
	if !isASCIIAlphaNumeric(agentID[0]) {
		return fmt.Errorf("agent id must start with an ASCII letter or digit")
	}
	for i := 1; i < len(agentID); i++ {
		if !isASCIIAlphaNumeric(agentID[i]) && agentID[i] != '-' && agentID[i] != '_' && agentID[i] != '.' {
			return fmt.Errorf("agent id contains an invalid character")
		}
	}
	return nil
}

// AgentIdentityURI returns the canonical URI SAN value for an Agent ID.
func AgentIdentityURI(agentID string) (string, error) {
	if err := ValidateAgentID(agentID); err != nil {
		return "", err
	}
	return AgentIdentityPrefix + agentID, nil
}

// AgentIdentityURL parses the canonical Agent identity URI for certificate
// generation.
func AgentIdentityURL(agentID string) (*url.URL, error) {
	uri, err := AgentIdentityURI(agentID)
	if err != nil {
		return nil, err
	}
	return url.Parse(uri)
}

// ParseAgentIdentityURI extracts an Agent ID from a canonical URI SAN.
func ParseAgentIdentityURI(uri *url.URL) (string, bool) {
	if uri == nil || !strings.HasPrefix(uri.String(), AgentIdentityPrefix) {
		return "", false
	}
	candidate := strings.TrimPrefix(uri.String(), AgentIdentityPrefix)
	if err := ValidateAgentID(candidate); err != nil {
		return "", false
	}
	return candidate, true
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}
