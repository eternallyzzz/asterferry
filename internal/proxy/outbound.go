// Package proxy contains the small protocol-independent boundaries used by
// local proxy handlers. It intentionally does not know about QUIC, routing
// configuration, or the Agent type.
package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
)

type Path string

const (
	PathDirect  Path = "direct"
	PathGateway Path = "gateway"
)

type Target struct {
	Network     string
	Host        string
	Port        uint16
	ResolvedIP  netip.Addr
	ResolvedIPs []netip.Addr
}

func (t Target) Address() string {
	if candidates := t.CandidateAddresses(); len(candidates) > 0 {
		return net.JoinHostPort(candidates[0], strconv.Itoa(int(t.Port)))
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
}

func (t Target) CandidateAddresses() []string {
	result := make([]string, 0, len(t.ResolvedIPs)+1)
	seen := make(map[string]struct{}, cap(result))
	appendAddress := func(addr netip.Addr) {
		if !addr.IsValid() {
			return
		}
		value := addr.Unmap().String()
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	for _, addr := range t.ResolvedIPs {
		appendAddress(addr)
	}
	appendAddress(t.ResolvedIP)
	return result
}

// Outbound is the boundary between local proxy protocol handling and the
// selected egress path. Stream and datagram openings are separate because
// datagrams use the AsterFerry frame protocol while streams use relay records.
type Outbound interface {
	OpenStream(context.Context, Target, Path) (io.ReadWriteCloser, error)
	OpenDatagram(context.Context, Target, Path) (io.ReadWriteCloser, error)
}

func ValidatePath(path Path) error {
	if path != PathDirect && path != PathGateway {
		return fmt.Errorf("unsupported proxy path %q", path)
	}
	return nil
}
