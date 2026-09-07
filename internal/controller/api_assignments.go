package controller

import (
	"asterferry/internal/domain"
	"net/http"
	"strings"
)

func (s *Server) assignments(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		items, err := s.resources.ListAssignments(r.Context(), r.URL.Query().Get("gateway_id"), r.URL.Query().Get("agent_id"))
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
	var assignment domain.Assignment
	if err := decodeJSON(r, &assignment, 2<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.resources.PutAssignment(r.Context(), assignment, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	created, err := s.resources.GetAssignment(r.Context(), assignment.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, created.Revision)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) assignmentAction(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/assignments/"), "/")
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "assignment not found")
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		assignment, err := s.resources.GetAssignment(r.Context(), id)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, assignment.Revision)
		writeJSON(w, http.StatusOK, assignment)
		return
	}
	user, ok := s.authorize(w, r, RoleOperator)
	if !ok {
		return
	}
	expected, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.resources.DeleteAssignment(r.Context(), id, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
		return
	}
	var assignment domain.Assignment
	if err := decodeJSON(r, &assignment, 2<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	assignment.ID = id
	if err := s.resources.PutAssignment(r.Context(), assignment, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.resources.GetAssignment(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, updated.Revision)
	writeJSON(w, http.StatusOK, updated)
}
