package controller

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"asterferry/internal/dashboard"
	"asterferry/internal/domain"
	"asterferry/internal/jsonutil"
)

type Server struct {
	store         *Store
	config        Config
	http          *http.Server
	sessions      sync.Map // opaque session id -> session
	sessionCtx    context.Context
	sessionCancel context.CancelFunc
	sessionDone   chan struct{}
	loginLimiter  *loginLimiter
	metrics       *ControllerMetrics
}

type session struct {
	User      User
	CSRF      string
	ExpiresAt time.Time
}

func NewServer(config Config, store *Store) (*Server, error) {
	if store == nil {
		return nil, errors.New("controller store is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if store.metrics == nil {
		store.metrics = newControllerMetrics()
	}
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	server := &Server{store: store, config: config, sessionCtx: sessionCtx, sessionCancel: sessionCancel, sessionDone: make(chan struct{}), loginLimiter: newLoginLimiter(), metrics: store.metrics}
	server.http = &http.Server{Addr: config.HTTPListen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second}
	go server.runSessionReaper()
	return server, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/metrics", s.metricsHandler)
	mux.HandleFunc("/openapi.yaml", s.openapi)
	// Keep the document available both at the deployment-root path and under
	// the versioned API prefix.  Reverse proxies commonly mount the whole
	// Controller behind /api, while the root endpoint is useful for probes and
	// mirrors the anonymous /healthz surface.
	mux.HandleFunc("/api/v1/openapi.yaml", s.openapi)
	mux.HandleFunc("/api/v1/auth/login", s.login)
	mux.HandleFunc("/api/v1/auth/logout", s.logout)
	mux.HandleFunc("/api/v1/me", s.me)
	mux.HandleFunc("/api/v1/nodes", s.nodes)
	mux.HandleFunc("/api/v1/nodes/", s.nodeAction)
	mux.HandleFunc("/api/v1/gateways", s.gateways)
	mux.HandleFunc("/api/v1/gateways/", s.gatewayAction)
	mux.HandleFunc("/api/v1/agents", s.agents)
	mux.HandleFunc("/api/v1/agents/", s.agentAction)
	mux.HandleFunc("/api/v1/services", s.services)
	mux.HandleFunc("/api/v1/services/", s.serviceAction)
	mux.HandleFunc("/api/v1/assignments", s.assignments)
	mux.HandleFunc("/api/v1/assignments/", s.assignmentAction)
	mux.HandleFunc("/api/v1/enrollment-tokens", s.enrollmentTokens)
	mux.HandleFunc("/api/v1/enrollment-tokens/", s.enrollmentTokenAction)
	mux.HandleFunc("/api/v1/audit", s.audit)
	mux.HandleFunc("/api/v1/events", s.audit)
	mux.HandleFunc("/api/v1/users", s.users)
	mux.HandleFunc("/api/v1/users/", s.userAction)
	if s.config.DashboardEnable {
		mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", dashboard.Handler()))
	}
	return s.metrics.middleware(securityHeaders(mux))
}

func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }
func (s *Server) ListenAndServeTLS() error {
	return s.http.ListenAndServeTLS(s.config.TLSCertPath, s.config.TLSKeyPath)
}

// TLSListener binds and configures the Controller HTTPS endpoint without
// starting a serving goroutine. Controller.Start uses this two-phase form so
// a bind or certificate error is returned to the caller instead of being lost
// in a background ListenAndServeTLS goroutine.
func (s *Server) TLSListener() (net.Listener, error) {
	if s == nil || s.http == nil {
		return nil, errors.New("controller HTTP server is not initialized")
	}
	certificate, err := tls.LoadX509KeyPair(s.config.TLSCertPath, s.config.TLSKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load controller HTTPS certificate: %w", err)
	}
	listener, err := net.Listen("tcp", s.config.HTTPListen)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	return tls.NewListener(listener, tlsConfig), nil
}

// Serve runs the already-bound HTTPS listener. It is separate from
// ListenAndServeTLS so startup can fail atomically when either Controller
// endpoint cannot be bound.
func (s *Server) Serve(listener net.Listener) error {
	if s == nil || s.http == nil {
		return errors.New("controller HTTP server is not initialized")
	}
	if listener == nil {
		return errors.New("controller HTTP listener is required")
	}
	return s.http.Serve(listener)
}
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	if s.sessionCancel != nil {
		s.sessionCancel()
		if s.sessionDone != nil {
			<-s.sessionDone
		}
	}
	if s.http == nil {
		return nil
	}
	return s.http.Close()
}

const sessionReapInterval = time.Minute

func (s *Server) runSessionReaper() {
	if s == nil {
		return
	}
	defer close(s.sessionDone)
	ticker := time.NewTicker(sessionReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.pruneExpiredSessions(time.Now())
		case <-s.sessionCtx.Done():
			return
		}
	}
}

func (s *Server) pruneExpiredSessions(now time.Time) {
	if s == nil {
		return
	}
	s.sessions.Range(func(key, value any) bool {
		sess, ok := value.(session)
		if !ok || !now.Before(sess.ExpiresAt) {
			s.sessions.Delete(key)
		}
		return true
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "controller"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, RoleViewer); !ok {
		return
	}
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "controller database is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true})
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, RoleViewer); !ok {
		return
	}
	s.metrics.refreshSQLite(s.store)
	s.metrics.Handler().ServeHTTP(w, r)
}

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
	http.SetCookie(w, &http.Cookie{Name: "af_session", Value: sessionID, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 60 * 60})
	http.SetCookie(w, &http.Cookie{Name: "af_csrf", Value: csrf, Path: "/", HttpOnly: false, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 60 * 60})
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
	http.SetCookie(w, &http.Cookie{Name: "af_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "af_csrf", Value: "", Path: "/", MaxAge: -1, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authorize(w, r, RoleViewer)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		role := r.URL.Query().Get("role")
		nodes, err := s.store.ListNodes(r.Context(), role)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": nodes})
		return
	}
	user, ok := s.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	var input struct {
		ID      string            `json:"id"`
		Role    string            `json:"role"`
		Name    string            `json:"name"`
		Labels  map[string]string `json:"labels"`
		Enabled *bool             `json:"enabled"`
	}
	if err := decodeJSON(r, &input, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	node := domain.Node{ID: input.ID, Role: input.Role, Name: input.Name, Labels: input.Labels, Enabled: enabled}
	if err := s.store.CreateNode(r.Context(), node, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	created, err := s.store.GetNode(r.Context(), node.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, created.Revision)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) nodeAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found", "node not found")
		return
	}
	nodeID := parts[0]
	if len(parts) == 2 && r.Method == http.MethodGet && (parts[1] == "observed" || parts[1] == "snapshot" || parts[1] == "desired") {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		if parts[1] == "observed" {
			observed, err := s.store.GetObserved(r.Context(), nodeID)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			setETagUint64(w, observed.AppliedGeneration)
			writeJSON(w, http.StatusOK, observed)
			return
		}
		// Materialize the latest complete desired document on demand. The
		// control stream also refreshes it periodically, but API callers should
		// not observe a stale/absent snapshot merely because no node is online.
		if _, ensureErr := s.store.EnsureDesiredSnapshot(r.Context(), nodeID); ensureErr != nil && !errors.Is(ensureErr, sql.ErrNoRows) {
			writeStoreError(w, ensureErr)
			return
		}
		snapshot, err := s.store.GetSnapshot(r.Context(), nodeID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETagUint64(w, snapshot.Generation)
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		node, err := s.store.GetNode(r.Context(), nodeID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, node.Revision)
		writeJSON(w, http.StatusOK, node)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		user, ok := s.authorize(w, r, RoleAdmin)
		if !ok {
			return
		}
		var input struct {
			Role              *string            `json:"role"`
			Name              *string            `json:"name"`
			Labels            *map[string]string `json:"labels"`
			Enabled           *bool              `json:"enabled"`
			CertificateState  *string            `json:"certificate_state"`
			CertificateSerial *string            `json:"certificate_serial"`
		}
		if err := decodeJSON(r, &input, 1<<20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		node, getErr := s.store.GetNode(r.Context(), nodeID)
		if getErr != nil {
			writeStoreError(w, getErr)
			return
		}
		if input.Role != nil {
			node.Role = *input.Role
		}
		if input.Name != nil {
			node.Name = *input.Name
		}
		if input.Labels != nil {
			node.Labels = *input.Labels
		}
		if input.Enabled != nil {
			node.Enabled = *input.Enabled
		}
		if input.CertificateState != nil {
			node.CertificateState = *input.CertificateState
		}
		if input.CertificateSerial != nil {
			node.CertificateSerial = *input.CertificateSerial
		}
		expected, err := parseIfMatch(r.Header.Get("If-Match"))
		if err != nil {
			writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
			return
		}
		if err := s.store.UpdateNode(r.Context(), node, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
			writeStoreError(w, err)
			return
		}
		if !node.Enabled || defaultCertificateState(node.CertificateState) != domain.CertificateActive {
			// Revoke is enforced at the next mTLS handshake. Ask an online node
			// to disconnect immediately as well. Disabling, expiry and a pending
			// certificate follow the same fail-closed path; an offline node is
			// rejected when it reconnects.
			delivered, actionErr := s.store.PublishAction(r.Context(), nodeID, "reconnect", "")
			if actionErr != nil || !delivered {
				if actionErr != nil {
					slog.Default().Error("failed to publish node security action", "node_id", nodeID, "error", actionErr)
				} else {
					slog.Default().Warn("node security action is not currently delivered", "node_id", nodeID)
				}
				eventType := "action_not_delivered"
				if actionErr != nil {
					eventType = "action_delivery_failed"
				}
				if eventErr := s.store.RecordEvent(context.Background(), user.Username, "", eventType, "reconnect action was not delivered immediately", nodeID, map[string]string{"action": "reconnect"}); eventErr != nil {
					slog.Default().Error("failed to record security action delivery event", "node_id", nodeID, "error", eventErr)
				}
			}
		}
		updated, err := s.store.GetNode(r.Context(), nodeID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		setETag(w, updated.Revision)
		writeJSON(w, http.StatusOK, updated)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		user, ok := s.authorize(w, r, RoleAdmin)
		if !ok {
			return
		}
		expected, err := parseIfMatch(r.Header.Get("If-Match"))
		if err != nil {
			writeError(w, http.StatusPreconditionRequired, "if_match_required", err.Error())
			return
		}
		if err := s.store.DeleteNode(r.Context(), nodeID, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 3 && parts[1] == "actions" && r.Method == http.MethodPost {
		user, ok := s.authorize(w, r, RoleOperator)
		if !ok {
			return
		}
		action := parts[2]
		if action != "drain" && action != "reconnect" && action != "resync" {
			writeError(w, http.StatusBadRequest, "unknown_action", "unsupported node action")
			return
		}
		// Persist and audit the request before publishing it to connected node
		// streams. The idempotency key covers both operations, so a retried
		// request cannot emit the same action twice.
		delivered, requestErr := s.store.RequestNodeAction(r.Context(), nodeID, action, "", WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
		if requestErr != nil {
			writeStoreError(w, requestErr)
			return
		}
		state := "queued"
		if delivered {
			state = "delivered"
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"node_id": nodeID, "action": action, "requested_by": user.Username, "state": state})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "node resource was not found")
}

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

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		views, err := s.store.ListAgentViews(r.Context())
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
	var spec domain.AgentSpec
	if err := decodeJSON(r, &spec, 4<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.store.PutAgentSpec(r.Context(), spec, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	created, err := s.store.GetAgentSpec(r.Context(), spec.NodeID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, created.Revision)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) agentAction(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/agents/"), "/")
	parts := strings.Split(path, "/")
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	if len(parts) == 2 && parts[1] == "egress" {
		s.agentEgressAction(w, r, id)
		return
	}
	if len(parts) >= 2 && (parts[1] == "proxies" || parts[1] == "routes") {
		s.agentSpecSubresource(w, r, id, parts[1], parts[2:])
		return
	}
	if len(parts) == 3 && parts[1] == "actions" && r.Method == http.MethodPost {
		user, ok := s.authorize(w, r, RoleOperator)
		if !ok {
			return
		}
		if parts[2] != "schedule" {
			writeError(w, http.StatusBadRequest, "unknown_action", "unsupported agent action")
			return
		}
		assignment, err := s.store.ScheduleAgent(r.Context(), id, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, assignment)
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not_found", "agent resource was not found")
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		spec, err := s.store.GetAgentSpec(r.Context(), id)
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
			if err := s.store.DeleteAgentSpec(r.Context(), id, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
				writeStoreError(w, err)
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
	var spec domain.AgentSpec
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
	if err := s.store.PutAgentSpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.store.GetAgentSpec(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, updated.Revision)
	writeJSON(w, http.StatusOK, updated)
}

// agentEgressAction is the Agent counterpart to gatewayEgressAction. The
// complete AgentSpec remains the persisted resource; this endpoint only offers
// a narrow policy-shaped API for Dashboard and automation clients.
func (s *Server) agentEgressAction(w http.ResponseWriter, r *http.Request, agentID string) {
	spec, err := s.store.GetAgentSpec(r.Context(), agentID)
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
	if err := s.store.PutAgentSpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.store.GetAgentSpec(r.Context(), agentID)
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
		writeError(w, http.StatusNotFound, "not_found", "agent subresource was not found")
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		spec, err := s.store.GetAgentSpec(r.Context(), agentID)
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
		writeError(w, http.StatusNotFound, "not_found", "agent subresource was not found")
		return
	}
	user, ok := s.authorize(w, r, RoleOperator)
	if !ok {
		return
	}
	spec, err := s.store.GetAgentSpec(r.Context(), agentID)
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
	if err := s.store.PutAgentSpec(r.Context(), spec, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.store.GetAgentSpec(r.Context(), agentID)
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

func (s *Server) services(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		items, err := s.store.ListServices(r.Context(), r.URL.Query().Get("agent_id"))
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
	if err := s.store.PutService(r.Context(), service, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	created, err := s.store.GetService(r.Context(), service.ID)
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
		item, err := s.store.GetService(r.Context(), id)
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
		if err := s.store.DeleteService(r.Context(), id, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
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
	service, getErr := s.store.GetService(r.Context(), id)
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
	if err := s.store.PutService(r.Context(), service, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, readErr := s.store.GetService(r.Context(), id)
	if readErr != nil {
		writeStoreError(w, readErr)
		return
	}
	setETag(w, updated.Revision)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) assignments(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, RoleViewer); !ok {
			return
		}
		items, err := s.store.ListAssignments(r.Context(), r.URL.Query().Get("gateway_id"), r.URL.Query().Get("agent_id"))
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
	if err := s.store.PutAssignment(r.Context(), assignment, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	created, err := s.store.GetAssignment(r.Context(), assignment.ID)
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
		assignment, err := s.store.GetAssignment(r.Context(), id)
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
		if err := s.store.DeleteAssignment(r.Context(), id, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
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
	if err := s.store.PutAssignment(r.Context(), assignment, WriteOptions{IfMatch: expected, Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")}); err != nil {
		writeStoreError(w, err)
		return
	}
	updated, err := s.store.GetAssignment(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	setETag(w, updated.Revision)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) enrollmentTokens(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		var input struct {
			Role       string `json:"role"`
			TTLSeconds int    `json:"ttl_seconds"`
		}
		if err := decodeJSON(r, &input, 16<<10); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		ttl := EnrollmentTTL
		if input.TTLSeconds > 0 {
			ttl = time.Duration(input.TTLSeconds) * time.Second
		}
		plain, token, err := s.store.CreateEnrollmentTokenWithOptions(r.Context(), input.Role, ttl, WriteOptions{Actor: user.Username, IdempotencyKey: r.Header.Get("Idempotency-Key")})
		if err != nil {
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
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": plain, "metadata": token, "created_by": actor.Username})
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

func decodeJSON(r *http.Request, value any, max int64) error {
	if r.Body == nil {
		return errors.New("request body is required")
	}
	defer r.Body.Close()
	if max <= 0 {
		max = 1 << 20
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > max {
		return errors.New("request body is too large")
	}
	if err := jsonutil.DecodeStrict(data, value); err != nil {
		if errors.Is(err, jsonutil.ErrTrailingJSON) {
			return errors.New("request contains trailing JSON")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func setETag(w http.ResponseWriter, revision int64) {
	if revision > 0 {
		w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(revision, 10)))
	}
}
func setETagUint64(w http.ResponseWriter, revision uint64) {
	if revision > 0 {
		w.Header().Set("ETag", strconv.Quote(strconv.FormatUint(revision, 10)))
	}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
}
func parseIfMatch(value string) (int64, error) {
	value = strings.Trim(strings.TrimSpace(value), "\"")
	if value == "" {
		return 0, errors.New("If-Match revision is required")
	}
	result, err := strconv.ParseInt(value, 10, 64)
	if err != nil || result <= 0 {
		return 0, errors.New("If-Match must be a positive revision")
	}
	return result, nil
}

func writeStoreError(w http.ResponseWriter, err error) {
	// modernc.org/sqlite exposes extended result codes through Code(). Classify
	// duplicate resources from the code rather than matching driver prose, which
	// may contain SQL fragments, paths, or change between driver versions.
	if isSQLiteUniqueConstraint(err) {
		writeError(w, http.StatusConflict, "already_exists", "resource already exists")
		return
	}
	var conflict *RevisionConflictError
	if errors.As(err, &conflict) {
		writeError(w, http.StatusConflict, "revision_conflict", conflict.Error())
		return
	}
	var portConflict *PortConflictError
	if errors.As(err, &portConflict) {
		writeError(w, http.StatusConflict, "port_conflict", portConflict.Error())
		return
	}
	var applyErr *domain.ApplyError
	if errors.As(err, &applyErr) {
		// Domain conflicts are safe to retry only after the caller resolves the
		// conflicting resource; expose them as HTTP 409 instead of collapsing
		// them into a generic malformed-request response.
		if applyErr.Code == "resource_conflict" || applyErr.Code == "port_conflict" || applyErr.Code == "bind_mismatch" || applyErr.Code == "port_mismatch" {
			writeApplyError(w, http.StatusConflict, applyErr)
			return
		}
		writeApplyError(w, http.StatusBadRequest, applyErr)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "resource was not found")
		return
	}
	if errors.Is(err, ErrStorageFailure) || isSQLiteError(err) || errors.Is(err, sql.ErrConnDone) {
		slog.Default().Error("controller store operation failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "controller storage is temporarily unavailable")
		return
	}
	// Do not reflect unclassified repository/driver errors. They can contain
	// SQL statements, filesystem paths, or other implementation details.
	writeError(w, http.StatusBadRequest, "request_rejected", "request was rejected")
}

type sqliteError interface{ Code() int }

func isSQLiteError(err error) bool {
	var coded sqliteError
	return errors.As(err, &coded)
}

func isSQLiteUniqueConstraint(err error) bool {
	var coded sqliteError
	if !errors.As(err, &coded) {
		return false
	}
	// SQLITE_CONSTRAINT_PRIMARYKEY and SQLITE_CONSTRAINT_UNIQUE are the
	// extended result codes used by SQLite for duplicate resource identities.
	return coded.Code() == 1555 || coded.Code() == 2067
}

func writeApplyError(w http.ResponseWriter, status int, applyErr *domain.ApplyError) {
	if applyErr == nil {
		writeError(w, status, "request_rejected", "request was rejected")
		return
	}
	fields := map[string]any{
		"code":      applyErr.Code,
		"message":   applyErr.Message,
		"retryable": applyErr.Retryable,
	}
	if applyErr.Path != "" {
		fields["path"] = applyErr.Path
	}
	writeJSON(w, status, map[string]any{"error": fields})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
