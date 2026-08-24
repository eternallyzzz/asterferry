package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"asterferry/internal/agent"
	"asterferry/internal/config"
	"asterferry/internal/gateway"
	"asterferry/internal/logging"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: asterferry <gateway|agent|validate> -c config.yaml")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	path := fs.String("c", "config.yaml", "configuration file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if err := config.ApplyEnv(c); err != nil {
		return err
	}
	switch args[0] {
	case "validate":
		fmt.Printf("valid %s configuration\n", c.Role)
		return nil
	case "gateway":
		if c.Role != config.RoleGateway {
			return errors.New("configuration mode is not gateway")
		}
		opts, err := c.ResolveGateway()
		if err != nil {
			return err
		}
		logger, closeLog, err := logging.New(opts.Logging, c.Role, os.Stderr)
		if err != nil {
			return err
		}
		defer closeLog()
		s, err := gateway.New(opts, logger)
		if err != nil {
			return err
		}
		if err = s.Start(); err != nil {
			return err
		}
		return wait(opts.Shutdown.GracePeriod, s.Shutdown, s.Close)
	case "agent":
		if c.Role != config.RoleAgent {
			return errors.New("configuration mode is not agent")
		}
		opts, err := c.ResolveAgent()
		if err != nil {
			return err
		}
		logger, closeLog, err := logging.New(opts.Logging, c.Role, os.Stderr)
		if err != nil {
			return err
		}
		defer closeLog()
		a, err := agent.New(opts, logger)
		if err != nil {
			return err
		}
		if err = a.Start(); err != nil {
			return err
		}
		return wait(opts.Shutdown.GracePeriod, a.Shutdown, a.Close)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func wait(grace time.Duration, shutdown func(context.Context) error, closeFn func() error) error {
	if grace <= 0 {
		grace = 30 * time.Second
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	return waitSignals(grace, shutdown, closeFn, signals)
}

func waitSignals(grace time.Duration, shutdown func(context.Context) error, closeFn func() error, signals <-chan os.Signal) error {
	if grace <= 0 {
		grace = 30 * time.Second
	}
	<-signals

	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- shutdown(ctx) }()
	select {
	case err := <-done:
		return intentionalShutdownError(err)
	case <-signals:
		forceErr := closeFn()
		shutdownErr := <-done
		if forceErr != nil {
			return forceErr
		}
		return intentionalShutdownError(shutdownErr)
	}
}

func intentionalShutdownError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}
