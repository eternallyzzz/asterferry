package controller

import (
	"asterferry/internal/domain"
	"net/http"
	"strings"
)

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		views, err := s.store.ListAgentViews(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(views))
		for _, view := range views {
			item := map[string]any{"node": view.Node}
			if view.Spec != nil {
				item["spec"] = view.Spec
			}
			items = append(items, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	user, ok := s.authorize(w, r, RoleOperator)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	var spec domain.AgentSpec
	if err := decodeJSON(r, &spec, 4<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.store.PutAgentSpec(r.Context(), spec, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	created, err := s.store.GetAgentSpec(r.Context(), spec.NodeID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, created.Revision)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) agentAction(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/agents/"), "/")
	parts := strings.Split(path, "/")
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	if len(parts) == 2 && parts[1] == "egress" {
		s.agentEgressAction(w, r, id)
		return
	}
	if len(parts) >= 2 && (parts[1] == "proxies" || parts[1] == "routes") {
		s.agentSpecSubresource(w, r, id, parts[1], parts[2:])
		return
	}
	if len(parts) == 3 && parts[1] == "actions" && r.Method == http.MethodPost {
		user, ok := s.authorize(w, r, RoleOperator)
		if !ok {
			return
		}
		if parts[2] != "schedule" {
			writeError(w, http.StatusBadRequest, "unknown_action", "unsupported agent action")
			return
		}
		assignment, err := s.store.ScheduleAgent(r.Context(), id, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, assignment)
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not_found", "agent resource was not found")
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		spec, err := s.store.GetAgentSpec(r.Context(), id)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, spec.Revision)
		writeJSON(w, http.StatusOK, spec)
		return
	}
	if r.Method != http.MethodPut {
		if r.Method == http.MethodDelete {
			user, ok := s.authorize(w, r, RoleOperator)
			if !ok {
				return
			}
			expected, err := parseIfMatch(r.Header.Get("If-Match"))
			if err != nil {
				writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
				return
			}
			if err := s.store.DeleteAgentSpec(r.Context(), id, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
				writeStoreError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
		return
	}
	user, ok := s.authorize(w, r, RoleOperator)
	if !ok {
		return
	}
	var spec domain.AgentSpec
	if err := decodeJSON(r, &spec, 4<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	spec.NodeID = id
	expected, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
		return
	}
	if err := s.store.PutAgentSpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.store.GetAgentSpec(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, updated.Revision)
	writeJSON(w, http.StatusOK, updated)
}

// agentEgressAction is the Agent counterpart to gatewayEgressAction. The
// complete AgentSpec remains the persisted resource; this endpoint only offers
// a narrow policy-shaped API for Dashboard and automation clients.
func (s *Server) agentEgressAction(w http.ResponseWriter, r *http.Request, agentID string) {
	spec, err := s.store.GetAgentSpec(r.Context(), agentID)
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, spec.Revision)
		writeJSON(w, http.StatusOK, spec.Egress)
		return
	}
	user, ok := s.authorize(w, r, RoleOperator)
	if !ok {
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodPatch)
		return
	}
	var policy domain.EgressPolicy
	if err := decodeJSON(r, &policy, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	spec.Egress = policy
	expected, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
		return
	}
	if err := s.store.PutAgentSpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.store.GetAgentSpec(r.Context(), agentID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, updated.Revision)
	writeJSON(w, http.StatusOK, updated.Egress)
}

// agentSpecSubresource exposes proxy entrances and route rules as compare-
// and-swap edits of the complete AgentSpec. The AgentSpec revision therefore
// protects the collection from lost concurrent updates.
func (s *Server) agentSpecSubresource(w http.ResponseWriter, r *http.Request, agentID, kind string, rest []string) {
	if len(rest) > 1 {
		writeError(w, http.StatusNotFound, "not_found", "agent subresource was not found")
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		spec, err := s.store.GetAgentSpec(r.Context(), agentID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, spec.Revision)
		if len(rest) == 0 {
			if kind == "proxies" {
				writeJSON(w, http.StatusOK, map[string]any{"items": spec.Proxies})
			} else {
				writeJSON(w, http.StatusOK, map[string]any{"items": spec.Routes})
			}
			return
		}
		if kind == "proxies" {
			for _, value := range spec.Proxies {
				if value.ID == rest[0] {
					writeJSON(w, http.StatusOK, value)
					return
				}
			}
		} else {
			for _, value := range spec.Routes {
				if value.Name == rest[0] {
					writeJSON(w, http.StatusOK, value)
					return
				}
			}
		}
		writeError(w, http.StatusNotFound, "not_found", "agent subresource was not found")
		return
	}
	user, ok := s.authorize(w, r, RoleOperator)
	if !ok {
		return
	}
	spec, err := s.store.GetAgentSpec(r.Context(), agentID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	expected, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
		return
	}
	key := ""
	if len(rest) == 1 {
		key = rest[0]
	}
	if r.Method == http.MethodPost && len(rest) == 0 {
		if kind == "proxies" {
			var value domain.ProxySpec
			if err := decodeJSON(r, &value, 1<<20); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			spec.Proxies = append(spec.Proxies, value)
			key = value.ID
		} else {
			var value domain.RouteRule
			if err := decodeJSON(r, &value, 1<<20); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			spec.Routes = append(spec.Routes, value)
			key = value.Name
		}
	} else if (r.Method == http.MethodPut || r.Method == http.MethodPatch) && len(rest) == 1 {
		if kind == "proxies" {
			var value domain.ProxySpec
			if err := decodeJSON(r, &value, 1<<20); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			value.ID = key
			found := false
			for i := range spec.Proxies {
				if spec.Proxies[i].ID == key {
					spec.Proxies[i], found = value, true
					break
				}
			}
			if !found {
				writeError(w, http.StatusNotFound, "not_found", "proxy was not found")
				return
			}
		} else {
			var value domain.RouteRule
			if err := decodeJSON(r, &value, 1<<20); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			value.Name = key
			found := false
			for i := range spec.Routes {
				if spec.Routes[i].Name == key {
					spec.Routes[i], found = value, true
					break
				}
			}
			if !found {
				writeError(w, http.StatusNotFound, "not_found", "route was not found")
				return
			}
		}
	} else if r.Method == http.MethodDelete && len(rest) == 1 {
		if kind == "proxies" {
			filtered := spec.Proxies[:0]
			for _, value := range spec.Proxies {
				if value.ID != key {
					filtered = append(filtered, value)
				}
			}
			if len(filtered) == len(spec.Proxies) {
				writeError(w, http.StatusNotFound, "not_found", "proxy was not found")
				return
			}
			spec.Proxies = filtered
		} else {
			filtered := spec.Routes[:0]
			for _, value := range spec.Routes {
				if value.Name != key {
					filtered = append(filtered, value)
				}
			}
			if len(filtered) == len(spec.Routes) {
				writeError(w, http.StatusNotFound, "not_found", "route was not found")
				return
			}
			spec.Routes = filtered
		}
	} else {
		methodNotAllowed(w, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete)
		return
	}
	if err := s.store.PutAgentSpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.store.GetAgentSpec(r.Context(), agentID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, updated.Revision)
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	if kind == "proxies" {
		for _, value := range updated.Proxies {
			if value.ID == key {
				writeJSON(w, status, value)
				return
			}
		}
	} else {
		for _, value := range updated.Routes {
			if value.Name == key {
				writeJSON(w, status, value)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, updated)
}
