package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/lifecycle"
	"asterferry/internal/observability"
	"asterferry/internal/transport"
)

type failingGatewayListener struct{}

func (failingGatewayListener) Accept(context.Context) (transport.Session, error) {
	return nil, errors.New("accept failed")
}
func (failingGatewayListener) StopAccepting() error { return nil }
func (failingGatewayListener) Close() error         { return nil }

func testGatewayRuntime() *Gateway {
	ctx, cancel := context.WithCancel(context.Background())
	return &Gateway{
		cfg:     &config.GatewayOptions{Shutdown: config.ShutdownOptions{GracePeriod: 30 * time.Second}},
		ctx:     ctx,
		cancel:  cancel,
		life:    lifecycle.NewGate(),
		metrics: &observability.Metrics{},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestGatewayShutdownTransitionsAndRecordsMetrics(t *testing.T) {
	g := testGatewayRuntime()
	if err := g.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := g.life.State(); got != lifecycle.StateStopped {
		t.Fatalf("state = %s, want stopped", got)
	}
	if g.IsReady() {
		t.Fatal("stopped gateway must not be ready")
	}
	if got := g.metrics.Shutdowns.Load(); got != 1 {
		t.Fatalf("shutdown count = %d, want 1", got)
	}
}

func TestGatewayShutdownForcesAfterDeadline(t *testing.T) {
	g := testGatewayRuntime()
	if !g.life.TryAdd() {
		t.Fatal("failed to admit test work")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := g.Shutdown(ctx); err == nil {
		t.Fatal("expected drain deadline")
	}
	if got := g.metrics.ForcedShutdowns.Load(); got != 1 {
		t.Fatalf("forced shutdown count = %d, want 1", got)
	}
	g.life.Done()
}

func TestGatewayAcceptLoopFailureMarksGatewayNotReady(t *testing.T) {
	g := testGatewayRuntime()
	g.ep = failingGatewayListener{}
	g.accepting.Store(true)
	g.shutdownTrigger = lifecycle.NewShutdownTrigger()
	go g.acceptLoop()
	select {
	case <-g.shutdownTrigger.C():
	case <-time.After(time.Second):
		t.Fatal("accept loop failure did not request shutdown")
	}
	if g.IsReady() {
		t.Fatal("gateway must not remain ready after its accept loop exits")
	}
}
