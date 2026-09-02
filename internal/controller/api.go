package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"asterferry/internal/dashboard"
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
	mux.HandleFunc("/api/v1/node-installations", s.nodeInstallations)
	mux.HandleFunc("/api/v1/node-installations/", s.nodeInstallationAction)
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
