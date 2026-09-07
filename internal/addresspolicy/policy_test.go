package addresspolicy

import (
	"net/netip"
	"testing"
)

func TestIsSpecialCoversCanonicalRanges(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want bool
	}{
		{name: "zero", addr: "0.1.2.3", want: true},
		{name: "private", addr: "10.1.2.3", want: true},
		{name: "carrier-grade NAT", addr: "100.64.0.1", want: true},
		{name: "azure metadata", addr: "168.63.129.16", want: true},
		{name: "documentation", addr: "192.0.2.1", want: true},
		{name: "multicast", addr: "224.0.0.1", want: true},
		{name: "IPv6 unique local", addr: "fc00::1", want: true},
		{name: "IPv6 link local", addr: "fe80::1", want: true},
		{name: "IPv6 public", addr: "2001:4860:4860::8888", want: false},
		{name: "IPv4 public", addr: "1.1.1.1", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatal(err)
			}
			if got := IsSpecial(addr); got != tc.want {
				t.Fatalf("IsSpecial(%s) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
	if IsSpecial(netip.MustParseAddr("::ffff:127.0.0.1")) != IsSpecial(netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("IPv4-mapped IPv6 addresses must use the IPv4 policy")
	}
	if IsSpecial(netip.Addr{}) {
		t.Fatal("invalid address must not match a special-use range")
	}
}

func TestIsSpecialPrefixRejectsBroadBypasses(t *testing.T) {
	cases := []struct {
		prefix string
		want   bool
	}{
		{prefix: "127.0.0.0/8", want: true},
		{prefix: "127.0.0.1/32", want: true},
		{prefix: "10.0.0.0/7", want: false},
		{prefix: "0.0.0.0/0", want: false},
		{prefix: "fc00::/7", want: true},
		{prefix: "fc00::/6", want: false},
	}
	for _, tc := range cases {
		prefix, err := netip.ParsePrefix(tc.prefix)
		if err != nil {
			t.Fatal(err)
		}
		if got := IsSpecialPrefix(prefix); got != tc.want {
			t.Fatalf("IsSpecialPrefix(%s) = %v, want %v", tc.prefix, got, tc.want)
		}
	}
}

func TestContainsUnmapsAddress(t *testing.T) {
	prefix := netip.MustParsePrefix("192.168.0.0/16")
	if !Contains([]netip.Prefix{prefix}, netip.MustParseAddr("192.168.1.20")) {
		t.Fatal("contained address was rejected")
	}
	if Contains([]netip.Prefix{prefix}, netip.MustParseAddr("192.169.1.20")) {
		t.Fatal("outside address was accepted")
	}
	if !Contains([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, netip.MustParseAddr("::ffff:127.0.0.1")) {
		t.Fatal("IPv4-mapped address was not unmapped")
	}
	if Contains(nil, netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("empty prefix list must reject addresses")
	}
}
