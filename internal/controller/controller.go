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

	"google.golang.org/grpc"
)

// Controller wires the authoritative store and the two control-plane
// listeners. The data-plane engines never import this type.
type Controller struct {
	Config          Config
	Repositories    *ControllerRepositories
	HTTP            *Server
	metrics         *ControllerMetrics
	Scheduler       *Scheduler
	grpcListener    net.Listener
	httpListener    net.Listener
	metricsListener net.Listener
	grpcServer      grpcServer
	grpcServeErr    <-chan error
	httpServeErr    <-chan error
	metricsServeErr <-chan error
	reconcileCancel context.CancelFunc
	closeOnce       sync.Once
	closeErr        error
	waitOnce        sync.Once
	waitDone        chan struct{}
	waitErr         error
	logger          *slog.Logger
	startMu         sync.Mutex
	started         bool
	closed          bool
}

var (
	ErrControllerAlreadyStarted = errors.New("controller is already started")
	ErrControllerClosed         = errors.New("controller is closed")
)

func New(config Config) (*Controller, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if err := validateControllerCertificate(config); err != nil {
		return nil, err
	}
	masterKey, err := LoadOrCreateMasterKey(config.MasterKeyPath)
	if err != nil {
		return nil, err
	}
	repositories, err := OpenControllerRepositoriesWithConfig(config, masterKey)
	if err != nil {
		return nil, err
	}
	controllerMetrics := newControllerMetrics(repositories.Resources.DatabaseDriver())
	scheduler, err := NewScheduler(repositories.Resources, controllerMetrics)
	if err != nil {
		_ = repositories.Close()
		return nil, err
	}
	httpServer, err := newServer(config, repositories, scheduler, controllerMetrics)
	if err != nil {
		repositories.Close()
		return nil, err
	}
	return &Controller{Config: config, Repositories: repositories, HTTP: httpServer, metrics: controllerMetrics, Scheduler: scheduler, logger: slog.Default()}, nil
}

func (c *Controller) Start(ctx context.Context) error {
	if c == nil || c.Repositories == nil || c.HTTP == nil || c.Scheduler == nil {
		return errors.New("controller is not initialized")
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.closed {
		return ErrControllerClosed
	}
	if c.started {
		return ErrControllerAlreadyStarted
	}
	c.started = true
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	listener, grpcServer, grpcServeErr, err := StartGRPCWithErrors(runCtx, c.Config, c.Repositories, c.metrics)
	if err != nil {
		c.started = false
		cancel()
		return err
	}
	httpListener, err := c.HTTP.TLSListener()
	if err != nil {
		c.started = false
		grpcServer.Stop()
		_ = listener.Close()
		cancel()
		return err
	}
	metricsListener, err := c.HTTP.MetricsListener()
	if err != nil && c.Config.MetricsListen != "" {
		c.started = false
		c.HTTP.Close()
		grpcServer.Stop()
		_ = listener.Close()
		_ = httpListener.Close()
		cancel()
		return fmt.Errorf("bind controller metrics listener: %w", err)
	}
	c.grpcListener, c.grpcServer = listener, grpcServer
	c.httpListener = httpListener
	c.metricsListener = metricsListener
	c.grpcServeErr = grpcServeErr
	c.waitDone = make(chan struct{})
	c.reconcileCancel = cancel
	httpServeErr := make(chan error, 1)
	c.httpServeErr = httpServeErr
	go func() {
		httpServeErr <- c.HTTP.Serve(httpListener)
		close(httpServeErr)
	}()
	var metricsServeErr chan error
	if metricsListener != nil {
		metricsServeErr = make(chan error, 1)
		c.metricsServeErr = metricsServeErr
		go func() {
			metricsServeErr <- c.HTTP.ServeMetrics(metricsListener)
			close(metricsServeErr)
		}()
	}
	// Context cancellation must stop both control-plane listeners. Close is
	// idempotent, so an explicit caller shutdown can safely race this goroutine.
	go func() {
		<-runCtx.Done()
		_ = c.HTTP.Close()
		_ = httpListener.Close()
		if metricsListener != nil {
			_ = metricsListener.Close()
		}
	}()
	go c.reconcileLoop(runCtx)
	go c.maintenanceLoop(runCtx)
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
		case err, ok := <-c.metricsServeErr:
			if !ok {
				err = errors.New("metrics server stopped unexpectedly")
			}
			if ctx.Err() != nil || isExpectedServerStopError(err) {
				finish(nil)
				return
			}
			if err == nil {
				err = errors.New("metrics server stopped unexpectedly")
			}
			c.logServeFailure("metrics", err)
			if c.reconcileCancel != nil {
				c.reconcileCancel()
			}
			if c.grpcServer != nil {
				c.grpcServer.Stop()
			}
			_ = c.HTTP.Close()
			finish(fmt.Errorf("metrics serve failed: %w", err))
			return
		}
	}
}

func isExpectedServerStopError(err error) bool {
	return errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) || errors.Is(err, grpc.ErrServerStopped)
}

func (c *Controller) logServeFailure(component string, err error) {
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("controller control-plane server stopped", "component", component, "error", err)
}

func (c *Controller) reconcileLoop(ctx context.Context) {
	changes, unsubscribe := c.Repositories.ChangeBus().SubscribeResourceChanges()
	defer unsubscribe()
	// Recover stale state once at startup. Normal operation is driven by
	// coalesced heartbeat/resource hints; the slow sweep remains a repair path
	// for missed notifications and for the passage of the offline deadline.
	c.reconcileAll(ctx)
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case change, ok := <-changes:
			if !ok {
				return
			}
			if len(change.NodeIDs) > 0 {
				if _, err := c.Scheduler.ReconcileAssignmentsForGateways(ctx, DefaultGatewayOfflineAfter, change.NodeIDs...); err != nil && ctx.Err() == nil {
					c.logReconcileFailure(err)
				}
				if _, err := c.Scheduler.ReconcileAssignmentsForAgents(ctx, change.NodeIDs...); err != nil && ctx.Err() == nil {
					c.logReconcileFailure(err)
				}
				if change.PendingServices {
					if _, err := c.Scheduler.ReconcilePendingServices(ctx); err != nil && ctx.Err() == nil {
						c.logReconcileFailure(err)
					}
				}
			}
		case <-ticker.C:
			c.reconcileAll(ctx)
		}
	}
}

func (c *Controller) reconcileAll(ctx context.Context) {
	if _, err := c.Scheduler.ReconcileAssignments(ctx, DefaultGatewayOfflineAfter); err != nil && ctx.Err() == nil {
		c.logReconcileFailure(err)
	}
	if _, err := c.Scheduler.ReconcilePendingServices(ctx); err != nil && ctx.Err() == nil {
		c.logReconcileFailure(err)
	}
}

func (c *Controller) logReconcileFailure(err error) {
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("controller assignment reconciliation failed", "error", err)
}

func (c *Controller) maintenanceLoop(ctx context.Context) {
	cleanup := func() {
		idempotencyTTL := time.Duration(c.Config.IdempotencyRetentionHours) * time.Hour
		auditTTL := time.Duration(c.Config.AuditRetentionDays) * 24 * time.Hour
		now := time.Now().UTC()
		if err := c.Repositories.Resources.PruneHistory(ctx, now, idempotencyTTL, auditTTL); err != nil && ctx.Err() == nil {
			logger := c.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("controller history cleanup failed", "error", err)
		}
		if err := c.Repositories.Runtime.PruneRuntimeHistory(ctx, now); err != nil && ctx.Err() == nil {
			logger := c.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("controller runtime history cleanup failed", "error", err)
		}
	}
	cleanup()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

func (c *Controller) Close() error {
	if c == nil {
		return nil
	}
	c.startMu.Lock()
	c.closed = true
	c.startMu.Unlock()
	c.closeOnce.Do(func() {
		if c.reconcileCancel != nil {
			c.reconcileCancel()
		}
		if c.grpcServer != nil {
			// Connect is a long-lived bidirectional stream.  Waiting for a
			// graceful gRPC drain here can block forever while a node keeps its
			// control stream open, which in turn prevents the database and the HTTP
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
		if c.metricsListener != nil {
			_ = c.metricsListener.Close()
		}
		if c.Repositories != nil {
			c.closeErr = c.Repositories.Close()
		}
	})
	return c.closeErr
}
