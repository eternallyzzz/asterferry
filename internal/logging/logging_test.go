package logging

import (
	"bytes"
	"encoding/json"
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
