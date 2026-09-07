package afdp

import (
	"sort"
)

func CanonicalCapabilities(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func DefaultCapabilities() []string { return append([]string(nil), defaultCapabilities...) }

// NegotiateCapabilities returns the sorted intersection of the capabilities
// offered by both peers. An AFDP/2 peer must never claim a feature merely
// because the other side echoed it; only the intersection is actionable.
func NegotiateCapabilities(local, peer []string) []string {
	if len(local) == 0 {
		local = defaultCapabilities
	}
	peerSet := make(map[string]struct{}, len(peer))
	for _, capability := range peer {
		peerSet[capability] = struct{}{}
	}
	result := make([]string, 0, len(local))
	seen := make(map[string]struct{}, len(local))
	for _, capability := range local {
		if _, ok := peerSet[capability]; !ok {
			continue
		}
		if _, ok := seen[capability]; ok {
			continue
		}
		seen[capability] = struct{}{}
		result = append(result, capability)
	}
	return CanonicalCapabilities(result)
}

func CanonicalServiceIDs(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
