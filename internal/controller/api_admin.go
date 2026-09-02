package controller

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) enrollmentTokens(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		var input struct {
			TTLSeconds int `json:"ttl_seconds"`
		}
		if err := decodeJSON(r, &input, 16<<10); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		ttl := EnrollmentTTL
		if input.TTLSeconds > 0 {
			ttl = time.Duration(input.TTLSeconds) * time.Second
		}
		plain, token, err := s.store.CreateEnrollmentTokenWithOptions(r.Context(), ttl, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
		if err != nil {
			if errors.Is(err, ErrSecretAlreadyCreated) {
				writeAlreadyCreatedSecret(w, "token_metadata", token)
				return
			}
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": plain, "token_metadata": token, "created_by": user.Username})
		return
	}
	if r.Method == http.MethodGet {
		items, err := s.store.ListEnrollmentTokens(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	methodNotAllowed(w, http.MethodGet, http.MethodPost)
}

func (s *Server) enrollmentTokenAction(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/enrollment-tokens/"), "/")
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "enrollment token not found")
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	if err := s.store.RevokeEnrollmentTokenWithOptions(r.Context(), id, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if _, ok := s.authorize(w, r, RoleViewer); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.store.ListAudit(r.Context(), limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleAdmin); !ok {
			return
		}
		items, err := s.store.ListUsers(r.Context())
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
	actor, ok := s.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decodeJSON(r, &input, 16<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	user, err := s.store.CreateUserWithOptions(r.Context(), input.Username, input.Password, input.Role, WriteOptions{Actor: actor.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, user.Revision)
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) userAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/users/"), "/"), "/")
	if len(parts) == 1 && parts[0] != "" {
		actor, ok := s.authorize(w, r, RoleAdmin)
		if !ok {
			return
		}
		if r.Method == http.MethodGet {
			user, err := s.store.GetUser(r.Context(), parts[0])
			if err != nil {
				writeStoreError(w, err)
				return
			}
			setETag(w, user.Revision)
			writeJSON(w, http.StatusOK, user)
			return
		}
		if r.Method == http.MethodDelete {
			expected, err := parseIfMatch(r.Header.Get("If-Match"))
			if err != nil {
				writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
				return
			}
			if err := s.store.DeleteUser(r.Context(), parts[0], WriteOptions{IfMatch: expected, Actor: actor.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
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
			Username *string `json:"username"`
			Password *string `json:"password"`
			Role     *string `json:"role"`
			Enabled  *bool   `json:"enabled"`
		}
		if err := decodeJSON(r, &input, 16<<10); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		expected, err := parseIfMatch(r.Header.Get("If-Match"))
		if err != nil {
			writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
			return
		}
		updated, err := s.store.UpdateUser(r.Context(), parts[0], UserUpdate{Username: input.Username, Password: input.Password, Role: input.Role, Enabled: input.Enabled}, WriteOptions{IfMatch: expected, Actor: actor.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, updated.Revision)
		writeJSON(w, http.StatusOK, updated)
		return
	}
	if len(parts) < 2 || parts[1] != "tokens" || parts[0] == "" || len(parts) > 3 {
		writeError(w, http.StatusNotFound, "not_found", "user token resource was not found")
		return
	}
	actor, ok := s.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	if len(parts) == 3 {
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, http.MethodDelete)
			return
		}
		if err := s.store.RevokeAPITokenForUser(r.Context(), parts[0], parts[2], WriteOptions{Actor: actor.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodGet {
		items, err := s.store.ListAPITokens(r.Context(), parts[0])
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
	var input struct {
		Name      string     `json:"name"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := decodeJSON(r, &input, 16<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	plain, token, err := s.store.CreateAPITokenWithOptions(r.Context(), parts[0], input.Name, input.ExpiresAt, WriteOptions{Actor: actor.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		if errors.Is(err, ErrSecretAlreadyCreated) {
			writeAlreadyCreatedSecret(w, "metadata", token)
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": plain, "metadata": token, "created_by": actor.Username})
}
