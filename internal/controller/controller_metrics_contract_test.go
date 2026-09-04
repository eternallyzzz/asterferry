package controller

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type capabilityResponseWriter struct {
	header       http.Header
	status       int
	body         bytes.Buffer
	flushed      bool
	hijacked     bool
	readFromUsed bool
	pushTarget   string
}

func (w *capabilityResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *capabilityResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *capabilityResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(data)
}

func (w *capabilityResponseWriter) Flush() {
	w.flushed = true
}

func (w *capabilityResponseWriter) FlushError() error {
	w.flushed = true
	return nil
}

func (w *capabilityResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	left, right := net.Pipe()
	_ = right.Close()
	return left, bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), bufio.NewWriter(io.Discard)), nil
}

func (w *capabilityResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	w.readFromUsed = true
	return io.Copy(&w.body, reader)
}

func (w *capabilityResponseWriter) Push(target string, _ *http.PushOptions) error {
	w.pushTarget = target
	return nil
}

type bareResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bareResponseWriter) Header() http.Header { return w.header }

func (w *bareResponseWriter) WriteHeader(status int) { w.status = status }

func (w *bareResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func TestMetricsResponseWriterPreservesOptionalCapabilitiesContract(t *testing.T) {
	underlying := &capabilityResponseWriter{}
	wrapper := &metricsResponseWriter{ResponseWriter: underlying}

	if wrapper.Unwrap() != underlying {
		t.Fatal("Unwrap did not return the underlying response writer")
	}
	if err := http.NewResponseController(wrapper).Flush(); err != nil {
		t.Fatalf("ResponseController.Flush() error = %v", err)
	}
	if !underlying.flushed || underlying.status != http.StatusOK {
		t.Fatalf("Flush was not forwarded with an implicit 200: flushed=%v status=%d", underlying.flushed, underlying.status)
	}
	if _, _, err := wrapper.Hijack(); err != nil {
		t.Fatalf("Hijack() error = %v", err)
	}
	if !underlying.hijacked {
		t.Fatal("Hijack was not forwarded")
	}
	if _, err := wrapper.ReadFrom(strings.NewReader("streamed")); err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if !underlying.readFromUsed || underlying.body.String() != "streamed" {
		t.Fatalf("ReadFrom was not forwarded: used=%v body=%q", underlying.readFromUsed, underlying.body.String())
	}
	if err := wrapper.Push("/asset", nil); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if underlying.pushTarget != "/asset" {
		t.Fatalf("Push target = %q, want /asset", underlying.pushTarget)
	}
}

func TestMetricsResponseWriterReportsUnsupportedCapabilitiesContract(t *testing.T) {
	underlying := &bareResponseWriter{header: make(http.Header)}
	wrapper := &metricsResponseWriter{ResponseWriter: underlying}

	if err := wrapper.FlushError(); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("FlushError() = %v, want http.ErrNotSupported", err)
	}
	if _, _, err := wrapper.Hijack(); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("Hijack() = %v, want http.ErrNotSupported", err)
	}
	if err := wrapper.Push("/asset", nil); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("Push() = %v, want http.ErrNotSupported", err)
	}
	if _, err := wrapper.ReadFrom(strings.NewReader("fallback")); err != nil {
		t.Fatalf("ReadFrom fallback error = %v", err)
	}
	if underlying.status != http.StatusOK || underlying.body.String() != "fallback" {
		t.Fatalf("ReadFrom fallback wrote status=%d body=%q", underlying.status, underlying.body.String())
	}
}
