package observability

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

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
}

func Start(listen string, metrics *Metrics, provider StatusProvider, authToken []byte, options ...ServerOptions) (*Server, error) {
	if len(authToken) < 32 {
		return nil, errors.New("management authentication token must contain at least 32 bytes")
	}
	if metrics == nil {
		metrics = &Metrics{}
	}
	var serverOptions ServerOptions
	if len(options) > 0 {
		serverOptions = options[0]
	}
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
	protected := func(next http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !authorized(r, authToken) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="asterferry-management"`)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("unauthorized\n"))
				return
			}
			next(w, r)
		})
	}
	mux.Handle("/metrics", protected(func(w http.ResponseWriter, _ *http.Request) { writeMetrics(w, metrics) }))
	mux.Handle("/v1/status", protected(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if provider == nil {
			_, _ = w.Write([]byte("{}\n"))
			return
		}
		b, err := json.Marshal(provider.Status())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(b)
	}))
	mux.Handle("/v1/dashboard", protected(func(w http.ResponseWriter, _ *http.Request) {
		serveDashboardSnapshot(w, provider, metrics)
	}))
	if serverOptions.Events != nil {
		mux.Handle("/v1/events", protected(func(w http.ResponseWriter, r *http.Request) {
			serveEvents(w, r, serverOptions.Events)
		}))
	}
	if serverOptions.Actions != nil {
		mux.Handle("/v1/actions/shutdown", protected(func(w http.ResponseWriter, r *http.Request) {
			serveAction(w, r, serverOptions.Actions, "shutdown")
		}))
		mux.Handle("/v1/actions/reconnect", protected(func(w http.ResponseWriter, r *http.Request) {
			serveAction(w, r, serverOptions.Actions, "reconnect")
		}))
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	hs := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	s := &Server{httpServer: hs, listener: ln, Metrics: metrics}
	go func() { _ = hs.Serve(ln) }()
	return s, nil
}

func authorized(r *http.Request, token []byte) bool {
	if r == nil {
		return false
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) < len("Bearer ")+1 || !strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		return false
	}
	presented := strings.TrimSpace(value[len("Bearer "):])
	if presented == "" {
		return false
	}
	wantHash := sha256.Sum256(token)
	gotHash := sha256.Sum256([]byte(presented))
	return subtleConstantTimeCompare(gotHash[:], wantHash[:])
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
