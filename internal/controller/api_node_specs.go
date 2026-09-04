package controller

import (
	"errors"
	"net/http"
	"strings"

	"asterferry/internal/domain"
)

// nodeSpecAction is the unified behavior endpoint. A Node is created and
// enrolled independently; this resource is where an operator chooses whether
// that daemon currently runs Gateway or Agent behavior.
func (s *Server) nodeSpecAction(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		spec, err := s.resources.GetNodeSpec(r.Context(), nodeID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, spec.Revision)
		writeJSON(w, http.StatusOK, spec)
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
		if err := s.resources.DeleteNodeSpec(r.Context(), nodeID, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
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
	var spec domain.NodeSpec
	if err := decodeJSON(r, &spec, 4<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	spec.NodeID = nodeID
	if spec.Kind == "" {
		switch {
		case spec.Gateway != nil:
			spec.Kind = domain.NodeSpecGateway
		case spec.Agent != nil:
			spec.Kind = domain.NodeSpecAgent
		}
	}
	if spec.Gateway != nil {
		spec.Gateway.NodeID = nodeID
	}
	if spec.Agent != nil {
		spec.Agent.NodeID = nodeID
	}
	expected, err := parseOptionalIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
		return
	}
	if err := s.resources.PutNodeSpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.resources.GetNodeSpec(r.Context(), nodeID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, updated.Revision)
	status := http.StatusOK
	if expected == 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, updated)
}

func (s *Server) nodeEgressAction(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
	} else {
		if _, ok := s.authorize(w, r, RoleOperator); !ok {
			return
		}
	}
	spec, err := s.resources.GetNodeSpec(r.Context(), nodeID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	switch spec.Kind {
	case domain.NodeSpecGateway:
		s.gatewayEgressAction(w, r, nodeID)
	case domain.NodeSpecAgent:
		s.agentEgressAction(w, r, nodeID)
	default:
		writeError(w, http.StatusNotFound, "not_found", "node behavior is not configured")
	}
}

func (s *Server) nodeAgentSpecSubresource(w http.ResponseWriter, r *http.Request, nodeID, kind string, rest []string) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
	} else {
		if _, ok := s.authorize(w, r, RoleOperator); !ok {
			return
		}
	}
	spec, err := s.resources.GetNodeSpec(r.Context(), nodeID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if spec.Kind != domain.NodeSpecAgent || spec.Agent == nil {
		writeError(w, http.StatusNotFound, "not_found", "node behavior is not an agent")
		return
	}
	s.agentSpecSubresource(w, r, nodeID, kind, rest)
}

func parseOptionalIfMatch(value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	value = strings.Trim(strings.TrimSpace(value), "\"")
	if value == "" {
		return 0, errors.New("If-Match must be a positive revision")
	}
	return parseIfMatch(value)
}
