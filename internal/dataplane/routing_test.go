package dataplane

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"asterferry/internal/domain"
)

func TestGeoIPResolverDisablesCleanlyWithoutPath(t *testing.T) {
	resolver := NewGeoIPResolver("")
	if resolver.Available() {
		t.Fatal("disabled GeoIP resolver reported available")
	}
	if err := resolver.Error(); err != nil {
		t.Fatalf("disabled GeoIP resolver error = %v, want nil", err)
	}
}

func TestGeoIPResolverRejectsStaleDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cn.mmdb")
	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	resolver := NewGeoIPResolverWithMaxAge(path, time.Hour)
	if resolver.Available() {
		t.Fatal("stale GeoIP database reported available")
	}
	if err := resolver.Error(); !errors.Is(err, ErrGeoIPStale) {
		t.Fatalf("stale GeoIP error = %v, want ErrGeoIPStale", err)
	}
}

func TestSelectRouteKeepsExplicitRulesWithoutGeoIPDatabase(t *testing.T) {
	spec := domain.AgentSpec{Routes: []domain.RouteRule{
		{Enabled: true, Destination: "gateway", CIDRs: []string{"10.0.0.0/8"}},
		{Enabled: true, Destination: "gateway", Domains: []string{"*.internal.example"}},
		{Enabled: true, Destination: "gateway", GeoIP: []string{"private"}},
	}}
	for _, test := range []struct {
		name   string
		target string
	}{
		{name: "cidr", target: net.JoinHostPort("10.1.2.3", "443")},
		{name: "domain", target: net.JoinHostPort("api.internal.example", "443")},
		{name: "private", target: net.JoinHostPort("127.0.0.1", "443")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := SelectRouteWithResolver(spec, test.target, nil); got != "gateway" {
				t.Fatalf("route for %s = %q, want gateway", test.target, got)
			}
		})
	}
}
