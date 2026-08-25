package observability

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"asterferry/internal/configstore"
	"asterferry/internal/transport"
)

type Metrics struct {
	Connections                 atomic.Int64
	ActiveStreams               atomic.Int64
	Draining                    atomic.Bool
	Shutdowns                   atomic.Uint64
	ForcedShutdowns             atomic.Uint64
	BytesIn                     atomic.Uint64
	BytesOut                    atomic.Uint64
	AuthFailures                atomic.Uint64
	MappingFailures             atomic.Uint64
	ObfuscationAccepted         atomic.Uint64
	ObfuscationRejected         atomic.Uint64
	ObfuscationPreviousKey      atomic.Uint64
	ObfuscationFragmentsDropped atomic.Uint64

	// QUIC fields are latest-session gauges. They intentionally have no
	// connection labels so a busy gateway cannot create unbounded metric
	// cardinality. Application byte counters above remain the authoritative
	// aggregate totals.
	QUICRTTMicros       atomic.Uint64
	QUICBytesSent       atomic.Uint64
	QUICBytesReceived   atomic.Uint64
	QUICBytesLost       atomic.Uint64
	QUICPacketsSent     atomic.Uint64
	QUICPacketsReceived atomic.Uint64
	QUICPacketsLost     atomic.Uint64
	QUICGSO             atomic.Bool
	QUICStatsSamples    atomic.Uint64

	ManagementAuthFailures         atomic.Uint64
	ManagementAuthRateLimited      atomic.Uint64
	ManagementActionsAccepted      atomic.Uint64
	ManagementActionsRejected      atomic.Uint64
	ManagementEventStreamsRejected atomic.Uint64
	ManagementEventSubscribers     atomic.Int64
}

func (m *Metrics) BeginDrain() {
	if m != nil {
		m.Draining.Store(true)
	}
}

func (m *Metrics) RecordShutdown(forced bool) {
	if m == nil {
		return
	}
	m.Shutdowns.Add(1)
	if forced {
		m.ForcedShutdowns.Add(1)
	}
}

// ObserveQUIC stores a low-cardinality snapshot of native QUIC diagnostics.
// quic-go exposes cumulative counters per connection; the management endpoint
// presents the most recently observed session, while the application counters
// continue to provide process-wide totals.
func (m *Metrics) ObserveQUIC(stats transport.ConnectionStats) {
	if m == nil {
		return
	}
	if stats.RTT > 0 {
		m.QUICRTTMicros.Store(uint64(stats.RTT / time.Microsecond))
	}
	m.QUICBytesSent.Store(stats.BytesSent)
	m.QUICBytesReceived.Store(stats.BytesReceived)
	m.QUICBytesLost.Store(stats.BytesLost)
	m.QUICPacketsSent.Store(stats.PacketsSent)
	m.QUICPacketsReceived.Store(stats.PacketsReceived)
	m.QUICPacketsLost.Store(stats.PacketsLost)
	m.QUICGSO.Store(stats.GSO)
	m.QUICStatsSamples.Add(1)
}

func (m *Metrics) ObfuscationPacketAccepted(previousKey bool) {
	if m == nil {
		return
	}
	m.ObfuscationAccepted.Add(1)
	if previousKey {
		m.ObfuscationPreviousKey.Add(1)
	}
}

func (m *Metrics) ObfuscationPacketRejected() {
	if m != nil {
		m.ObfuscationRejected.Add(1)
	}
}

func (m *Metrics) ObfuscationFragmentDropped() {
	if m != nil {
		m.ObfuscationFragmentsDropped.Add(1)
	}
}

type Server struct {
	httpServer *http.Server
	listener   net.Listener
	Metrics    *Metrics
}

type StatusProvider interface {
	Status() any
	IsReady() bool
}

type ServerOptions struct {
	Events    *EventHub
	Actions   ActionProvider
	Dashboard http.Handler
	TLS       *TLSServerOptions
	Config    *configstore.Manager
	Restart   func() bool
	Logger    *slog.Logger
}

type TLSServerOptions struct {
	CertFile string
	KeyFile  string
}

type AuthScope uint8

const (
	ScopeNone AuthScope = iota
	ScopeViewer
	ScopeAdmin
)

// AuthTokens contains the credentials accepted by the management plane. An
// admin token also satisfies viewer requests; a viewer token never satisfies
// mutating requests.
type AuthTokens struct {
	Admin  []byte
	Viewer []byte
}

func Start(listen string, metrics *Metrics, provider StatusProvider, authToken []byte, options ...ServerOptions) (*Server, error) {
	return StartWithTokens(listen, metrics, provider, AuthTokens{Admin: authToken, Viewer: authToken}, options...)
}

func StartWithTokens(listen string, metrics *Metrics, provider StatusProvider, tokens AuthTokens, options ...ServerOptions) (*Server, error) {
	if len(tokens.Admin) < 32 || len(tokens.Viewer) < 32 {
		return nil, errors.New("management authentication tokens must each contain at least 32 bytes")
	}
	if metrics == nil {
		metrics = &Metrics{}
	}
	var serverOptions ServerOptions
	if len(options) > 0 {
		serverOptions = options[0]
	}
	if !managementListenIsLoopback(listen) && serverOptions.TLS == nil {
		return nil, errors.New("non-loopback management listener requires TLS")
	}
	auth := newAuthGuard(time.Now)
	mux := http.NewServeMux()
	if serverOptions.Dashboard != nil {
		mux.Handle("/dashboard/", http.StripPrefix("/dashboard", serverOptions.Dashboard))
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if provider != nil && !provider.IsReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	protected := func(required AuthScope, next http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope := authorizationScope(r, tokens)
			if scope == ScopeAdmin || (scope == ScopeViewer && required == ScopeViewer) {
				auth.reset()
				next(w, r)
				return
			}
			if scope != ScopeNone {
				auth.reset()
				w.Header().Set("WWW-Authenticate", `Bearer realm="asterferry-management", scope="admin"`)
				writeActionError(w, http.StatusForbidden, "insufficient_scope", "management token lacks the required scope")
				return
			}
			if blocked, retryAfter := auth.blocked(); blocked {
				metrics.ManagementAuthRateLimited.Add(1)
				auditManagement(serverOptions.Logger, "management authentication rate limited", "management.auth.rate_limited", "error_kind", "rate_limited")
				writeRateLimited(w, retryAfter)
				return
			}
			limited, retryAfter := auth.recordFailure()
			if limited {
				metrics.ManagementAuthRateLimited.Add(1)
				auditManagement(serverOptions.Logger, "management authentication rate limited", "management.auth.rate_limited", "error_kind", "rate_limited")
				writeRateLimited(w, retryAfter)
				return
			}
			metrics.ManagementAuthFailures.Add(1)
			auditManagement(serverOptions.Logger, "management authentication rejected", "management.auth.rejected", "error_kind", "invalid_credentials")
			w.Header().Set("WWW-Authenticate", `Bearer realm="asterferry-management"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("unauthorized\n"))
		})
	}
	if serverOptions.Config != nil {
		mux.Handle("/v1/config", protected(ScopeViewer, func(w http.ResponseWriter, r *http.Request) {
			serveConfigSnapshot(w, r, serverOptions.Config)
		}))
		mux.Handle("/v1/config/validate", protected(ScopeViewer, func(w http.ResponseWriter, r *http.Request) {
			serveConfigValidate(w, r, serverOptions.Config, serverOptions.Logger)
		}))
		mux.Handle("/v1/config/apply", protected(ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
			serveConfigApply(w, r, serverOptions.Config, serverOptions.Restart, serverOptions.Logger)
		}))
		mux.Handle("/v1/config/rollback", protected(ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
			serveConfigRollback(w, r, serverOptions.Config, serverOptions.Restart, serverOptions.Logger)
		}))
	}
	mux.Handle("/metrics", protected(ScopeViewer, func(w http.ResponseWriter, _ *http.Request) { writeMetrics(w, metrics) }))
	mux.Handle("/v1/status", protected(ScopeViewer, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if provider == nil {
			_, _ = w.Write([]byte("{}\n"))
			return
		}
		b, err := json.Marshal(provider.Status())
		if err != nil {
			logManagementError(serverOptions.Logger, "management status serialization failed", "management.status.serialize_failed")
			writeActionError(w, http.StatusInternalServerError, "status_unavailable", "status is temporarily unavailable")
			return
		}
		_, _ = w.Write(b)
	}))
	mux.Handle("/v1/dashboard", protected(ScopeViewer, func(w http.ResponseWriter, _ *http.Request) {
		serveDashboardSnapshot(w, provider, metrics, serverOptions.Logger)
	}))
	if serverOptions.Events != nil {
		mux.Handle("/v1/events", protected(ScopeViewer, func(w http.ResponseWriter, r *http.Request) {
			serveEvents(w, r, serverOptions.Events, metrics, serverOptions.Logger)
		}))
	}
	if serverOptions.Actions != nil {
		mux.Handle("/v1/actions/shutdown", protected(ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
			serveAction(w, r, serverOptions.Actions, "shutdown", metrics, serverOptions.Logger)
		}))
		mux.Handle("/v1/actions/reconnect", protected(ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
			serveAction(w, r, serverOptions.Actions, "reconnect", metrics, serverOptions.Logger)
		}))
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	if serverOptions.TLS != nil {
		if strings.TrimSpace(serverOptions.TLS.CertFile) == "" || strings.TrimSpace(serverOptions.TLS.KeyFile) == "" {
			_ = ln.Close()
			return nil, errors.New("management TLS requires both certificate and key files")
		}
		if _, err := tls.LoadX509KeyPair(serverOptions.TLS.CertFile, serverOptions.TLS.KeyFile); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("load management TLS certificate: %w", err)
		}
	}
	hs := &http.Server{
		Addr:              listen,
		Handler:           withManagementHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// SSE is intentionally long-lived. serveEvents applies a rolling
		// deadline per event/heartbeat so slow dead clients are still bounded.
		WriteTimeout:   0,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}
	s := &Server{httpServer: hs, listener: ln, Metrics: metrics}
	if serverOptions.TLS != nil {
		hs.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		go func() { _ = hs.ServeTLS(ln, serverOptions.TLS.CertFile, serverOptions.TLS.KeyFile) }()
	} else {
		go func() { _ = hs.Serve(ln) }()
	}
	return s, nil
}

func managementListenIsLoopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func withManagementHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func authorizationScope(r *http.Request, tokens AuthTokens) AuthScope {
	if r == nil {
		return ScopeNone
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) < len("Bearer ")+1 || !strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		return ScopeNone
	}
	presented := strings.TrimSpace(value[len("Bearer "):])
	if presented == "" {
		return ScopeNone
	}
	gotHash := sha256.Sum256([]byte(presented))
	adminHash := sha256.Sum256(tokens.Admin)
	if subtleConstantTimeCompare(gotHash[:], adminHash[:]) {
		return ScopeAdmin
	}
	viewerHash := sha256.Sum256(tokens.Viewer)
	if subtleConstantTimeCompare(gotHash[:], viewerHash[:]) {
		return ScopeViewer
	}
	return ScopeNone
}

// Kept as a tiny wrapper to make the authentication decision easy to test
// without exposing crypto implementation details to the HTTP handlers.
func subtleConstantTimeCompare(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (s *Server) Close() error {
	if s == nil || s.httpServer == nil {
		return nil
	}
	err := s.httpServer.Close()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	return err
}

func writeMetrics(w http.ResponseWriter, m *Metrics) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	f := func(name string, value any) { _, _ = fmt.Fprintf(w, "%s %v\n", name, value) }
	f("asterferry_connections", m.Connections.Load())
	f("asterferry_active_streams", m.ActiveStreams.Load())
	if m.Draining.Load() {
		f("asterferry_draining", 1)
	} else {
		f("asterferry_draining", 0)
	}
	f("asterferry_shutdowns_total", m.Shutdowns.Load())
	f("asterferry_forced_shutdowns_total", m.ForcedShutdowns.Load())
	f("asterferry_bytes_in_total", m.BytesIn.Load())
	f("asterferry_bytes_out_total", m.BytesOut.Load())
	f("asterferry_auth_failures_total", m.AuthFailures.Load())
	f("asterferry_management_auth_failures_total", m.ManagementAuthFailures.Load())
	f("asterferry_management_auth_rate_limited_total", m.ManagementAuthRateLimited.Load())
	f("asterferry_management_actions_accepted_total", m.ManagementActionsAccepted.Load())
	f("asterferry_management_actions_rejected_total", m.ManagementActionsRejected.Load())
	f("asterferry_management_event_stream_rejections_total", m.ManagementEventStreamsRejected.Load())
	f("asterferry_management_event_subscribers", m.ManagementEventSubscribers.Load())
	f("asterferry_mapping_failures_total", m.MappingFailures.Load())
	f("asterferry_obfuscation_packets_accepted_total", m.ObfuscationAccepted.Load())
	f("asterferry_obfuscation_packets_rejected_total", m.ObfuscationRejected.Load())
	f("asterferry_obfuscation_previous_key_total", m.ObfuscationPreviousKey.Load())
	f("asterferry_obfuscation_fragments_dropped_total", m.ObfuscationFragmentsDropped.Load())
	f("asterferry_quic_rtt_microseconds", m.QUICRTTMicros.Load())
	f("asterferry_quic_bytes_sent", m.QUICBytesSent.Load())
	f("asterferry_quic_bytes_received", m.QUICBytesReceived.Load())
	f("asterferry_quic_bytes_lost", m.QUICBytesLost.Load())
	f("asterferry_quic_packets_sent", m.QUICPacketsSent.Load())
	f("asterferry_quic_packets_received", m.QUICPacketsReceived.Load())
	f("asterferry_quic_packets_lost", m.QUICPacketsLost.Load())
	if m.QUICGSO.Load() {
		f("asterferry_quic_gso", 1)
	} else {
		f("asterferry_quic_gso", 0)
	}
	f("asterferry_quic_stats_samples", m.QUICStatsSamples.Load())
}
