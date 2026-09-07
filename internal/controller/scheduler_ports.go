package controller

import (
	"asterferry/internal/domain"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

type PortConflictError struct {
	GatewayID string
	Protocol  string
	Bind      string
	Port      uint16
}

func (e *PortConflictError) Error() string {
	return fmt.Sprintf("public %s binding %s:%d is already allocated on gateway %s", e.Protocol, e.Bind, e.Port, e.GatewayID)
}

func allocatePort(pool domain.PortPool, protocol, bind string, requested uint16, used map[string]struct{}) (uint16, error) {
	if requested != 0 {
		key := bindingKey(protocol, bind, requested)
		if _, exists := used[key]; exists {
			return 0, &PortConflictError{Protocol: protocol, Bind: bind, Port: requested}
		}
		if !portInPool(pool, protocol, requested) {
			return 0, fmt.Errorf("requested port %d is not in gateway %s port pool", requested, protocol)
		}
		return requested, nil
	}
	var ranges []domain.PortRange
	if protocol == domain.ProtocolTCP {
		ranges = pool.TCP
	} else {
		ranges = pool.UDP
	}
	for _, r := range ranges {
		for port := uint32(r.Min); port <= uint32(r.Max); port++ {
			candidate := uint16(port)
			if _, exists := used[bindingKey(protocol, bind, candidate)]; !exists {
				return candidate, nil
			}
		}
	}
	return 0, errors.New("gateway port pool is exhausted")
}

func portInPool(pool domain.PortPool, protocol string, port uint16) bool {
	var ranges []domain.PortRange
	if protocol == domain.ProtocolTCP {
		ranges = pool.TCP
	} else {
		ranges = pool.UDP
	}
	if len(ranges) == 0 {
		return true
	}
	for _, r := range ranges {
		if port >= r.Min && port <= r.Max {
			return true
		}
	}
	return false
}

func bindingKey(protocol, bind string, port uint16) string {
	bind = strings.TrimSpace(bind)
	if address, err := netip.ParseAddr(bind); err == nil {
		bind = address.Unmap().String()
	}
	return fmt.Sprintf("%s|%s|%d", protocol, bind, port)
}

func cloneBindingSet(source map[string]struct{}) map[string]struct{} {
	if source == nil {
		return nil
	}
	result := make(map[string]struct{}, len(source))
	for key := range source {
		result[key] = struct{}{}
	}
	return result
}
