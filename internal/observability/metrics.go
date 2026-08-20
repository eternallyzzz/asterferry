package observability

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
)

type Metrics struct {
	Connections     atomic.Int64
	ActiveStreams   atomic.Int64
	BytesIn         atomic.Uint64
	BytesOut        atomic.Uint64
	AuthFailures    atomic.Uint64
	MappingFailures atomic.Uint64
}

type Server struct {
	httpServer *http.Server
	listener   net.Listener
	Metrics    *Metrics
}

func Start(listen string, metrics *Metrics, status func() any, ready func() bool) (*Server, error) {
	if metrics == nil {
		metrics = &Metrics{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready != nil && !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { writeMetrics(w, metrics) })
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status == nil {
			_, _ = w.Write([]byte("{}\n"))
			return
		}
		b, err := json.Marshal(status())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(b)
	})
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	hs := &http.Server{Addr: listen, Handler: mux}
	s := &Server{httpServer: hs, listener: ln, Metrics: metrics}
	go func() { _ = hs.Serve(ln) }()
	return s, nil
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
	f("asterferry_bytes_in_total", m.BytesIn.Load())
	f("asterferry_bytes_out_total", m.BytesOut.Load())
	f("asterferry_auth_failures_total", m.AuthFailures.Load())
	f("asterferry_mapping_failures_total", m.MappingFailures.Load())
}
