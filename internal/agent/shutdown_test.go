package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/lifecycle"
	"asterferry/internal/observability"
)

func testAgentRuntime() *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	return &Agent{
		cfg:     &config.AgentOptions{Shutdown: config.ShutdownOptions{GracePeriod: 30 * time.Second}},
		ctx:     ctx,
		cancel:  cancel,
		life:    lifecycle.NewGate(),
		metrics: &observability.Metrics{},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestAgentShutdownTransitionsAndRecordsMetrics(t *testing.T) {
	a := testAgentRuntime()
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.life.State(); got != lifecycle.StateStopped {
		t.Fatalf("state = %s, want stopped", got)
	}
	if a.IsReady() {
		t.Fatal("stopped agent must not be ready")
	}
	if got := a.metrics.Shutdowns.Load(); got != 1 {
		t.Fatalf("shutdown count = %d, want 1", got)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal("repeated shutdown: ", err)
	}
	if got := a.metrics.Shutdowns.Load(); got != 1 {
		t.Fatalf("repeated shutdown count = %d, want 1", got)
	}
}

func TestAgentShutdownForcesAfterDeadline(t *testing.T) {
	a := testAgentRuntime()
	if !a.life.TryAdd() {
		t.Fatal("failed to admit test work")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := a.Shutdown(ctx); err == nil {
		t.Fatal("expected drain deadline")
	}
	if got := a.metrics.ForcedShutdowns.Load(); got != 1 {
		t.Fatalf("forced shutdown count = %d, want 1", got)
	}
	a.life.Done()
}
