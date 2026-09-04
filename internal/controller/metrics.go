package controller

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"asterferry/internal/domain"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ControllerMetrics contains only bounded-label metrics. Resource IDs and
// arbitrary observed map keys are intentionally never used as labels.
type ControllerMetrics struct {
	registry          *prometheus.Registry
	up                prometheus.Gauge
	databaseUp        *prometheus.GaugeVec
	sqliteUp          prometheus.Gauge
	databaseDriver    string
	httpReq           *prometheus.CounterVec
	httpTime          *prometheus.HistogramVec
	grpcReq           *prometheus.CounterVec
	streams           prometheus.Gauge
	schedRun          *prometheus.CounterVec
	schedTime         prometheus.Histogram
	observedNodes     *prometheus.GaugeVec
	snapshotGen       *prometheus.GaugeVec
	activeStreams     *prometheus.GaugeVec
	activeSessions    *prometheus.GaugeVec
	activeEgress      *prometheus.GaugeVec
	activeConnections *prometheus.GaugeVec
	activeFlows       *prometheus.GaugeVec
	bytesIn           *prometheus.GaugeVec
	bytesOut          *prometheus.GaugeVec
	listeners         *prometheus.GaugeVec
	geoipUp           *prometheus.GaugeVec
	mu                sync.Mutex
	nodes             map[string]observedMetric
}

type observedMetric struct {
	kind        string
	healthy     bool
	generation  uint64
	streams     float64
	sessions    float64
	egress      float64
	connections float64
	flows       float64
	bytesIn     float64
	bytesOut    float64
	geoipUp     bool
	listeners   map[string]int
}

func newControllerMetrics(databaseDrivers ...string) *ControllerMetrics {
	databaseDriver := DatabaseDriverSQLite
	if len(databaseDrivers) > 0 && strings.TrimSpace(databaseDrivers[0]) != "" {
		databaseDriver = normalizeDatabaseDriver(databaseDrivers[0])
	}
	m := &ControllerMetrics{registry: prometheus.NewRegistry(), nodes: make(map[string]observedMetric), databaseDriver: databaseDriver}
	m.up = prometheus.NewGauge(prometheus.GaugeOpts{Name: "asterferry_controller_up", Help: "Controller process health."})
	m.databaseUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_database_up", Help: "Whether the configured Controller database is reachable."}, []string{"backend"})
	m.sqliteUp = prometheus.NewGauge(prometheus.GaugeOpts{Name: "asterferry_controller_sqlite_up", Help: "Whether the Controller SQLite store is reachable."})
	m.httpReq = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "asterferry_controller_http_requests_total", Help: "Controller HTTP requests."}, []string{"method", "route", "status"})
	m.httpTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "asterferry_controller_http_request_duration_seconds", Help: "Controller HTTP request duration."}, []string{"method", "route"})
	m.grpcReq = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "asterferry_controller_grpc_requests_total", Help: "Controller gRPC messages and calls."}, []string{"method", "code"})
	m.streams = prometheus.NewGauge(prometheus.GaugeOpts{Name: "asterferry_controller_control_streams", Help: "Connected node control streams."})
	m.schedRun = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "asterferry_controller_scheduler_runs_total", Help: "Controller scheduling and reconciliation runs."}, []string{"result"})
	m.schedTime = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "asterferry_controller_scheduler_duration_seconds", Help: "Controller scheduling duration."})
	m.observedNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_observed_nodes", Help: "Observed node counts by behavior kind and health."}, []string{"kind", "health"})
	m.snapshotGen = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_snapshot_generation", Help: "Highest applied snapshot generation observed by node kind."}, []string{"kind"})
	m.activeStreams = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_node_active_streams", Help: "Observed active data streams by node kind."}, []string{"kind"})
	m.activeSessions = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_node_active_sessions", Help: "Observed active sessions by node kind."}, []string{"kind"})
	m.activeEgress = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_node_active_egress", Help: "Observed active egress connections by node kind."}, []string{"kind"})
	m.activeConnections = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_node_active_connections", Help: "Observed active runtime connections by node kind."}, []string{"kind"})
	m.activeFlows = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_node_active_flows", Help: "Observed active runtime flows by node kind."}, []string{"kind"})
	m.bytesIn = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_node_bytes_in_total", Help: "Observed runtime bytes received by node kind."}, []string{"kind"})
	m.bytesOut = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_node_bytes_out_total", Help: "Observed runtime bytes sent by node kind."}, []string{"kind"})
	m.listeners = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_node_listeners", Help: "Observed listeners by protocol."}, []string{"protocol"})
	m.geoipUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "asterferry_controller_geoip_up", Help: "Number of observed nodes with an available optional GeoIP database, by node kind."}, []string{"kind"})
	collectors := []prometheus.Collector{m.up, m.databaseUp, m.httpReq, m.httpTime, m.grpcReq, m.streams, m.schedRun, m.schedTime, m.observedNodes, m.snapshotGen, m.activeStreams, m.activeSessions, m.activeEgress, m.activeConnections, m.activeFlows, m.bytesIn, m.bytesOut, m.listeners, m.geoipUp}
	if databaseDriver == DatabaseDriverSQLite {
		collectors = append(collectors, m.sqliteUp)
	}
	for _, collector := range collectors {
		m.registry.MustRegister(collector)
	}
	m.up.Set(1)
	m.databaseUp.WithLabelValues(databaseDriver).Set(1)
	if databaseDriver == DatabaseDriverSQLite {
		m.sqliteUp.Set(1)
	}
	for _, kind := range []string{string(domain.NodeSpecGateway), string(domain.NodeSpecAgent)} {
		m.geoipUp.WithLabelValues(kind).Set(0)
	}
	return m
}

func (m *ControllerMetrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

func (m *ControllerMetrics) refreshDatabase(store *Repository) {
	if m == nil || store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.Ping(ctx); err != nil {
		m.databaseUp.WithLabelValues(m.databaseDriver).Set(0)
		if m.sqliteUp != nil && m.databaseDriver == DatabaseDriverSQLite {
			m.sqliteUp.Set(0)
		}
		return
	}
	m.databaseUp.WithLabelValues(m.databaseDriver).Set(1)
	if m.sqliteUp != nil && m.databaseDriver == DatabaseDriverSQLite {
		m.sqliteUp.Set(1)
	}
	if nodes, err := store.ListNodes(ctx, ""); err == nil {
		existing := make(map[string]struct{}, len(nodes))
		for _, node := range nodes {
			existing[node.ID] = struct{}{}
		}
		m.mu.Lock()
		for nodeID := range m.nodes {
			if _, ok := existing[nodeID]; !ok {
				delete(m.nodes, nodeID)
			}
		}
		m.recomputeLocked()
		m.mu.Unlock()
	}
}

func (m *ControllerMetrics) observeHTTP(method, route string, status int, elapsed time.Duration) {
	if m == nil {
		return
	}
	if route == "" {
		route = "unknown"
	}
	m.httpReq.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.httpTime.WithLabelValues(method, route).Observe(elapsed.Seconds())
}

func (m *ControllerMetrics) observeGRPC(method, code string) {
	if m != nil {
		m.grpcReq.WithLabelValues(method, code).Inc()
	}
}

func (m *ControllerMetrics) observeNode(nodeID, kind string, observed domain.ObservedState) {
	if m == nil {
		return
	}
	current := observedMetric{kind: kind, healthy: observed.Healthy && !observed.Degraded, generation: observed.AppliedGeneration, streams: float64(observed.Metrics.ActiveStreams), sessions: float64(observed.Metrics.ActiveSessions), egress: float64(observed.Metrics.ActiveEgress), connections: float64(observed.Metrics.ActiveConnections), flows: float64(observed.Metrics.ActiveFlows), bytesIn: float64(observed.Metrics.RuntimeBytesInTotal), bytesOut: float64(observed.Metrics.RuntimeBytesOutTotal), geoipUp: observed.Metrics.GeoIPUp, listeners: make(map[string]int)}
	for _, listener := range observed.Listeners {
		current.listeners[listener.Protocol]++
	}
	m.mu.Lock()
	m.nodes[nodeID] = current
	m.recomputeLocked()
	m.mu.Unlock()
}

func (m *ControllerMetrics) recomputeLocked() {
	byKind := make(map[string]struct{ healthy, degraded, streams, sessions, egress, connections, flows, bytesIn, bytesOut, generation, geoipUp float64 })
	listenerTotals := make(map[string]float64)
	for _, value := range m.nodes {
		aggregate := byKind[value.kind]
		if value.healthy {
			aggregate.healthy++
		} else {
			aggregate.degraded++
		}
		aggregate.streams += value.streams
		aggregate.sessions += value.sessions
		aggregate.egress += value.egress
		aggregate.connections += value.connections
		aggregate.flows += value.flows
		aggregate.bytesIn += value.bytesIn
		aggregate.bytesOut += value.bytesOut
		if value.geoipUp {
			aggregate.geoipUp++
		}
		if generation := float64(value.generation); generation > aggregate.generation {
			aggregate.generation = generation
		}
		byKind[value.kind] = aggregate
		for protocol, count := range value.listeners {
			listenerTotals[protocol] += float64(count)
		}
	}
	for kind, aggregate := range byKind {
		m.observedNodes.WithLabelValues(kind, "healthy").Set(aggregate.healthy)
		m.observedNodes.WithLabelValues(kind, "degraded").Set(aggregate.degraded)
		m.activeStreams.WithLabelValues(kind).Set(aggregate.streams)
		m.activeSessions.WithLabelValues(kind).Set(aggregate.sessions)
		m.activeEgress.WithLabelValues(kind).Set(aggregate.egress)
		m.activeConnections.WithLabelValues(kind).Set(aggregate.connections)
		m.activeFlows.WithLabelValues(kind).Set(aggregate.flows)
		m.bytesIn.WithLabelValues(kind).Set(aggregate.bytesIn)
		m.bytesOut.WithLabelValues(kind).Set(aggregate.bytesOut)
		m.snapshotGen.WithLabelValues(kind).Set(aggregate.generation)
		m.geoipUp.WithLabelValues(kind).Set(aggregate.geoipUp)
	}
	for _, kind := range []string{string(domain.NodeSpecGateway), string(domain.NodeSpecAgent)} {
		if _, ok := byKind[kind]; !ok {
			m.geoipUp.WithLabelValues(kind).Set(0)
		}
	}
	for _, protocol := range []string{"tcp", "udp", "http", "socks5"} {
		m.listeners.WithLabelValues(protocol).Set(listenerTotals[protocol])
	}
}

func (m *ControllerMetrics) startSchedule() func(error) {
	if m == nil {
		return func(error) {}
	}
	started := time.Now()
	return func(err error) {
		result := "success"
		if err != nil {
			result = "error"
		}
		m.schedRun.WithLabelValues(result).Inc()
		m.schedTime.Observe(time.Since(started).Seconds())
	}
}

func routeLabel(path string) string {
	if path == "/healthz" || path == "/readyz" || path == "/metrics" || path == "/openapi.yaml" || path == "/api/v1/openapi.yaml" {
		return path
	}
	if len(path) >= len("/api/v1/") && path[:len("/api/v1/")] == "/api/v1/" {
		for _, prefix := range []string{"auth/login", "auth/logout", "me", "nodes", "node-installations", "services", "assignments", "enrollment-tokens", "audit", "events", "runtime", "users"} {
			base := "/api/v1/" + prefix
			if path == base || (len(path) > len(base) && path[:len(base)+1] == base+"/") {
				return base
			}
		}
	}
	return "other"
}

type metricsResponseWriter struct {
	http.ResponseWriter
	status int
}

type responseFlusher interface {
	FlushError() error
}

var (
	_ http.Flusher  = (*metricsResponseWriter)(nil)
	_ http.Hijacker = (*metricsResponseWriter)(nil)
	_ http.Pusher   = (*metricsResponseWriter)(nil)
	_ io.ReaderFrom = (*metricsResponseWriter)(nil)
)

func (w *metricsResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *metricsResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

// Unwrap lets http.ResponseController reach optional capabilities exposed by
// the underlying writer even though this middleware records response metrics.
func (w *metricsResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// FlushError is the error-returning form used by http.ResponseController.
// Keep Flush below for handlers that use the legacy http.Flusher interface.
func (w *metricsResponseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(responseFlusher); ok {
		return flusher.FlushError()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
		return nil
	}
	return http.ErrNotSupported
}

func (w *metricsResponseWriter) Flush() {
	_ = w.FlushError()
}

func (w *metricsResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (w *metricsResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(reader)
	}
	return io.Copy(w.ResponseWriter, reader)
}

func (w *metricsResponseWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

func (m *ControllerMetrics) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		wrapped := &metricsResponseWriter{ResponseWriter: w}
		next.ServeHTTP(wrapped, r)
		status := wrapped.status
		if status == 0 {
			status = http.StatusOK
		}
		m.observeHTTP(r.Method, routeLabel(r.URL.Path), status, time.Since(started))
	})
}
