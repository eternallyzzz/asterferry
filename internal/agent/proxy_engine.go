package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"asterferry/internal/config"
	"asterferry/internal/lifecycle"
)

// ProxyEngine owns local proxy listener lifecycle. Protocol handling is
// injected so the listener component does not know about Agent session state
// or QUIC transport details.
type ProxyEngine struct {
	inbounds []config.Inbound
	handler  func(net.Conn, config.Inbound)

	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	listeners []net.Listener
	closed    bool
	draining  bool
	stopOnce  sync.Once
	stopErr   error
	gate      *lifecycle.Gate
}

type ProxyEngineOptions struct {
	Inbounds []config.Inbound
	Handler  func(net.Conn, config.Inbound)
	Gate     *lifecycle.Gate
}

func NewProxyEngine(opts ProxyEngineOptions) (*ProxyEngine, error) {
	if opts.Handler == nil {
		return nil, errors.New("proxy engine handler is required")
	}
	return &ProxyEngine{inbounds: append([]config.Inbound(nil), opts.Inbounds...), handler: opts.Handler, gate: opts.Gate}, nil
}

func (e *ProxyEngine) Start(parent context.Context) error {
	if e == nil {
		return errors.New("proxy engine is nil")
	}
	if parent == nil {
		parent = context.Background()
	}
	e.mu.Lock()
	if e.cancel != nil || e.closed || e.draining {
		e.mu.Unlock()
		return errors.New("proxy engine already started or closed")
	}
	e.ctx, e.cancel = context.WithCancel(parent)
	e.mu.Unlock()
	go func(ctx context.Context) {
		<-ctx.Done()
		_ = e.Close()
	}(e.ctx)
	for _, in := range e.inbounds {
		listener, err := net.Listen("tcp", in.Listen)
		if err != nil {
			_ = e.Close()
			return fmt.Errorf("listen %s: %w", in.Tag, err)
		}
		e.mu.Lock()
		if e.closed || e.draining {
			e.mu.Unlock()
			_ = listener.Close()
			_ = e.Close()
			return errors.New("proxy engine closed during startup")
		}
		e.listeners = append(e.listeners, listener)
		e.mu.Unlock()
		go e.acceptLoop(listener, in)
	}
	return nil
}

func (e *ProxyEngine) acceptLoop(listener net.Listener, in config.Inbound) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if e.gate != nil && !e.gate.TryAdd() {
			_ = conn.Close()
			continue
		}
		go func() {
			if e.gate != nil {
				defer e.gate.Done()
			}
			e.handler(conn, in)
		}()
	}
}

// BeginDrain closes local proxy listeners while allowing already accepted
// connections to finish. Close remains the hard-stop path.
func (e *ProxyEngine) BeginDrain() error {
	if e == nil {
		return nil
	}
	e.stopOnce.Do(func() {
		e.mu.Lock()
		e.draining = true
		listeners := append([]net.Listener(nil), e.listeners...)
		e.listeners = nil
		e.mu.Unlock()
		var first error
		for _, listener := range listeners {
			if err := listener.Close(); err != nil && first == nil {
				first = err
			}
		}
		e.mu.Lock()
		e.stopErr = first
		e.mu.Unlock()
	})
	e.mu.Lock()
	err := e.stopErr
	e.mu.Unlock()
	return err
}

func (e *ProxyEngine) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	if e.cancel != nil {
		e.cancel()
	}
	e.mu.Unlock()
	return e.BeginDrain()
}
