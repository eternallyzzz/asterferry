package node

import (
	"asterferry/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

type runtimeSelector struct {
	ConnectionID string `json:"connection_id,omitempty"`
	SourceIP     string `json:"source_ip,omitempty"`
	PeerNodeID   string `json:"peer_node_id,omitempty"`
	AssignmentID string `json:"assignment_id,omitempty"`
	ServiceID    string `json:"service_id,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
}

type runtimeActionRequest struct {
	Action         string          `json:"action"`
	Selector       runtimeSelector `json:"selector"`
	ConnectionID   string          `json:"connection_id,omitempty"`
	Direction      string          `json:"direction,omitempty"`
	BytesPerSecond uint64          `json:"bytes_per_second,omitempty"`
	BurstBytes     uint64          `json:"burst_bytes,omitempty"`
	TTLSeconds     int             `json:"ttl_seconds,omitempty"`
}

func (t *runtimeTelemetry) applyAction(ctx context.Context, name string, payload []byte) (int, error) {
	if t == nil {
		return 0, errors.New("runtime telemetry is unavailable")
	}
	var request runtimeActionRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &request); err != nil {
			return 0, errors.New("runtime action payload is invalid")
		}
	}
	if request.Action == "" {
		request.Action = name
	}
	if request.Action == "clear_runtime_controls" {
		t.mu.Lock()
		entries := make([]*runtimeConnection, 0, len(t.connections))
		for _, entry := range t.connections {
			entries = append(entries, entry)
		}
		t.mu.Unlock()
		for _, entry := range entries {
			entry.clearLimit()
		}
		return len(entries), nil
	}
	if request.ConnectionID != "" && request.Selector.ConnectionID == "" {
		request.Selector.ConnectionID = request.ConnectionID
	}
	if request.Action != "disconnect" && request.Action != "rate_limit" && request.Action != "clear_limit" {
		return 0, errors.New("runtime connection action is unsupported")
	}
	if request.Action == "rate_limit" {
		if request.Direction == "" {
			request.Direction = "both"
		}
		if request.BytesPerSecond == 0 || request.BurstBytes == 0 {
			return 0, errors.New("runtime rate limit requires bytes_per_second and burst_bytes")
		}
		if request.TTLSeconds <= 0 {
			request.TTLSeconds = int(defaultRuntimeLimitTTL / time.Second)
		}
		if request.TTLSeconds > 24*60*60 {
			return 0, errors.New("runtime rate limit ttl is too long")
		}
	}
	if request.Selector.SourceIP != "" {
		request.Selector.SourceIP = normalizedRuntimeIP(request.Selector.SourceIP)
	}
	entries := t.match(request.Selector)
	if len(entries) > runtimeActionMaxMatches {
		return 0, errors.New("runtime action matches too many connections")
	}
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		switch request.Action {
		case "disconnect":
			entry.close(domain.RuntimeCloseOperator)
		case "clear_limit":
			entry.clearLimit()
		case "rate_limit":
			expires := time.Now().UTC().Add(time.Duration(request.TTLSeconds) * time.Second)
			entry.setLimit(domain.RuntimeRateLimit{Direction: request.Direction, BytesPerSecond: request.BytesPerSecond, BurstBytes: request.BurstBytes, ExpiresAt: expires})
			t.recordLimitEvent(entry)
		}
	}
	return len(entries), nil
}

func (t *runtimeTelemetry) match(selector runtimeSelector) []*runtimeConnection {
	t.mu.Lock()
	entries := make([]*runtimeConnection, 0, len(t.connections))
	for _, entry := range t.connections {
		if entry.matches(selector) {
			entries = append(entries, entry)
		}
	}
	t.mu.Unlock()
	return entries
}

func (e *runtimeConnection) matches(selector runtimeSelector) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	value := e.meta
	return (selector.ConnectionID == "" || value.ID == selector.ConnectionID) &&
		(selector.SourceIP == "" || value.SourceIP == selector.SourceIP) &&
		(selector.PeerNodeID == "" || value.PeerNodeID == selector.PeerNodeID) &&
		(selector.AssignmentID == "" || value.AssignmentID == selector.AssignmentID) &&
		(selector.ServiceID == "" || value.ServiceID == selector.ServiceID) &&
		(selector.Protocol == "" || value.Protocol == selector.Protocol)
}

func (t *runtimeTelemetry) recordLimitEvent(entry *runtimeConnection) {
	if t == nil || entry == nil {
		return
	}
	value, ok := entry.snapshot()
	if !ok {
		return
	}
	t.mu.Lock()
	t.rateLimited++
	t.appendEventLocked(domain.RuntimeEvent{ID: runtimeConnectionID(), Type: domain.RuntimeEventRateLimited, NodeID: value.NodeID, ConnectionID: value.ID, Connection: &value, CreatedAt: time.Now().UTC()})
	t.mu.Unlock()
}

func normalizedRuntimeIP(value string) string {
	address, err := netipParse(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return address
}

// netipParse is kept tiny so the selector code does not accept arbitrary
// strings as an IP while avoiding a second copy of domain's internal helper.
func netipParse(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := netip.ParseAddr(value)
	if err != nil {
		return "", err
	}
	return parsed.Unmap().String(), nil
}

func runtimeAddr(addr net.Addr) (string, uint16) {
	if addr == nil {
		return "", 0
	}
	host, portText, err := net.SplitHostPort(addr.String())
	if err != nil {
		return normalizedRuntimeIP(addr.String()), 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return normalizedRuntimeIP(host), 0
	}
	return normalizedRuntimeIP(host), uint16(port)
}
