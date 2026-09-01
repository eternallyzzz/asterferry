package controller

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input, 16<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	keys := loginKeys(r, input.Username)
	if allowed, retry := s.loginLimiter.allow(keys...); !allowed {
		seconds := int(retry / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many failed login attempts")
		return
	}
	user, err := s.store.Authenticate(r.Context(), input.Username, input.Password)
	if err != nil {
		if !isCredentialError(err) {
			slog.Default().Error("controller login storage failure", "error", err)
			writeError(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication service is temporarily unavailable")
			return
		}
		s.loginLimiter.failure(keys...)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "username or password is invalid")
		return
	}
	s.loginLimiter.success(keys...)
	sessionID, csrf, err := randomSessionValues()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_failed", "could not create session")
		return
	}
	s.sessions.Store(sessionID, session{User: user, CSRF: csrf, ExpiresAt: time.Now().Add(12 * time.Hour)})
	http.SetCookie(w, &http.Cookie{Name: "af_session", Value: sessionID, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 60 * 60})
	http.SetCookie(w, &http.Cookie{Name: "af_csrf", Value: csrf, Path: "/", HttpOnly: false, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 60 * 60})
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "csrf_token": csrf})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if _, ok := s.authorize(w, r, RoleViewer); !ok {
		return
	}
	if sessionID, err := r.Cookie("af_session"); err == nil {
		s.sessions.Delete(sessionID.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "af_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "af_csrf", Value: "", Path: "/", MaxAge: -1, Secure: true, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authorize(w, r, RoleViewer)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, required string) (User, bool) {
	var user User
	var err error
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		parts := strings.Fields(header)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			user, err = s.store.AuthenticateToken(r.Context(), parts[1])
		}
	} else if cookie, cookieErr := r.Cookie("af_session"); cookieErr == nil {
		if value, ok := s.sessions.Load(cookie.Value); ok {
			sess, valid := value.(session)
			if !valid {
				s.sessions.Delete(cookie.Value)
				writeError(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
				return User{}, false
			}
			if time.Now().Before(sess.ExpiresAt) {
				// Do not let an in-memory session outlive an Admin revocation or
				// role change. The database remains authoritative after login.
				fresh, lookupErr := s.store.GetUser(r.Context(), sess.User.ID)
				if lookupErr != nil {
					if !errors.Is(lookupErr, sql.ErrNoRows) {
						slog.Default().Error("controller session lookup failed", "error", lookupErr)
						writeError(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication service is temporarily unavailable")
						return User{}, false
					}
					s.sessions.Delete(cookie.Value)
					writeError(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
					return User{}, false
				}
				if !fresh.Enabled {
					s.sessions.Delete(cookie.Value)
					writeError(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
					return User{}, false
				}
				if !fresh.PasswordChangedAt.Equal(sess.User.PasswordChangedAt) {
					// Password changes invalidate every in-memory session even when
					// the session itself has not expired.
					s.sessions.Delete(cookie.Value)
					writeError(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
					return User{}, false
				}
				user = fresh
				if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-CSRF-Token") != sess.CSRF {
					writeError(w, http.StatusForbidden, "csrf_failed", "CSRF token is missing or invalid")
					return User{}, false
				}
			} else {
				s.sessions.Delete(cookie.Value)
			}
		}
	}
	if err != nil {
		if !isCredentialError(err) {
			slog.Default().Error("controller authentication storage failure", "error", err)
			writeError(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication service is temporarily unavailable")
			return User{}, false
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
		return User{}, false
	}
	if !user.Enabled {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
		return User{}, false
	}
	if !roleAllows(user.Role, required) {
		writeError(w, http.StatusForbidden, "forbidden", "the current role cannot perform this operation")
		return User{}, false
	}
	return user, true
}

func roleAllows(actual, required string) bool {
	if actual == RoleAdmin {
		return true
	}
	if required == RoleViewer {
		return actual == RoleViewer || actual == RoleOperator
	}
	return actual == required
}

func randomSessionValues() (string, string, error) {
	first, second := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(first); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(second); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(first), hex.EncodeToString(second), nil
}
