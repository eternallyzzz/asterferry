package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type deadlineResponseWriter struct {
	header   http.Header
	deadline time.Time
}

func (w *deadlineResponseWriter) Header() http.Header { return w.header }

func (w *deadlineResponseWriter) Write(data []byte) (int, error) { return len(data), nil }

func (w *deadlineResponseWriter) WriteHeader(int) {}

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestHTTPWriteDeadlineSeparatesStreamingAndOrdinaryRequestsContract(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	normalWriter := &deadlineResponseWriter{header: make(http.Header)}
	normalRequest := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	started := time.Now()
	httpWriteDeadlineMiddleware(next).ServeHTTP(normalWriter, normalRequest)
	if normalWriter.deadline.IsZero() || !normalWriter.deadline.After(started) {
		t.Fatalf("ordinary request deadline = %v, want a future deadline", normalWriter.deadline)
	}

	sseWriter := &deadlineResponseWriter{header: make(http.Header)}
	sseRequest := httptest.NewRequest(http.MethodGet, "/api/v1/runtime/stream", nil)
	httpWriteDeadlineMiddleware(next).ServeHTTP(sseWriter, sseRequest)
	if !sseWriter.deadline.IsZero() {
		t.Fatalf("SSE request deadline = %v, want no deadline", sseWriter.deadline)
	}
}

func TestControllerEndpointAuthenticationPolicyContract(t *testing.T) {
	server := &Server{metrics: newControllerMetrics()}
	handler := server.Handler()

	for _, path := range []string{"/metrics"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s status = %d, want %d", path, recorder.Code, http.StatusUnauthorized)
		}
	}
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("standalone /readyz status = %d, want %d", ready.Code, http.StatusServiceUnavailable)
	}
	if ready.Body.String() != `{"ready":false}`+"\n" {
		t.Fatalf("standalone /readyz body = %q, want minimal not-ready response", ready.Body.String())
	}

	for _, path := range []string{"/openapi.yaml", "/api/v1/openapi.yaml"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("anonymous %s status = %d, want %d", path, recorder.Code, http.StatusOK)
		}
		if recorder.Body.Len() == 0 {
			t.Fatalf("anonymous %s returned an empty document", path)
		}
	}
}
