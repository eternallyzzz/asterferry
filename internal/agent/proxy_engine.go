package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"asterferry/internal/config"
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
}

type ProxyEngineOptions struct {
	Inbounds []config.Inbound
	Handler  func(net.Conn, config.Inbound)
}

func NewProxyEngine(opts ProxyEngineOptions) (*ProxyEngine, error) {
	if opts.Handler == nil {
		return nil, errors.New("proxy engine handler is required")
	}
	return &ProxyEngine{inbounds: append([]config.Inbound(nil), opts.Inbounds...), handler: opts.Handler}, nil
}

func (e *ProxyEngine) Start(parent context.Context) error {
	if e == nil {
		return errors.New("proxy engine is nil")
	}
	if parent == nil {
		parent = context.Background()
	}
	e.mu.Lock()
	if e.cancel != nil || e.closed {
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
		if e.closed {
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
		go e.handler(conn, in)
	}
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
	listeners := append([]net.Listener(nil), e.listeners...)
	e.listeners = nil
	e.mu.Unlock()
	var first error
	for _, listener := range listeners {
		if err := listener.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
