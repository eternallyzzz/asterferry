// Package addresspolicy owns the canonical special-use address table shared
// by configuration validation and runtime egress enforcement.
package addresspolicy

import "net/netip"

var specialUsePrefixes = mustPrefixes([]string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"168.63.129.16/32", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
	"192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "100::/64", "2001:2::/48", "2001:10::/28",
	"2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
})

func mustPrefixes(values []string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			panic(err)
		}
		result = append(result, prefix.Masked())
	}
	return result
}

func IsSpecial(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range specialUsePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// IsSpecialPrefix accepts only a CIDR that is equal to or narrower than a
// canonical special-use range. This prevents an exception such as 0.0.0.0/0
// from becoming an accidental global bypass.
func IsSpecialPrefix(prefix netip.Prefix) bool {
	prefix = prefix.Masked()
	for _, special := range specialUsePrefixes {
		if prefix.Addr().BitLen() == special.Addr().BitLen() && prefix.Bits() >= special.Bits() && special.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func Contains(prefixes []netip.Prefix, addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
