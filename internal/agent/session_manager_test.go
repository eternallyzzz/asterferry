package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"asterferry/internal/transport"
)

func TestSessionManagerWaitAndClear(t *testing.T) {
	m := newSessionManager(context.Background(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sess := &Session{}
	if !m.set(sess) {
		t.Fatal("session should be accepted")
	}
	got, err := m.wait(context.Background())
	if err != nil || got != sess {
		t.Fatalf("wait returned %p, %v", got, err)
	}
	m.clear(sess)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected wait timeout, got %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionManagerReconnectLoopStops(t *testing.T) {
	m := newSessionManager(context.Background(), func(context.Context) (transport.Session, *Session, error) {
		return nil, nil, errors.New("dial failed")
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Start()
	deadline := time.Now().Add(time.Second)
	for m.Reconnects() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Reconnects() == 0 {
		t.Fatal("reconnect loop did not attempt a connection")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}
