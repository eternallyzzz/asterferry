package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"asterferry/internal/addresspolicy"
	"asterferry/internal/afdp"
	"asterferry/internal/domain"
)

// AcquireEgress applies the Agent snapshot's outbound policy and reserves one
// connection slot. It returns the exact address that passed the policy; for a
// hostname this is a resolved IP, which prevents a second DNS lookup at dial
// time from bypassing CIDR rules. The release callback is safe to invoke more
// than once and must be called when the connection closes.
func (e *Engine) AcquireEgress(ctx context.Context, network, target string) (string, func(), error) {
	if e == nil {
		return "", func() {}, errors.New("data-plane engine is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if e.closed.Load() || e.draining.Load() {
		return "", func() {}, errors.New("data-plane engine is not accepting egress")
	}
	normalizedTarget, host, port, err := normalizeEgressTarget(network, target)
	if err != nil {
		return "", func() {}, err
	}
	e.mu.RLock()
	policy := cloneEgressPolicy(e.egress)
	e.mu.RUnlock()
	reserve := func() (func(), error) {
		if policy.MaxConnections <= 0 {
			return func() {}, nil
		}
		for {
			current := e.egressOpen.Load()
			if current >= int64(policy.MaxConnections) {
				return nil, fmt.Errorf("%w: egress connection limit reached", afdp.ErrTransient)
			}
			if e.egressOpen.CompareAndSwap(current, current+1) {
				var once sync.Once
				return func() { once.Do(func() { e.releaseEgress() }) }, nil
			}
		}
	}
	if !policy.Enabled {
		// Even an unrestricted policy must receive a syntactically valid target.
		// This keeps malformed metadata out of the socket layer and makes the
		// disabled-policy path obey the same fail-closed boundary as a filtered
		// policy, while retaining hostnames for normal direct DNS dialing.  A
		// standalone MaxConnections value still applies: disabling address
		// filtering must not silently remove an operator's resource limit.
		release, reserveErr := reserve()
		if reserveErr != nil {
			return "", func() {}, reserveErr
		}
		return normalizedTarget, release, nil
	}
	address, err := resolveAllowedEgress(ctx, policy, network, host, port)
	if err != nil {
		return "", func() {}, err
	}
	release, reserveErr := reserve()
	if reserveErr != nil {
		return "", func() {}, reserveErr
	}
	return address, release, nil
}

// ActiveEgress is included in observed metrics and is useful to admission
// tests without exposing the policy's internal semaphore.
func (e *Engine) ActiveEgress() int64 {
	if e == nil {
		return 0
	}
	return e.egressOpen.Load()
}

func (e *Engine) releaseEgress() {
	for {
		current := e.egressOpen.Load()
		if current <= 0 || e.egressOpen.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func resolveAllowedEgress(ctx context.Context, policy domain.EgressPolicy, network, host string, port int) (string, error) {
	if !egressPortAllowed(policy, network, port) {
		return "", fmt.Errorf("egress %s port %d is not allowed", network, port)
	}

	addresses := make([]netip.Addr, 0, 4)
	if literal, parseErr := netip.ParseAddr(strings.Trim(host, "[]")); parseErr == nil {
		addresses = append(addresses, literal.Unmap())
	} else {
		ips, lookupErr := net.DefaultResolver.LookupNetIP(ctx, "ip", strings.TrimSuffix(host, "."))
		if lookupErr != nil {
			return "", fmt.Errorf("resolve egress destination: %w", lookupErr)
		}
		// A resolver is allowed to return an arbitrary number of addresses. Do
		// not let a single hostname consume unbounded policy-checking or dial
		// work; callers can retry after DNS rotation if the set is larger.
		if len(ips) > 64 {
			return "", errors.New("egress destination resolves to too many addresses")
		}
		for _, ip := range ips {
			addresses = append(addresses, ip.Unmap())
		}
	}
	for _, address := range addresses {
		if !egressIPAllowed(policy, address) {
			continue
		}
		return net.JoinHostPort(address.String(), strconv.Itoa(port)), nil
	}
	return "", errors.New("egress destination is denied by policy")
}

// normalizeEgressTarget validates the network and host:port form before any
// policy shortcut is taken. It returns a canonical target plus the individual
// host/port components used by policy resolution. Hostnames are intentionally
// not resolved here when policy is disabled; direct egress may continue to
// follow the platform resolver at dial time.
func normalizeEgressTarget(network, target string) (normalized, host string, port int, err error) {
	if network != domain.ProtocolTCP && network != domain.ProtocolUDP {
		return "", "", 0, fmt.Errorf("unsupported egress network %q", network)
	}
	if strings.TrimSpace(target) != target || len(target) == 0 || len(target) > 2048 || strings.ContainsAny(target, "\x00\r\n") {
		return "", "", 0, errors.New("egress target must be host:port")
	}
	host, portText, splitErr := net.SplitHostPort(target)
	if splitErr != nil || strings.TrimSpace(host) == "" || host != strings.TrimSpace(host) || strings.ContainsAny(host, "\x00\r\n \t") || strings.ContainsAny(portText, "\x00\r\n \t") {
		return "", "", 0, errors.New("egress target must be host:port")
	}
	port, err = strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", "", 0, errors.New("egress target port is invalid")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), host, port, nil
}

func egressPortAllowed(policy domain.EgressPolicy, network string, port int) bool {
	values := policy.TCPPorts
	if network == domain.ProtocolUDP {
		values = policy.UDPPorts
	}
	for _, value := range values {
		parts := strings.Split(strings.TrimSpace(value), "-")
		if len(parts) < 1 || len(parts) > 2 {
			continue
		}
		min, minErr := strconv.Atoi(strings.TrimSpace(parts[0]))
		max := min
		if len(parts) == 2 {
			max, minErr = strconv.Atoi(strings.TrimSpace(parts[1]))
		}
		if minErr == nil && min >= 1 && max <= 65535 && min <= max && port >= min && port <= max {
			return true
		}
	}
	return false
}

func egressIPAllowed(policy domain.EgressPolicy, address netip.Addr) bool {
	address = address.Unmap()
	if addresspolicy.IsSpecial(address) && !containsEgressPrefix(policy.AllowSpecialCIDRs, address) {
		return false
	}
	if len(policy.AllowCIDRs) == 0 {
		return true
	}
	for _, value := range policy.AllowCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err == nil && prefix.Contains(address) {
			return true
		}
	}
	return false
}

func containsEgressPrefix(values []string, address netip.Addr) bool {
	for _, value := range values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err == nil && prefix.Contains(address) {
			return true
		}
	}
	return false
}
