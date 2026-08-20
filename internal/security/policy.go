package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"asterferry/internal/config"
)

var ErrDenied = errors.New("egress destination denied")

// EgressPolicy is the compiled, fail-closed policy for one agent. It resolves
// hostnames itself so the address checked by the policy is the address that is
// subsequently dialed, preventing DNS rebinding from bypassing CIDR rules.
type EgressPolicy struct {
	tcp            []config.PortRange
	udp            []config.PortRange
	allow          []*net.IPNet
	denyPrivate    bool
	maxConnections int64
	semaphore      chan struct{}
}

func NewEgressPolicy(raw config.EgressPolicy) (*EgressPolicy, error) {
	if !raw.Enabled {
		return &EgressPolicy{}, nil
	}
	tcp, err := config.ParsePortRanges(raw.TCPPorts)
	if err != nil {
		return nil, err
	}
	udp, err := config.ParsePortRanges(raw.UDPPorts)
	if err != nil {
		return nil, err
	}
	p := &EgressPolicy{tcp: tcp, udp: udp, denyPrivate: raw.DenyPrivateNetworks, maxConnections: raw.MaxConnections}
	for _, value := range raw.AllowCIDRs {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("parse egress CIDR %q: %w", value, err)
		}
		p.allow = append(p.allow, network)
	}
	if p.maxConnections > 0 {
		p.semaphore = make(chan struct{}, p.maxConnections)
	}
	return p, nil
}

func (p *EgressPolicy) Enabled() bool {
	return p != nil && (len(p.tcp) > 0 || len(p.udp) > 0)
}

func (p *EgressPolicy) Allow(ctx context.Context, network, host string, port uint16) (string, error) {
	if p == nil || !p.Enabled() {
		return "", ErrDenied
	}
	if network != "tcp" && network != "udp" {
		return "", fmt.Errorf("unsupported egress network %q", network)
	}
	ranges := p.tcp
	if network == "udp" {
		ranges = p.udp
	}
	allowedPort := false
	for _, r := range ranges {
		if r.Contains(port) {
			allowedPort = true
			break
		}
	}
	if !allowedPort {
		return "", fmt.Errorf("egress %s port %d is not allowed", network, port)
	}
	if strings.TrimSpace(host) == "" {
		return "", ErrDenied
	}
	ipList := []net.IP{net.ParseIP(strings.TrimSpace(host))}
	if ipList[0] == nil {
		if ctx == nil {
			ctx = context.Background()
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", strings.TrimSuffix(host, "."))
		if err != nil {
			return "", fmt.Errorf("resolve egress destination: %w", err)
		}
		ipList = ips
	}
	for _, ip := range ipList {
		if ip == nil || !p.allowedIP(ip) {
			continue
		}
		return net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), nil
	}
	return "", ErrDenied
}

func (p *EgressPolicy) allowedIP(ip net.IP) bool {
	if p.denyPrivate && isPrivateOrLocal(ip) {
		return false
	}
	if len(p.allow) == 0 {
		return true
	}
	for _, network := range p.allow {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func isPrivateOrLocal(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("100.100.100.200")) || ip.Equal(net.ParseIP("fd00:ec2::254"))
}

// Acquire reserves one egress connection. The returned function is
// idempotent by convention and should be deferred by the caller.
func (p *EgressPolicy) Acquire() (func(), bool) {
	if p == nil || !p.Enabled() {
		return func() {}, false
	}
	if p.semaphore == nil {
		return func() {}, true
	}
	select {
	case p.semaphore <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-p.semaphore }) }, true
	default:
		return func() {}, false
	}
}
