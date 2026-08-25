// Package runner owns the common lifecycle around a single AsterFerry role.
// Role packages provide the data-plane service; this package provides the
// configuration, logging, management, signal, and restart orchestration.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/configstore"
	"asterferry/internal/lifecycle"
	"asterferry/internal/logging"
	"asterferry/internal/observability"
)

const RestartRequestedExitCode = 75

var (
	ErrForcedShutdownTimeout = errors.New("forced shutdown did not finish within the close wait period")
	forcedShutdownWait       = 2 * time.Second
)

type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string { return e.Err.Error() }
func (e *ExitCodeError) Unwrap() error { return e.Err }
func (e *ExitCodeError) ExitCode() int { return e.Code }

// Service is the lifecycle contract shared by Gateway and Agent runtimes.
type Service interface {
	Start() error
	Shutdown(context.Context) error
	Close() error
}

type Dependencies struct {
	Logger          *slog.Logger
	Events          *observability.EventHub
	ShutdownTrigger *lifecycle.ShutdownTrigger
	Config          *configstore.Manager
}

type StartInfo struct {
	Message    string
	Attributes []any
}

type Factory func(*config.Config, Dependencies) (Service, StartInfo, error)

type Options struct {
	ConfigPath   string
	ExpectedRole string
	Errors       io.Writer
	Factory      Factory
}

func Run(ctx context.Context, options Options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(options.ConfigPath) == "" {
		return errors.New("runtime configuration path is required")
	}
	if options.Factory == nil {
		return errors.New("runtime factory is required")
	}
	if options.Errors == nil {
		options.Errors = io.Discard
	}
	c, err := config.LoadRuntime(options.ConfigPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("configuration %q was not found: %w; create it or run asterferry init", options.ConfigPath, err)
		}
		var legacy *config.LegacyFieldError
		if errors.As(err, &legacy) {
			return fmt.Errorf("configuration %q uses the removed %s field: %w; run asterferry migrate <bundle>", options.ConfigPath, legacy.Path, err)
		}
		return fmt.Errorf("configuration %q is invalid: %w; run asterferry doctor after fixing YAML errors", options.ConfigPath, err)
	}
	if options.ExpectedRole != "" && c.Role != options.ExpectedRole {
		return fmt.Errorf("configuration role is not %s; choose a %s configuration", options.ExpectedRole, options.ExpectedRole)
	}
	configManager, err := configstore.New(options.ConfigPath)
	if err != nil {
		return fmt.Errorf("prepare configuration manager: %w", err)
	}
	events := observability.NewEventHub(0)
	trigger := lifecycle.NewShutdownTrigger()
	logger, closeLog, err := logging.New(config.ResolveLogging(c.Logging), c.Role, options.Errors, events)
	if err != nil {
		return err
	}
	defer closeLog()

	service, startInfo, err := options.Factory(c, Dependencies{
		Logger:          logger,
		Events:          events,
		ShutdownTrigger: trigger,
		Config:          configManager,
	})
	if err != nil {
		return err
	}
	if service == nil {
		return errors.New("runtime factory returned a nil service")
	}
	if err := service.Start(); err != nil {
		_ = service.Close()
		return err
	}
	defer func() { _ = service.Close() }()
	if startInfo.Message == "" {
		startInfo.Message = c.Role + " started"
	}
	attributes := append([]any{"event", "runtime.started", "config", options.ConfigPath}, startInfo.Attributes...)
	logger.Info(startInfo.Message, attributes...)
	return waitForTrigger(ctx, c.Shutdown.GracePeriodSec, service.Shutdown, service.Close, trigger)
}

func waitForTrigger(ctx context.Context, graceSeconds int64, shutdown func(context.Context) error, closeFn func() error, trigger *lifecycle.ShutdownTrigger) error {
	grace := time.Duration(graceSeconds) * time.Second
	if grace <= 0 {
		grace = 30 * time.Second
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	return waitForTriggerSignals(ctx, grace, shutdown, closeFn, signals, trigger)
}

func waitForTriggerSignals(ctx context.Context, grace time.Duration, shutdown func(context.Context) error, closeFn func() error, signals <-chan os.Signal, trigger *lifecycle.ShutdownTrigger) error {
	if err := waitSignals(ctx, grace, shutdown, closeFn, signals, trigger.C()); err != nil {
		return err
	}
	if trigger.RestartRequested() {
		return &ExitCodeError{Code: RestartRequestedExitCode, Err: errors.New("configuration restart requested")}
	}
	return nil
}

func waitSignals(ctx context.Context, grace time.Duration, shutdown func(context.Context) error, closeFn func() error, signals <-chan os.Signal, requests ...<-chan struct{}) error {
	if grace <= 0 {
		grace = 30 * time.Second
	}
	var requested <-chan struct{}
	if len(requests) > 0 {
		requested = requests[0]
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-signals:
	case <-requested:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- shutdown(shutdownCtx) }()
	select {
	case err := <-done:
		return intentionalShutdownError(err)
	case <-signals:
		forceErr, closeFinished := waitClose(closeFn, forcedShutdownWait)
		if !closeFinished {
			return ErrForcedShutdownTimeout
		}
		shutdownErr, finished := waitShutdown(done, forcedShutdownWait)
		if !finished {
			if forceErr != nil {
				return errors.Join(forceErr, ErrForcedShutdownTimeout)
			}
			return ErrForcedShutdownTimeout
		}
		if forceErr != nil {
			return forceErr
		}
		return intentionalShutdownError(shutdownErr)
	case <-ctx.Done():
		forceErr, closeFinished := waitClose(closeFn, forcedShutdownWait)
		if !closeFinished {
			return ErrForcedShutdownTimeout
		}
		shutdownErr, finished := waitShutdown(done, forcedShutdownWait)
		if !finished {
			if forceErr != nil {
				return errors.Join(forceErr, ErrForcedShutdownTimeout)
			}
			return ErrForcedShutdownTimeout
		}
		if forceErr != nil {
			return forceErr
		}
		if shutdownErr != nil {
			return shutdownErr
		}
		return ctx.Err()
	}
}

func waitClose(closeFn func() error, timeout time.Duration) (error, bool) {
	if closeFn == nil {
		return nil, true
	}
	done := make(chan error, 1)
	go func() { done <- closeFn() }()
	return waitShutdown(done, timeout)
}

func waitShutdown(done <-chan error, timeout time.Duration) (error, bool) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err, true
	case <-timer.C:
		return nil, false
	}
}

func intentionalShutdownError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}
