package controller

import (
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
	node, err := s.resources.GetNode(r.Context(), nodeID)
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
	plain, token, err := s.resources.CreateNodeEnrollmentTokenWithOptions(r.Context(), node.ID, EnrollmentTTL, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
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
