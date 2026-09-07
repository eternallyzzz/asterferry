package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"asterferry/internal/domain"
)

type runtimeSelectorInput struct {
	ConnectionID string `json:"connection_id,omitempty"`
	SourceIP     string `json:"source_ip,omitempty"`
	PeerNodeID   string `json:"peer_node_id,omitempty"`
	AssignmentID string `json:"assignment_id,omitempty"`
	ServiceID    string `json:"service_id,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
}

type runtimeActionInput struct {
	Action         string               `json:"action"`
	Selector       runtimeSelectorInput `json:"selector"`
	Direction      string               `json:"direction,omitempty"`
	BytesPerSecond uint64               `json:"bytes_per_second,omitempty"`
	BurstBytes     uint64               `json:"burst_bytes,omitempty"`
	TTLSeconds     int                  `json:"ttl_seconds,omitempty"`
}

func (s *Server) runtimeSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		enabled, err := s.resources.AdvancedOperationsEnabled(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"advanced_operations_enabled": enabled, "runtime_retention_days": 30})
	case http.MethodPut:
		user, ok := s.authorize(w, r, RoleAdmin)
		if !ok {
			return
		}
		var input struct {
			Enabled *bool `json:"advanced_operations_enabled"`
		}
		if err := decodeJSON(r, &input, 16<<10); err != nil || input.Enabled == nil {
			if err == nil {
				err = errors.New("advanced_operations_enabled is required")
			}
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err := s.resources.SetAdvancedOperationsEnabled(r.Context(), *input.Enabled, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
			writeStoreError(w, err)
			return
		}
		if !*input.Enabled {
			s.broadcastRuntimeControlClear(r, user.Username)
		}
		writeJSON(w, http.StatusOK, map[string]any{"advanced_operations_enabled": *input.Enabled, "runtime_retention_days": 30})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

func (s *Server) broadcastRuntimeControlClear(r *http.Request, actor string) {
	nodes, err := s.resources.ListNodes(r.Context(), "")
	if err != nil {
		return
	}
	for _, node := range nodes {
		if _, err := s.resources.PublishAction(r.Context(), node.ID, "clear_runtime_controls", `{}`); err != nil {
			_ = s.resources.RecordEvent(r.Context(), actor, "", "runtime_control_clear_failed", "could not clear runtime controls immediately", node.ID, map[string]string{"action": "clear_runtime_controls"})
		}
	}
}

func (s *Server) runtimeConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if _, ok := s.authorize(w, r, RoleViewer); !ok {
		return
	}
	filter, err := runtimeFilterFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	items, err := s.runtime.ListRuntimeConnections(r.Context(), filter)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) runtimeEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if _, ok := s.authorize(w, r, RoleViewer); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.runtime.ListRuntimeEvents(r.Context(), r.URL.Query().Get("node_id"), limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) runtimeTraffic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if _, ok := s.authorize(w, r, RoleViewer); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.runtime.ListRuntimeTraffic(r.Context(), r.URL.Query().Get("node_id"), limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) runtimeStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, RoleViewer); !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream_unsupported", "runtime event streaming is unavailable")
		return
	}
	nodeID := strings.TrimSpace(r.URL.Query().Get("node_id"))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	writeSSE := func(event string, value any) error {
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := writeSSE("ready", map[string]any{"node_id": nodeID}); err != nil {
		return
	}
	changes, unsubscribe := s.changes.SubscribeRuntimeChanges()
	defer unsubscribe()
	keepalive := time.NewTicker(10 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case changed, ok := <-changes:
			if !ok {
				return
			}
			if nodeID != "" && changed != "" && changed != nodeID {
				continue
			}
			if err := writeSSE("runtime", map[string]string{"node_id": changed}); err != nil {
				return
			}
		case <-keepalive.C:
			if err := writeSSE("keepalive", map[string]any{"at": time.Now().UTC()}); err != nil {
				return
			}
		}
	}
}

func (s *Server) nodeRuntimeAction(w http.ResponseWriter, r *http.Request, nodeID string, parts []string) {
	if len(parts) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "runtime resource was not found")
		return
	}
	if parts[0] == "connections" {
		switch {
		case len(parts) == 1 && r.Method == http.MethodGet:
			if _, ok := s.authorize(w, r, RoleViewer); !ok {
				return
			}
			filter, err := runtimeFilterFromQuery(r)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			filter.NodeID = nodeID
			items, err := s.runtime.ListRuntimeConnections(r.Context(), filter)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": items})
			return
		case len(parts) == 2 && r.Method == http.MethodGet:
			if _, ok := s.authorize(w, r, RoleViewer); !ok {
				return
			}
			item, err := s.runtime.GetRuntimeConnection(r.Context(), nodeID, parts[1])
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, item)
			return
		case len(parts) == 3 && parts[2] == "actions" && r.Method == http.MethodPost:
			var input runtimeActionInput
			submitRuntimeAction(s, w, r, nodeID, parts[1], &input, true)
			return
		}
	}
	if len(parts) == 1 && parts[0] == "actions" && r.Method == http.MethodPost {
		var input runtimeActionInput
		submitRuntimeAction(s, w, r, nodeID, "", &input, false)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "runtime resource was not found")
}

func submitRuntimeAction(s *Server, w http.ResponseWriter, r *http.Request, nodeID, connectionID string, input *runtimeActionInput, forceConnection bool) {
	user, ok := s.authorize(w, r, RoleOperator)
	if !ok {
		return
	}
	enabled, err := s.resources.AdvancedOperationsEnabled(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !enabled {
		writeError(w, http.StatusForbidden, "advanced_operations_disabled", "advanced runtime operations are disabled")
		return
	}
	if err := decodeJSON(r, input, 32<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	payload, err := validateRuntimeActionInput(*input, connectionID, forceConnection)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	delivered, err := s.resources.RequestNodeAction(r.Context(), nodeID, "runtime_connection", payload, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	state := "queued"
	if delivered {
		state = "delivered"
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"node_id": nodeID, "action": input.Action, "state": state})
}

func runtimeFilterFromQuery(r *http.Request) (RuntimeConnectionFilter, error) {
	query := r.URL.Query()
	filter := RuntimeConnectionFilter{NodeID: strings.TrimSpace(query.Get("node_id")), State: strings.TrimSpace(query.Get("state")), Type: strings.TrimSpace(query.Get("type")), SourceIP: strings.TrimSpace(query.Get("source_ip")), PeerNodeID: strings.TrimSpace(query.Get("peer_node_id")), GatewayID: strings.TrimSpace(query.Get("gateway_id")), AgentID: strings.TrimSpace(query.Get("agent_id")), AssignmentID: strings.TrimSpace(query.Get("assignment_id")), ServiceID: strings.TrimSpace(query.Get("service_id")), Protocol: strings.TrimSpace(query.Get("protocol"))}
	if filter.SourceIP != "" {
		address, err := netip.ParseAddr(filter.SourceIP)
		if err != nil {
			return RuntimeConnectionFilter{}, errors.New("source_ip must be an IP address")
		}
		filter.SourceIP = address.Unmap().String()
	}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 1000 {
			return RuntimeConnectionFilter{}, errors.New("limit must be between 1 and 1000")
		}
		filter.Limit = limit
	}
	return filter, nil
}

func validateRuntimeActionInput(input runtimeActionInput, connectionID string, forceConnection bool) (string, error) {
	if input.Action != "disconnect" && input.Action != "rate_limit" && input.Action != "clear_limit" {
		return "", errors.New("action must be disconnect, rate_limit or clear_limit")
	}
	if forceConnection {
		input.Selector.ConnectionID = connectionID
	}
	if input.Selector.ConnectionID == "" && input.Selector.SourceIP == "" && input.Selector.PeerNodeID == "" && input.Selector.AssignmentID == "" && input.Selector.ServiceID == "" && input.Selector.Protocol == "" {
		return "", errors.New("a runtime selector is required")
	}
	for name, value := range map[string]string{"connection_id": input.Selector.ConnectionID, "peer_node_id": input.Selector.PeerNodeID, "assignment_id": input.Selector.AssignmentID, "service_id": input.Selector.ServiceID} {
		if value != "" {
			if err := domain.ValidateID(value, "selector."+name); err != nil {
				return "", err
			}
		}
	}
	if input.Selector.SourceIP != "" {
		address, err := netip.ParseAddr(input.Selector.SourceIP)
		if err != nil {
			return "", errors.New("selector.source_ip must be an IP address")
		}
		input.Selector.SourceIP = address.Unmap().String()
	}
	if input.Selector.Protocol != "" && input.Selector.Protocol != domain.ProtocolTCP && input.Selector.Protocol != domain.ProtocolUDP && input.Selector.Protocol != "quic" {
		return "", errors.New("selector.protocol is invalid")
	}
	if input.Action == "rate_limit" {
		if input.Direction == "" {
			input.Direction = "both"
		}
		if input.Direction != "in" && input.Direction != "out" && input.Direction != "both" {
			return "", errors.New("direction must be in, out or both")
		}
		if input.BytesPerSecond == 0 || input.BytesPerSecond > 1<<40 || input.BurstBytes == 0 || input.BurstBytes > 1<<40 {
			return "", errors.New("rate limit values are out of range")
		}
		if input.TTLSeconds <= 0 {
			input.TTLSeconds = 3600
		}
		if input.TTLSeconds > 86400 {
			return "", errors.New("ttl_seconds must be at most 86400")
		}
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}
