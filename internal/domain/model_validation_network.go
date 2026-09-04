package domain

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

func validateListener(l Listener) error {
	if l.Protocol != ProtocolTCP && l.Protocol != ProtocolUDP {
		return errors.New("listener protocol must be tcp or udp")
	}
	if l.Port == 0 {
		return errors.New("listener port must be non-zero")
	}
	if strings.TrimSpace(l.Bind) == "" {
		return errors.New("listener bind is required")
	}
	if l.Bind != strings.TrimSpace(l.Bind) {
		return errors.New("listener bind must not contain surrounding whitespace")
	}
	if _, err := netip.ParseAddr(l.Bind); err != nil {
		return fmt.Errorf("listener bind must be an IP address: %w", err)
	}
	return nil
}

func normalizedIP(value string) string {
	value = strings.TrimSpace(value)
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Unmap().String()
	}
	return value
}

func validateEndpoint(value string) error {
	if value != strings.TrimSpace(value) {
		return errors.New("endpoint must not contain surrounding whitespace")
	}
	if value == "" || len(value) > 2048 {
		return errors.New("endpoint is empty or too long")
	}
	return validateHostPort(value)
}

func validateHostPort(value string) error {
	if containsControl(value) || len(value) > 2048 {
		return errors.New("host:port contains invalid characters")
	}
	if value != strings.TrimSpace(value) {
		return errors.New("host:port must not contain surrounding whitespace")
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return err
	}
	if strings.TrimSpace(host) == "" || host != strings.TrimSpace(host) || strings.ContainsAny(host, "\t ") || strings.ContainsAny(portText, "\t ") {
		return errors.New("host is required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func (p PortPool) Validate() error {
	for name, ranges := range map[string][]PortRange{"tcp": p.TCP, "udp": p.UDP} {
		for _, r := range ranges {
			if r.Min == 0 || r.Max < r.Min {
				return &ApplyError{Code: "invalid_port_pool", Path: "gateway.port_pool." + name, Message: "port range is invalid"}
			}
		}
		ordered := append([]PortRange(nil), ranges...)
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].Min != ordered[j].Min {
				return ordered[i].Min < ordered[j].Min
			}
			return ordered[i].Max < ordered[j].Max
		})
		for i := 1; i < len(ordered); i++ {
			if ordered[i].Min <= ordered[i-1].Max {
				return &ApplyError{Code: "invalid_port_pool", Path: "gateway.port_pool." + name, Message: "port ranges overlap"}
			}
		}
	}
	return nil
}

func ValidateID(value, path string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed != value {
		return &ApplyError{Code: "invalid_id", Path: path, Message: "id must not contain surrounding whitespace"}
	}
	value = trimmed
	if len(value) < 1 || len(value) > 128 {
		return &ApplyError{Code: "invalid_id", Path: path, Message: "id must contain 1 to 128 characters"}
	}
	for i, r := range value {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') || i == 0 && (r == '-' || r == '_' || r == '.') {
			return &ApplyError{Code: "invalid_id", Path: path, Message: "id contains an invalid character"}
		}
	}
	last := value[len(value)-1]
	if last == '-' || last == '_' || last == '.' {
		return &ApplyError{Code: "invalid_id", Path: path, Message: "id must not end with punctuation"}
	}
	return nil
}
