package controller

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"asterferry/internal/domain"
)

// nodeInstallations is the Dashboard-facing lifecycle for nodes that have
// not enrolled yet. Keeping this resource separate from /nodes makes the
// distinction visible to API clients as well as to the UI.
func (s *Server) nodeInstallations(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		items, err := s.resources.ListPendingNodeBootstraps(r.Context())
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
	user, ok := s.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	var input NodeInstallationRequest
	if err := decodeJSON(r, &input, 4<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	platform, arch, err := normalizeBootstrapPlatform(input.Platform, input.Arch)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	node := domain.Node{ID: strings.TrimSpace(input.NodeID), Name: strings.TrimSpace(input.Name), Labels: input.Labels, Enabled: enabled}
	if err := node.Validate(); err != nil {
		writeStoreError(w, err)
		return
	}
	if _, _, err := validateBootstrapConfiguration(s.config); err != nil {
		writeError(w, http.StatusServiceUnavailable, "bootstrap_unavailable", err.Error())
		return
	}

	var spec *domain.NodeSpec
	if input.Spec != nil {
		value := *input.Spec
		value.NodeID = node.ID
		if value.Gateway != nil {
			value.Gateway.NodeID = node.ID
		}
		if value.Agent != nil {
			value.Agent.NodeID = node.ID
		}
		if value.Kind == "" {
			if value.Gateway != nil {
				value.Kind = domain.NodeSpecGateway
			} else if value.Agent != nil {
				value.Kind = domain.NodeSpecAgent
			}
		}
		switch value.Kind {
		case domain.NodeSpecGateway:
			if value.Gateway == nil {
				writeError(w, http.StatusBadRequest, "gateway_spec_required", "gateway spec is required for a gateway node spec")
				return
			}
			spec = &value
		case domain.NodeSpecAgent:
			if value.Agent == nil {
				writeError(w, http.StatusBadRequest, "agent_spec_required", "agent spec is required for an agent node spec")
				return
			}
			spec = &value
		default:
			writeError(w, http.StatusBadRequest, "invalid_spec_kind", "spec kind must be gateway or agent")
			return
		}
	}

	plain, pending, err := s.resources.CreatePendingNodeBootstrap(r.Context(), node, platform, arch, spec, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		if errors.Is(err, ErrSecretAlreadyCreated) {
			writeAlreadyCreatedSecret(w, "installation", pending)
			return
		}
		writeStoreError(w, err)
		return
	}
	response, err := buildNodeInstallCommand(s.config, node, platform, arch, plain, nil)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "bootstrap_unavailable", err.Error())
		return
	}
	response.InstallationID = pending.NodeID
	response.State = "pending"
	response.ExpiresAt = pending.ExpiresAt.UTC().Format(time.RFC3339Nano)
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) nodeInstallationAction(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/node-installations/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		writeError(w, http.StatusNotFound, "not_found", "pending node installation was not found")
		return
	}
	user, ok := s.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	nodeID := parts[0]
	if len(parts) == 2 && parts[1] == "reissue" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if _, _, err := validateBootstrapConfiguration(s.config); err != nil {
			writeError(w, http.StatusServiceUnavailable, "bootstrap_unavailable", err.Error())
			return
		}
		plain, pending, err := s.resources.ReissuePendingNodeBootstrap(r.Context(), nodeID, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
		if err != nil {
			if errors.Is(err, ErrSecretAlreadyCreated) {
				writeAlreadyCreatedSecret(w, "installation", pending)
				return
			}
			writeStoreError(w, err)
			return
		}
		response, err := buildNodeInstallCommand(s.config, pending.node(), pending.Platform, pending.Arch, plain, nil)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "bootstrap_unavailable", err.Error())
			return
		}
		response.InstallationID = pending.NodeID
		response.State = "pending"
		response.ExpiresAt = pending.ExpiresAt.UTC().Format(time.RFC3339Nano)
		writeJSON(w, http.StatusOK, response)
		return
	}
	if len(parts) != 1 || r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	if err := s.resources.DeletePendingNodeBootstrap(r.Context(), nodeID, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
