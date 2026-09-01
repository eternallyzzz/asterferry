package controller

import (
	"asterferry/internal/domain"
	"net/http"
	"strings"
)

func (s *Server) gateways(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		views, err := s.store.ListGatewayViews(r.Context())
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
	var spec domain.GatewaySpec
	if err := decodeJSON(r, &spec, 4<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.store.PutGatewaySpec(r.Context(), spec, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	created, err := s.store.GetGatewaySpec(r.Context(), spec.NodeID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, created.Revision)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) gatewayAction(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/gateways/"), "/")
	parts := strings.Split(path, "/")
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "gateway not found")
		return
	}
	if len(parts) == 2 && parts[1] == "egress" {
		s.gatewayEgressAction(w, r, id)
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not_found", "gateway resource was not found")
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		spec, err := s.store.GetGatewaySpec(r.Context(), id)
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
			var deleteErr error
			if id != "" {
				deleteErr = s.store.DeleteGatewaySpec(r.Context(), id, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
			}
			if deleteErr != nil {
				writeStoreError(w, deleteErr)
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
	var spec domain.GatewaySpec
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
	if err := s.store.PutGatewaySpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.store.GetGatewaySpec(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, updated.Revision)
	writeJSON(w, http.StatusOK, updated)
}

// gatewayEgressAction edits the singleton egress policy as a compare-and-swap
// subresource of GatewaySpec. Keeping the parent revision on the wire makes
// policy changes participate in the same optimistic-concurrency transaction
// as the rest of the typed Gateway document.
func (s *Server) gatewayEgressAction(w http.ResponseWriter, r *http.Request, gatewayID string) {
	spec, err := s.store.GetGatewaySpec(r.Context(), gatewayID)
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
	if err := s.store.PutGatewaySpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.store.GetGatewaySpec(r.Context(), gatewayID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, updated.Revision)
	writeJSON(w, http.StatusOK, updated.Egress)
}
