package security

import (
	"context"
	"testing"

	"asterferry/internal/config"
)

func TestEgressPolicyFailsClosed(t *testing.T) {
	p, err := NewEgressPolicy(config.EgressPolicy{
		Enabled:        true,
		TCPPorts:       []string{"80", "443"},
		MaxConnections: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Allow(context.Background(), "tcp", "127.0.0.1", 80); err == nil {
		t.Fatal("loopback destination should be denied")
	}
	if _, err := p.Allow(context.Background(), "tcp", "1.1.1.1", 22); err == nil {
		t.Fatal("disallowed port should be denied")
	}
	address, err := p.Allow(context.Background(), "tcp", "1.1.1.1", 443)
	if err != nil || address != "1.1.1.1:443" {
		t.Fatalf("public destination should be allowed: %q %v", address, err)
	}
}

func TestEgressPolicyRequiresNarrowSpecialUseException(t *testing.T) {
	if _, err := NewEgressPolicy(config.EgressPolicy{Enabled: true, TCPPorts: []string{"80"}, AllowSpecialCIDRs: []string{"0.0.0.0/0"}}); err == nil {
		t.Fatal("broad special-use exception should be rejected")
	}
	p, err := NewEgressPolicy(config.EgressPolicy{Enabled: true, TCPPorts: []string{"80"}, AllowSpecialCIDRs: []string{"127.0.0.1/32"}})
	if err != nil {
		t.Fatal(err)
	}
	address, err := p.Allow(context.Background(), "tcp", "127.0.0.1", 80)
	if err != nil || address != "127.0.0.1:80" || !p.IsSpecialException(address) {
		t.Fatalf("explicit loopback exception = %q %v", address, err)
	}
}

func TestEgressPolicyRejectsCanonicalSpecialUseRanges(t *testing.T) {
	p, err := NewEgressPolicy(config.EgressPolicy{Enabled: true, TCPPorts: []string{"443"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"100.64.0.1", "168.63.129.16", "192.0.2.1", "198.18.0.1", "2001:db8::1", "fc00::1"} {
		if _, err := p.Allow(context.Background(), "tcp", address, 443); err == nil {
			t.Fatalf("special-use address %s was allowed", address)
		}
	}
}

func TestEgressConnectionLimit(t *testing.T) {
	p, err := NewEgressPolicy(config.EgressPolicy{Enabled: true, TCPPorts: []string{"443"}, MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	release, ok := p.Acquire()
	if !ok {
		t.Fatal("first connection should be accepted")
	}
	defer release()
	if _, ok := p.Acquire(); ok {
		t.Fatal("second connection should be rejected")
	}
}
