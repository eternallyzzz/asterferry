package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"asterferry/internal/config"
)

const (
	serviceName  = "asterferry"
	defaultEvent = "log.uncategorized"
)

// New builds the process logger. The returned close function emits the final
// suppression summary; slog handlers write synchronously, so no background
// queue needs to be drained.
func New(cfg config.LoggingOptions, role string, out io.Writer) (*slog.Logger, func() error, error) {
	if out == nil {
		out = os.Stderr
	}
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, nil, err
	}
	options := &slog.HandlerOptions{Level: level, AddSource: false}
	var next slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "json", "":
		next = slog.NewJSONHandler(out, options)
	case "text":
		next = slog.NewTextHandler(out, options)
	default:
		return nil, nil, fmt.Errorf("unsupported log format %q", cfg.Format)
	}
	state := newSamplerState(cfg.Sampling)
	baseAttrs := []slog.Attr{
		slog.String("service", serviceName),
		slog.String("role", role),
	}
	h := &samplingHandler{next: next.WithAttrs(baseAttrs), state: state, attrs: baseAttrs}
	logger := slog.New(h)
	return logger, h.close, nil
}

func parseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", value)
	}
}

type samplerState struct {
	mu       sync.Mutex
	config   config.SamplingOptions
	buckets  map[string]*bucket
	dropped  map[string]uint64
	nextEmit time.Time
	closed   bool
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newSamplerState(cfg config.SamplingOptions) *samplerState {
	now := time.Now()
	return &samplerState{
		config:   cfg,
		buckets:  make(map[string]*bucket),
		dropped:  make(map[string]uint64),
		nextEmit: now.Add(time.Duration(cfg.SummaryIntervalSec) * time.Second),
	}
}

func (s *samplerState) allow(level slog.Level, key string, now time.Time) (bool, []slog.Attr) {
	enabled := s.config.Enabled
	if level >= slog.LevelWarn || !enabled {
		return true, nil
	}
	if key == "" {
		key = defaultEvent
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return true, nil
	}
	if _, exists := s.buckets[key]; !exists && len(s.buckets) >= int(s.config.MaxKeys)-1 {
		key = "log.key_overflow"
	}
	b := s.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(s.config.Burst), last: now}
		s.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * float64(s.config.RatePerSecond)
		if b.tokens > float64(s.config.Burst) {
			b.tokens = float64(s.config.Burst)
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		summary := s.takeSummaryLocked(now)
		s.mu.Unlock()
		return true, summary
	}
	s.dropped[key]++
	summary := s.takeSummaryLocked(now)
	s.mu.Unlock()
	return false, summary
}

func (s *samplerState) takeSummaryLocked(now time.Time) []slog.Attr {
	if now.Before(s.nextEmit) || len(s.dropped) == 0 {
		return nil
	}
	attrs := make([]slog.Attr, 0, len(s.dropped)*2)
	var total uint64
	for key, count := range s.dropped {
		attrs = append(attrs, slog.Group("suppressed", slog.String("key", key), slog.Uint64("count", count)))
		total += count
	}
	attrs = append([]slog.Attr{slog.Uint64("suppressed_total", total)}, attrs...)
	s.dropped = make(map[string]uint64)
	s.nextEmit = now.Add(time.Duration(s.config.SummaryIntervalSec) * time.Second)
	return attrs
}

func (s *samplerState) finalSummary() []slog.Attr {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	attrs := make([]slog.Attr, 0, len(s.dropped)*2+1)
	var total uint64
	for key, count := range s.dropped {
		attrs = append(attrs, slog.Group("suppressed", slog.String("key", key), slog.Uint64("count", count)))
		total += count
	}
	s.dropped = nil
	s.mu.Unlock()
	if total == 0 {
		return nil
	}
	return append([]slog.Attr{slog.Uint64("suppressed_total", total)}, attrs...)
}

type samplingHandler struct {
	next  slog.Handler
	state *samplerState
	attrs []slog.Attr
}

func (h *samplingHandler) close() error {
	attrs := h.state.finalSummary()
	if len(attrs) == 0 {
		return nil
	}
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "log records suppressed", 0)
	r.AddAttrs(slog.String("event", "log.suppression_summary"))
	r.AddAttrs(attrs...)
	return h.next.Handle(context.Background(), r)
}

func (h *samplingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *samplingHandler) Handle(ctx context.Context, record slog.Record) error {
	key := sampleKey(h.attrs, record)
	allowed, summary := h.state.allow(record.Level, key, record.Time)
	if err := h.emitSummary(ctx, record.Time, summary); err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	return h.next.Handle(ctx, record)
}

func (h *samplingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cloned := append([]slog.Attr(nil), h.attrs...)
	cloned = append(cloned, attrs...)
	return &samplingHandler{next: h.next.WithAttrs(attrs), state: h.state, attrs: cloned}
}

func (h *samplingHandler) WithGroup(name string) slog.Handler {
	return &samplingHandler{next: h.next.WithGroup(name), state: h.state, attrs: h.attrs}
}

func (h *samplingHandler) emitSummary(ctx context.Context, now time.Time, attrs []slog.Attr) error {
	if len(attrs) == 0 {
		return nil
	}
	r := slog.NewRecord(now, slog.LevelInfo, "log records suppressed", 0)
	r.AddAttrs(slog.String("event", "log.suppression_summary"))
	r.AddAttrs(attrs...)
	return h.next.Handle(ctx, r)
}

func sampleKey(base []slog.Attr, record slog.Record) string {
	var event, role, entity string
	read := func(attr slog.Attr) bool {
		if attr.Value.Kind() != slog.KindString {
			return true
		}
		value := attr.Value.String()
		switch attr.Key {
		case "event":
			event = value
		case "role":
			role = value
		case "entity", "agent_id", "inbound", "mapping":
			if entity == "" {
				entity = value
			}
		}
		return true
	}
	for _, attr := range base {
		read(attr)
	}
	record.Attrs(read)
	if event == "" {
		event = defaultEvent
	}
	return event + "|" + role + "|" + entity
}
