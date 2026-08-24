package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
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

func TestSessionManagerBeginDrainRejectsWait(t *testing.T) {
	m := newSessionManager(context.Background(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.BeginDrain()
	if _, err := m.wait(context.Background()); err == nil || err.Error() != "agent session manager is draining" {
		t.Fatalf("draining wait error = %v", err)
	}
	var nilManager *sessionManager
	nilManager.BeginDrain()
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

func TestSessionManagerReconnectsAfterSessionLoss(t *testing.T) {
	var attempts atomic.Int32
	closed := make(chan *managerFakeSession, 2)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := newSessionManager(context.Background(), func(ctx context.Context) (transport.Session, *Session, error) {
		if attempts.Add(1) == 1 {
			return nil, nil, errors.New("initial dial failed")
		}
		fake := newManagerFakeSession(ctx)
		sessionCtx, cancel := context.WithCancel(fake.Context())
		sess := &Session{agent: &Agent{logger: logger}, conn: fake, ctx: sessionCtx, cancel: cancel}
		go func() {
			<-fake.Context().Done()
			cancel()
		}()
		closed <- fake
		return fake, sess, nil
	}, logger)
	m.retryBase = 5 * time.Millisecond
	m.Start()
	var first *managerFakeSession
	select {
	case first = <-closed:
	case <-time.After(time.Second):
		t.Fatal("session was not established after initial failure")
	}
	_ = first.Close()
	deadline := time.Now().Add(time.Second)
	for attempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := attempts.Load(); got < 3 {
		t.Fatalf("attempts after session loss = %d, want at least 3", got)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionManagerRequestReconnectCoalescesAndClosesSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := newSessionManager(context.Background(), nil, logger)
	fake := newManagerFakeSession(context.Background())
	sessionCtx, cancel := context.WithCancel(fake.Context())
	sess := &Session{agent: &Agent{logger: logger}, conn: fake, ctx: sessionCtx, cancel: cancel}
	if !m.set(sess) {
		t.Fatal("session should be accepted")
	}
	if err := m.RequestReconnect(); err != nil {
		t.Fatal(err)
	}
	if err := m.RequestReconnect(); err == nil {
		t.Fatal("second reconnect request should be coalesced")
	}
	select {
	case <-fake.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("reconnect did not close current session")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

type managerFakeSession struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func newManagerFakeSession(parent context.Context) *managerFakeSession {
	ctx, cancel := context.WithCancel(parent)
	return &managerFakeSession{ctx: ctx, cancel: cancel}
}

func (s *managerFakeSession) OpenStream(context.Context) (transport.Stream, error) {
	return nil, errors.New("fake stream unavailable")
}

func (s *managerFakeSession) AcceptStream(context.Context) (transport.Stream, error) {
	return nil, errors.New("fake stream unavailable")
}

func (s *managerFakeSession) Context() context.Context { return s.ctx }

func (s *managerFakeSession) RemoteAddr() net.Addr { return nil }

func (s *managerFakeSession) PeerCertificates() []*x509.Certificate { return nil }

func (s *managerFakeSession) Close() error {
	s.closeOnce.Do(s.cancel)
	return nil
}
