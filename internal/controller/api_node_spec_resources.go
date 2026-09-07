package controller

import (
	"net/http"

	"asterferry/internal/domain"
)

// gatewayEgressAction edits the singleton egress policy as a compare-and-swap
// subresource of the gateway branch of NodeSpec. The parent revision on the
// wire makes policy changes participate in the same optimistic-concurrency
// transaction as the complete typed document.
func (s *Server) gatewayEgressAction(w http.ResponseWriter, r *http.Request, gatewayID string) {
	spec, err := s.resources.GetGatewaySpec(r.Context(), gatewayID)
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
	if err := s.resources.PutGatewaySpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.resources.GetGatewaySpec(r.Context(), gatewayID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, updated.Revision)
	writeJSON(w, http.StatusOK, updated.Egress)
}

// agentEgressAction is the agent counterpart to gatewayEgressAction. The
// complete AgentSpec remains the persisted resource; this endpoint only offers
// a narrow policy-shaped API for Dashboard and automation clients.
func (s *Server) agentEgressAction(w http.ResponseWriter, r *http.Request, agentID string) {
	spec, err := s.resources.GetAgentSpec(r.Context(), agentID)
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
	if err := s.resources.PutAgentSpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.resources.GetAgentSpec(r.Context(), agentID)
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
		writeError(w, http.StatusNotFound, "not_found", "node spec subresource was not found")
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		spec, err := s.resources.GetAgentSpec(r.Context(), agentID)
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
		writeError(w, http.StatusNotFound, "not_found", "node spec subresource was not found")
		return
	}
	user, ok := s.authorize(w, r, RoleOperator)
	if !ok {
		return
	}
	spec, err := s.resources.GetAgentSpec(r.Context(), agentID)
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
	if err := s.resources.PutAgentSpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.resources.GetAgentSpec(r.Context(), agentID)
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
