package routing

import (
	"net"
	"testing"

	"asterferry/internal/config"
)

func TestRouterMatchesOrderedRules(t *testing.T) {
	r, err := New(config.ProxyConfig{
		DefaultRoute: config.RouteGateway,
		Rules: []config.RouteRule{
			{Inbound: "socks", CIDRs: []string{"10.0.0.0/8"}, Route: config.RouteDirect},
			{Inbound: "socks", Domains: []string{"example.com"}, Route: config.RouteDirect},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.Choose("socks", "internal", net.ParseIP("10.1.2.3")); got != config.RouteDirect {
		t.Fatalf("cidr route: %s", got)
	}
	if got := r.Choose("socks", "api.example.com", net.ParseIP("192.0.2.1")); got != config.RouteDirect {
		t.Fatalf("domain route: %s", got)
	}
	if got := r.Choose("http", "api.example.com", net.ParseIP("192.0.2.1")); got != config.RouteGateway {
		t.Fatalf("inbound isolation: %s", got)
	}
}

func TestRouterPrivateGeoIP(t *testing.T) {
	r, err := New(config.ProxyConfig{DefaultRoute: config.RouteGateway, Rules: []config.RouteRule{{GeoIP: []string{"private"}, Route: config.RouteDirect}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.Choose("socks", "localhost", net.ParseIP("127.0.0.1")); got != config.RouteDirect {
		t.Fatalf("private route: %s", got)
	}
}
