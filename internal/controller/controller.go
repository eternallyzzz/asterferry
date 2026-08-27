package controller

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// Controller wires the authoritative store and the two control-plane
// listeners. The data-plane engines never import this type.
type Controller struct {
	Config       Config
	Store        *Store
	HTTP         *Server
	grpcListener interface{ Close() error }
	httpListener net.Listener
	grpcServer   interface {
		Stop()
		GracefulStop()
	}
	reconcileCancel context.CancelFunc
	closeOnce       sync.Once
	closeErr        error
}

func New(ctx context.Context, config Config) (*Controller, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	store, err := OpenStore(config.DatabasePath)
	if err != nil {
		return nil, err
	}
	httpServer, err := NewServer(config, store)
	if err != nil {
		store.Close()
		return nil, err
	}
	_ = ctx
	return &Controller{Config: config, Store: store, HTTP: httpServer}, nil
}

func (c *Controller) Start(ctx context.Context) error {
	if c == nil || c.Store == nil || c.HTTP == nil {
		return errors.New("controller is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	listener, grpcServer, err := StartGRPC(runCtx, c.Config, c.Store)
	if err != nil {
		cancel()
		return err
	}
	httpListener, err := c.HTTP.TLSListener()
	if err != nil {
		grpcServer.Stop()
		_ = listener.Close()
		cancel()
		return err
	}
	c.grpcListener, c.grpcServer = listener, grpcServer
	c.httpListener = httpListener
	c.reconcileCancel = cancel
	go func() { _ = c.HTTP.Serve(httpListener) }()
	// Context cancellation must stop both control-plane listeners. Close is
	// idempotent, so an explicit caller shutdown can safely race this goroutine.
	go func() {
		<-runCtx.Done()
		_ = c.HTTP.Close()
		_ = httpListener.Close()
	}()
	go c.reconcileLoop(runCtx)
	return nil
}

func (c *Controller) reconcileLoop(ctx context.Context) {
	// Run once promptly after startup, then at a bounded interval. The pass is
	// intentionally best-effort: a transient SQLite or placement error leaves
	// the assignment degraded and the next tick retries it.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		_, _ = c.Store.ReconcileAssignments(ctx, DefaultGatewayOfflineAfter)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Controller) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.reconcileCancel != nil {
			c.reconcileCancel()
		}
		if c.grpcServer != nil {
			// Connect is a long-lived bidirectional stream.  Waiting for a
			// graceful gRPC drain here can block forever while a node keeps its
			// control stream open, which in turn prevents SQLite and the HTTP
			// listener from being released during process shutdown.  The
			// controller context has already been cancelled above; force-stop the
			// server so active Connect RPCs observe cancellation and unwind.
			c.grpcServer.Stop()
		}
		if c.grpcListener != nil {
			_ = c.grpcListener.Close()
		}
		if c.HTTP != nil {
			_ = c.HTTP.Close()
		}
		if c.httpListener != nil {
			_ = c.httpListener.Close()
		}
		if c.Store != nil {
			c.closeErr = c.Store.Close()
		}
	})
	return c.closeErr
}
