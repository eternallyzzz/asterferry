package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"asterferry/internal/dashboard"
)

type Server struct {
	resources     *ResourceRepository
	runtime       *RuntimeRepository
	changes       *ChangeBus
	config        Config
	http          *http.Server
	metricsHTTP   *http.Server
	sessions      sync.Map // process-local opaque session id -> session; not shared or persisted
	sessionCtx    context.Context
	sessionCancel context.CancelFunc
	sessionDone   chan struct{}
	loginLimiter  *loginLimiter
	metrics       *ControllerMetrics
	scheduler     *Scheduler
}

type session struct {
	User      User
	CSRF      string
	ExpiresAt time.Time
}

const (
	sessionTTL              = 12 * time.Hour
	sessionCookieMaxAge     = int(sessionTTL / time.Second)
	sessionReapInterval     = time.Minute
	controllerWriteDeadline = 30 * time.Second
)

func NewServer(config Config, repositories *ControllerRepositories, metrics ...*ControllerMetrics) (*Server, error) {
	return newServer(config, repositories, nil, metrics...)
}

// newServer composes the HTTP surface with the application scheduler. The
// public constructor keeps its historical convenience behavior for embedders,
// while Controller.New injects the single scheduler owned by the composition
// root so reconciliation and API actions share one decision component.
func newServer(config Config, repositories *ControllerRepositories, scheduler *Scheduler, metrics ...*ControllerMetrics) (*Server, error) {
	if repositories == nil || repositories.Resources == nil || repositories.Runtime == nil || repositories.Changes == nil {
		return nil, errors.New("controller repositories are required")
	}
	resources, runtime, changes := repositories.Resources, repositories.Runtime, repositories.Changes
	if err := config.Validate(); err != nil {
		return nil, err
	}
	var controllerMetrics *ControllerMetrics
	if len(metrics) > 0 {
		controllerMetrics = metrics[0]
	}
	if controllerMetrics == nil {
		controllerMetrics = newControllerMetrics(resources.DatabaseDriver())
	}
	if scheduler == nil {
		var err error
		scheduler, err = NewScheduler(resources, controllerMetrics)
		if err != nil {
			return nil, err
		}
	}
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	server := &Server{resources: resources, runtime: runtime, changes: changes, config: config, sessionCtx: sessionCtx, sessionCancel: sessionCancel, sessionDone: make(chan struct{}), loginLimiter: newLoginLimiter(), metrics: controllerMetrics, scheduler: scheduler}
	// Runtime SSE is intentionally long-lived.  Per-request handlers retain
	// their own bounded read/decode limits; the write deadline must not cut off
	// a healthy event stream after an absolute 30-second wall clock interval.
	server.http = &http.Server{Addr: config.HTTPListen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 90 * time.Second}
	if strings.TrimSpace(config.MetricsListen) != "" {
		server.metricsHTTP = &http.Server{Addr: config.MetricsListen, Handler: server.metricsOnlyHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second}
	}
	go server.runSessionReaper()
	return server, nil
}

// metricsOnlyHandler is deliberately a separate surface from the management
// HTTPS handler. It exposes only Prometheus metrics and relies on the bind
// address plus deployment network policy for access control.
func (s *Server) metricsOnlyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.internalMetricsHandler)
	return s.metrics.middleware(securityHeaders(httpWriteDeadlineMiddleware(mux)))
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
	mux.HandleFunc("/api/v1/runtime/settings", s.runtimeSettings)
	mux.HandleFunc("/api/v1/runtime/connections", s.runtimeConnections)
	mux.HandleFunc("/api/v1/runtime/events", s.runtimeEvents)
	mux.HandleFunc("/api/v1/runtime/traffic", s.runtimeTraffic)
	mux.HandleFunc("/api/v1/runtime/stream", s.runtimeStream)
	mux.HandleFunc("/api/v1/users", s.users)
	mux.HandleFunc("/api/v1/users/", s.userAction)
	if s.config.DashboardEnable {
		mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", dashboard.Handler()))
	}
	return s.metrics.middleware(securityHeaders(httpWriteDeadlineMiddleware(mux)))
}

// httpWriteDeadlineMiddleware keeps ordinary responses from holding a
// connection indefinitely while leaving the authenticated runtime SSE stream
// available for as long as the client remains connected. WriteTimeout stays
// disabled on the server because it is an absolute connection/request timeout
// and would terminate a healthy SSE stream.
func httpWriteDeadlineMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline := time.Now().Add(controllerWriteDeadline)
		if r.URL.Path == "/api/v1/runtime/stream" {
			deadline = time.Time{}
		}
		// ResponseController reaches the net/http writer through the metrics
		// wrapper. Test writers and other adapters may not support deadlines;
		// the real net/http server does.
		_ = http.NewResponseController(w).SetWriteDeadline(deadline)
		next.ServeHTTP(w, r)
	})
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

// MetricsListener binds the optional internal metrics endpoint. It is a plain
// HTTP listener by design: native defaults bind loopback, while Kubernetes
// deployments must explicitly expose and restrict the metrics Service.
func (s *Server) MetricsListener() (net.Listener, error) {
	if s == nil || s.metricsHTTP == nil {
		return nil, nil
	}
	return net.Listen("tcp", s.config.MetricsListen)
}

// ServeMetrics runs the already-bound internal metrics listener.
func (s *Server) ServeMetrics(listener net.Listener) error {
	if s == nil || s.metricsHTTP == nil {
		return errors.New("controller metrics server is disabled")
	}
	if listener == nil {
		return errors.New("controller metrics listener is required")
	}
	return s.metricsHTTP.Serve(listener)
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
		if s.metricsHTTP == nil {
			return nil
		}
		return s.metricsHTTP.Close()
	}
	mainErr := s.http.Close()
	if s.metricsHTTP != nil {
		return errors.Join(mainErr, s.metricsHTTP.Close())
	}
	return mainErr
}

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
	if err := s.resources.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "controller database is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true})
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, RoleViewer); !ok {
		return
	}
	s.internalMetricsHandler(w, r)
}

func (s *Server) internalMetricsHandler(w http.ResponseWriter, r *http.Request) {
	s.metrics.refreshDatabase(s.resources)
	s.metrics.Handler().ServeHTTP(w, r)
}
