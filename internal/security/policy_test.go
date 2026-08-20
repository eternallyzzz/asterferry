package security

import (
	"context"
	"testing"

	"asterferry/internal/config"
)

func TestEgressPolicyFailsClosed(t *testing.T) {
	p, err := NewEgressPolicy(config.EgressPolicy{
		Enabled:             true,
		TCPPorts:            []string{"80", "443"},
		DenyPrivateNetworks: true,
		MaxConnections:      1,
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
