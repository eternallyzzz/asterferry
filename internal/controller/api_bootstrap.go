package controller

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"asterferry/internal/domain"
)

func (s *Server) nodeBootstrap(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	user, ok := s.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	var input NodeBootstrapRequest
	if err := decodeJSON(r, &input, 4<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	platform, arch, err := normalizeBootstrapPlatform(input.Platform, input.Arch)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	node, err := s.store.GetNode(r.Context(), nodeID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !node.Enabled {
		writeStoreError(w, &domain.ApplyError{Code: "node_disabled", Path: "node", Message: "disabled nodes cannot be provisioned"})
		return
	}
	_, caPEM, err := validateBootstrapConfiguration(s.config)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "bootstrap_unavailable", err.Error())
		return
	}
	if err := s.ensureBootstrapSpec(r, node, input, user.Username); err != nil {
		writeStoreError(w, err)
		return
	}
	plain, token, err := s.store.CreateNodeEnrollmentTokenWithOptions(r.Context(), node.ID, EnrollmentTTL, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		if errors.Is(err, ErrSecretAlreadyCreated) {
			writeAlreadyCreatedSecret(w, "token_metadata", token)
			return
		}
		writeStoreError(w, err)
		return
	}
	response, err := buildNodeInstallCommand(s.config, node, platform, arch, plain, caPEM)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "bootstrap_unavailable", err.Error())
		return
	}
	response.ExpiresAt = token.ExpiresAt.UTC().Format(time.RFC3339Nano)
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) ensureBootstrapSpec(r *http.Request, node domain.Node, input NodeBootstrapRequest, actor string) error {
	if _, err := s.store.GetNodeSpec(r.Context(), node.ID); err == nil {
		// Existing behavior is never replaced by an install/bootstrap request.
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if input.Spec != nil {
		spec := *input.Spec
		spec.NodeID = node.ID
		if spec.Gateway != nil {
			spec.Gateway.NodeID = node.ID
		}
		if spec.Agent != nil {
			spec.Agent.NodeID = node.ID
		}
		if spec.Kind == "" {
			switch {
			case spec.Gateway != nil:
				spec.Kind = domain.NodeSpecGateway
			case spec.Agent != nil:
				spec.Kind = domain.NodeSpecAgent
			}
		}
		return s.store.PutNodeSpec(r.Context(), spec, WriteOptions{Actor: actor})
	}
	// A generic node may be bootstrapped before its behavior is chosen. It will
	// enroll, establish a control stream, and remain idle until /spec is set.
	return nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
