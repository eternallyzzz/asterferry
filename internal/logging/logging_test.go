package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"log/slog"

	"asterferry/internal/config"
)

func TestSamplingKeepsSecurityLevels(t *testing.T) {
	var out bytes.Buffer
	logger, closeLog, err := New(config.LoggingOptions{
		Level:  "debug",
		Format: "json",
		Sampling: config.SamplingOptions{
			Enabled:            true,
			RatePerSecond:      1,
			Burst:              1,
			SummaryIntervalSec: 60,
			MaxKeys:            64,
		},
	}, "agent", &out)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	for i := 0; i < 10; i++ {
		logger.Info("repeated", "event", "proxy.request", "inbound", "socks")
	}
	logger.Warn("audit", "event", "gateway.auth.rejected", "security_audit", true)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one sampled info and one warning, got %d lines: %s", len(lines), out.String())
	}
	var warning map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &warning); err != nil {
		t.Fatal(err)
	}
	if warning["level"] != "WARN" || warning["event"] != "gateway.auth.rejected" {
		t.Fatalf("unexpected warning record: %#v", warning)
	}
}

func TestSamplingSummaryAndKeyBound(t *testing.T) {
	state := newSamplerState(config.SamplingOptions{Enabled: true, RatePerSecond: 1, Burst: 1, SummaryIntervalSec: 1, MaxKeys: 64})
	now := time.Now()
	if ok, _ := state.allow(slog.LevelInfo, "event|agent|one", now); !ok {
		t.Fatal("first record should use burst token")
	}
	if ok, _ := state.allow(slog.LevelInfo, "event|agent|one", now); ok {
		t.Fatal("second immediate record should be suppressed")
	}
	if ok, summary := state.allow(slog.LevelInfo, "event|agent|one", now.Add(2*time.Second)); !ok || len(summary) == 0 {
		t.Fatal("expected refill and suppression summary")
	}
	for i := 0; i < 200; i++ {
		state.allow(slog.LevelInfo, "event|agent|"+string(rune(i+1)), now)
	}
	state.mu.Lock()
	keys := len(state.buckets)
	state.mu.Unlock()
	if keys > 64 {
		t.Fatalf("sampler exceeded key bound: %d", keys)
	}
}

func TestDomainHashDoesNotContainDomain(t *testing.T) {
	domain := "private.example.internal"
	hash := DomainHash(domain)
	if hash == "" || strings.Contains(hash, domain) || len(hash) != 16 {
		t.Fatalf("unexpected domain hash %q", hash)
	}
	if hash != DomainHash(strings.ToUpper(domain)+".") {
		t.Fatal("domain normalization should be stable")
	}
}

func TestLoggingValidationAndHandlerComposition(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "warning", "error", ""} {
		if _, err := parseLevel(level); err != nil {
			t.Fatalf("level %q failed: %v", level, err)
		}
	}
	if _, err := parseLevel("trace"); err == nil {
		t.Fatal("unknown logging level should fail")
	}
	if _, _, err := New(config.LoggingOptions{Level: "info", Format: "xml"}, "agent", io.Discard); err == nil {
		t.Fatal("unknown logging format should fail")
	}
	var out bytes.Buffer
	logger, closeLog, err := New(config.LoggingOptions{Level: "info", Format: "text", Sampling: config.SamplingOptions{Enabled: false}}, "agent", &out)
	if err != nil {
		t.Fatal(err)
	}
	logger.With("entity", "edge").WithGroup("proxy").Info("request", "event", "proxy.request")
	if err := closeLog(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "proxy") || !strings.Contains(out.String(), "proxy.request") {
		t.Fatalf("composed logger output = %q", out.String())
	}
}

func TestSamplingHandlerEmitsPeriodicAndFinalSummaries(t *testing.T) {
	var out bytes.Buffer
	state := newSamplerState(config.SamplingOptions{
		Enabled:            true,
		RatePerSecond:      1,
		Burst:              1,
		SummaryIntervalSec: 1,
		MaxKeys:            8,
	})
	h := &samplingHandler{next: slog.NewTextHandler(&out, nil), state: state}
	now := time.Now()
	first := slog.NewRecord(now, slog.LevelInfo, "first", 0)
	first.AddAttrs(slog.String("event", "test.sample"))
	second := slog.NewRecord(now, slog.LevelInfo, "second", 0)
	second.AddAttrs(slog.String("event", "test.sample"))
	later := slog.NewRecord(now.Add(2*time.Second), slog.LevelInfo, "later", 0)
	later.AddAttrs(slog.String("event", "test.sample"))
	if err := h.Handle(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "log records suppressed") || !strings.Contains(got, "suppressed_total=1") {
		t.Fatalf("periodic suppression summary = %q", got)
	}

	var finalOut bytes.Buffer
	finalState := newSamplerState(config.SamplingOptions{Enabled: true, RatePerSecond: 1, Burst: 1, SummaryIntervalSec: 60, MaxKeys: 8})
	finalHandler := &samplingHandler{next: slog.NewTextHandler(&finalOut, nil), state: finalState}
	if err := finalHandler.Handle(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := finalHandler.Handle(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := finalHandler.close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(finalOut.String(), "suppressed_total=1") {
		t.Fatalf("final suppression summary = %q", finalOut.String())
	}
}
