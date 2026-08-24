package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"asterferry/internal/lifecycle"
	"asterferry/internal/observability"
	"asterferry/internal/transport"
)

type sessionConnector func(context.Context) (transport.Session, *Session, error)

type sessionManager struct {
	ctx     context.Context
	cancel  context.CancelFunc
	connect sessionConnector
	logger  *slog.Logger

	mu               sync.RWMutex
	session          *Session
	ready            chan struct{}
	reconnect        chan struct{}
	connects         atomic.Int64
	reconnectPending atomic.Bool
	retryBase        time.Duration
	closeOnce        sync.Once
	draining         atomic.Bool
}

func newSessionManager(parent context.Context, connect sessionConnector, logger *slog.Logger) *sessionManager {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	if logger == nil {
		logger = slog.Default()
	}
	return &sessionManager{ctx: ctx, cancel: cancel, connect: connect, logger: logger, ready: make(chan struct{}), reconnect: make(chan struct{}, 1)}
}

func (m *sessionManager) Start() {
	if m == nil || m.connect == nil {
		return
	}
	go m.connectLoop()
}

func (m *sessionManager) connectLoop() {
	backoff := m.retryDelay()
	for {
		if m.ctx.Err() != nil || m.draining.Load() {
			return
		}
		m.connects.Add(1)
		conn, sess, err := m.connect(m.ctx)
		if err == nil {
			if m.ctx.Err() != nil || m.draining.Load() {
				sess.Close()
				_ = transport.CloseSession(conn)
				return
			}
			backoff = m.retryDelay()
			if !m.set(sess) {
				_ = transport.CloseSession(conn)
				return
			}
			if m.clearReconnectRequest() {
				m.logger.Info("dashboard reconnect completed", "event", "management.action.completed", "action", "reconnect", "security_audit", true)
			}
			if waitErr := sess.Wait(); waitErr != nil && m.ctx.Err() == nil {
				m.logger.Info("QUIC session ended", "event", "agent.session.ended", "error_kind", lifecycle.ErrorKind(waitErr), "session_id", sess.sessionID, "node_id", sess.agent.nodeID)
			}
			m.clear(sess)
			_ = transport.CloseSession(conn)
			if m.draining.Load() {
				return
			}
			continue
		}
		if m.ctx.Err() != nil || m.draining.Load() {
			return
		}
		m.logger.Info("gateway connection failed", "event", "agent.gateway.connect_failed", "error_kind", lifecycle.ErrorKind(err))
		timer := time.NewTimer(backoff + time.Duration(m.connects.Load()%5)*100*time.Millisecond)
		select {
		case <-timer.C:
		case <-m.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-m.reconnect:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			m.clearReconnectRequest()
			backoff = m.retryDelay()
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (m *sessionManager) retryDelay() time.Duration {
	if m != nil && m.retryBase > 0 {
		return m.retryBase
	}
	return time.Second
}

func (m *sessionManager) set(sess *Session) bool {
	if sess == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil || m.draining.Load() {
		sess.Close()
		return false
	}
	if m.session != nil {
		m.session.Close()
	}
	m.session = sess
	close(m.ready)
	return true
}

func (m *sessionManager) clear(sess *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session == sess {
		m.session = nil
		m.ready = make(chan struct{})
	}
}

func (m *sessionManager) wait(ctx context.Context) (*Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		m.mu.RLock()
		sess, ready := m.session, m.ready
		m.mu.RUnlock()
		if sess != nil {
			return sess, nil
		}
		if m.draining.Load() {
			return nil, errors.New("agent session manager is draining")
		}
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.ctx.Done():
			return nil, m.ctx.Err()
		}
	}
}

// BeginDrain stops reconnect attempts but leaves the current control session
// alive so already admitted streams can finish.
func (m *sessionManager) BeginDrain() {
	if m != nil {
		m.draining.Store(true)
	}
}

func (m *sessionManager) IsReady() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.session != nil
}

func (m *sessionManager) Reconnects() int64 {
	if m == nil {
		return 0
	}
	return m.connects.Load()
}

// RequestReconnect closes the current session and wakes a pending retry. The
// request is intentionally coalesced so a dashboard button cannot create a
// reconnect storm.
func (m *sessionManager) RequestReconnect() error {
	if m == nil || m.ctx.Err() != nil || m.draining.Load() {
		return observability.ErrActionUnavailable
	}
	m.mu.RLock()
	sess := m.session
	m.mu.RUnlock()
	if sess != nil {
		if !m.reconnectPending.CompareAndSwap(false, true) {
			return observability.ErrActionBusy
		}
		sess.Close()
		return nil
	}
	if !m.reconnectPending.CompareAndSwap(false, true) {
		return observability.ErrActionBusy
	}
	select {
	case m.reconnect <- struct{}{}:
		return nil
	default:
		m.reconnectPending.Store(false)
		return observability.ErrActionBusy
	}
}

func (m *sessionManager) clearReconnectRequest() bool {
	if m == nil {
		return false
	}
	wasPending := m.reconnectPending.Swap(false)
	select {
	case <-m.reconnect:
	default:
	}
	return wasPending
}

func (m *sessionManager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.cancel()
		m.mu.Lock()
		sess := m.session
		m.session = nil
		m.ready = make(chan struct{})
		m.mu.Unlock()
		if sess != nil {
			sess.Close()
		}
	})
	return nil
}
