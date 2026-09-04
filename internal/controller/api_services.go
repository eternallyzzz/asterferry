package controller

import (
	"asterferry/internal/domain"
	"net/http"
	"strings"
)

func (s *Server) services(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		items, err := s.resources.ListServices(r.Context(), r.URL.Query().Get("agent_id"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	user, ok := s.authorize(w, r, RoleOperator)
	if !ok {
		return
	}
	var service domain.Service
	if err := decodeJSON(r, &service, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.resources.PutService(r.Context(), service, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	created, err := s.resources.GetService(r.Context(), service.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, created.Revision)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) serviceAction(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/services/"), "/")
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "service not found")
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		item, err := s.resources.GetService(r.Context(), id)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, item.Revision)
		writeJSON(w, http.StatusOK, item)
		return
	}
	user, ok := s.authorize(w, r, RoleOperator)
	if !ok {
		return
	}
	if r.Method == http.MethodDelete {
		expected, err := parseIfMatch(r.Header.Get("If-Match"))
		if err != nil {
			writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
			return
		}
		if err := s.resources.DeleteService(r.Context(), id, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPatch {
		methodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
		return
	}
	var input struct {
		AgentID         *string          `json:"agent_id"`
		Protocol        *string          `json:"protocol"`
		LocalTarget     *string          `json:"local_target"`
		PublicBind      *string          `json:"public_bind"`
		PublicPort      *uint16          `json:"public_port"`
		GatewaySelector *domain.Selector `json:"gateway_selector"`
		Enabled         *bool            `json:"enabled"`
	}
	if err := decodeJSON(r, &input, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	service, getErr := s.resources.GetService(r.Context(), id)
	if getErr != nil {
		writeStoreError(w, getErr)
		return
	}
	if input.AgentID != nil {
		service.AgentID = *input.AgentID
	}
	if input.Protocol != nil {
		service.Protocol = *input.Protocol
	}
	if input.LocalTarget != nil {
		service.LocalTarget = *input.LocalTarget
	}
	if input.PublicBind != nil {
		service.PublicBind = *input.PublicBind
	}
	if input.PublicPort != nil {
		service.PublicPort = *input.PublicPort
	}
	if input.GatewaySelector != nil {
		service.GatewaySelector = *input.GatewaySelector
	}
	if input.Enabled != nil {
		service.Enabled = *input.Enabled
	}
	expected, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
		return
	}
	if err := s.resources.PutService(r.Context(), service, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, readErr := s.resources.GetService(r.Context(), id)
	if readErr != nil {
		writeStoreError(w, readErr)
		return
	}
	setETag(w, updated.Revision)
	writeJSON(w, http.StatusOK, updated)
}
