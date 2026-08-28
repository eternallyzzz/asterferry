package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	grpcServeErr    <-chan error
	httpServeErr    <-chan error
	reconcileCancel context.CancelFunc
	closeOnce       sync.Once
	closeErr        error
	waitOnce        sync.Once
	waitDone        chan struct{}
	waitErr         error
	logger          *slog.Logger
}

func New(config Config) (*Controller, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	masterKey, err := LoadOrCreateMasterKey(config.MasterKeyPath)
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(config.DatabasePath, masterKey)
	if err != nil {
		return nil, err
	}
	httpServer, err := NewServer(config, store)
	if err != nil {
		store.Close()
		return nil, err
	}
	return &Controller{Config: config, Store: store, HTTP: httpServer, logger: slog.Default()}, nil
}

func (c *Controller) Start(ctx context.Context) error {
	if c == nil || c.Store == nil || c.HTTP == nil {
		return errors.New("controller is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	listener, grpcServer, grpcServeErr, err := StartGRPCWithErrors(runCtx, c.Config, c.Store)
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
	c.grpcServeErr = grpcServeErr
	c.waitDone = make(chan struct{})
	c.reconcileCancel = cancel
	httpServeErr := make(chan error, 1)
	c.httpServeErr = httpServeErr
	go func() {
		httpServeErr <- c.HTTP.Serve(httpListener)
		close(httpServeErr)
	}()
	// Context cancellation must stop both control-plane listeners. Close is
	// idempotent, so an explicit caller shutdown can safely race this goroutine.
	go func() {
		<-runCtx.Done()
		_ = c.HTTP.Close()
		_ = httpListener.Close()
	}()
	go c.reconcileLoop(runCtx)
	go c.monitorServers(runCtx)
	return nil
}

// Wait blocks until the Controller is stopped or either control-plane server
// exits. A non-nil result means a server failed unexpectedly and should make
// the process exit non-zero.
func (c *Controller) Wait() error {
	if c == nil || c.waitDone == nil {
		return errors.New("controller is not started")
	}
	<-c.waitDone
	return c.waitErr
}

func (c *Controller) monitorServers(ctx context.Context) {
	finish := func(err error) {
		c.waitOnce.Do(func() {
			c.waitErr = err
			close(c.waitDone)
		})
	}
	for {
		select {
		case <-ctx.Done():
			finish(nil)
			return
		case err, ok := <-c.grpcServeErr:
			if !ok {
				err = errors.New("gRPC server stopped unexpectedly")
			}
			if ctx.Err() != nil || isExpectedServerStopError(err) {
				finish(nil)
				return
			}
			if err == nil {
				err = errors.New("gRPC server stopped unexpectedly")
			}
			c.logServeFailure("grpc", err)
			if c.reconcileCancel != nil {
				c.reconcileCancel()
			}
			_ = c.HTTP.Close()
			finish(fmt.Errorf("gRPC serve failed: %w", err))
			return
		case err, ok := <-c.httpServeErr:
			if !ok {
				err = errors.New("HTTP server stopped unexpectedly")
			}
			if ctx.Err() != nil || isExpectedServerStopError(err) {
				finish(nil)
				return
			}
			if err == nil {
				err = errors.New("HTTP server stopped unexpectedly")
			}
			c.logServeFailure("http", err)
			if c.reconcileCancel != nil {
				c.reconcileCancel()
			}
			if c.grpcServer != nil {
				c.grpcServer.Stop()
			}
			finish(fmt.Errorf("HTTP serve failed: %w", err))
			return
		}
	}
}

func isExpectedServerStopError(err error) bool {
	return errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed)
}

func (c *Controller) logServeFailure(component string, err error) {
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("controller control-plane server stopped", "component", component, "error", err)
}

func (c *Controller) reconcileLoop(ctx context.Context) {
	// Run once promptly after startup, then at a bounded interval. The pass is
	// intentionally best-effort: a transient SQLite or placement error leaves
	// the assignment degraded and the next tick retries it.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if _, err := c.Store.ReconcileAssignments(ctx, DefaultGatewayOfflineAfter); err != nil && ctx.Err() == nil {
			logger := c.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("controller assignment reconciliation failed", "error", err)
		}
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
