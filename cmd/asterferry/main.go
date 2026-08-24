package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCode(err))
	}
}

func run(args []string) error {
	root := newRootCommand(os.Stdout, os.Stderr)
	root.SetArgs(args)
	return root.Execute()
}

func exitCode(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return 1
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
