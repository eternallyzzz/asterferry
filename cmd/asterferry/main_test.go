package main

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"asterferry/internal/lifecycle"
)

func TestWaitSignalsGracefulFirstSignal(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	var forced atomic.Bool
	err := waitSignals(time.Second, func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("shutdown context must be bounded")
		}
		return nil
	}, func() error {
		forced.Store(true)
		return nil
	}, signals)
	if err != nil {
		t.Fatal(err)
	}
	if forced.Load() {
		t.Fatal("single signal must not force close")
	}
}

func TestWaitSignalsSecondSignalForcesClose(t *testing.T) {
	signals := make(chan os.Signal, 2)
	signals <- os.Interrupt
	signals <- os.Interrupt
	var forced atomic.Bool
	release := make(chan struct{})
	err := waitSignals(time.Second, func(context.Context) error {
		<-release
		return nil
	}, func() error {
		forced.Store(true)
		close(release)
		return nil
	}, signals)
	if err != nil {
		t.Fatal(err)
	}
	if !forced.Load() {
		t.Fatal("second signal must force close")
	}
}

func TestWaitSignalsManagementRequestStartsGracefulShutdown(t *testing.T) {
	signals := make(chan os.Signal, 1)
	trigger := lifecycle.NewShutdownTrigger()
	trigger.Request()
	var called atomic.Bool
	err := waitSignals(time.Second, func(ctx context.Context) error {
		called.Store(true)
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("shutdown context must be bounded")
		}
		return nil
	}, func() error {
		t.Fatal("management request must not force close")
		return nil
	}, signals, trigger.C())
	if err != nil || !called.Load() {
		t.Fatalf("management shutdown result = %v called=%v", err, called.Load())
	}
}

func TestIntentionalShutdownError(t *testing.T) {
	if got := intentionalShutdownError(context.DeadlineExceeded); got != nil {
		t.Fatal("deadline should be treated as intentional shutdown")
	}
	if got := intentionalShutdownError(errors.New("close failed")); got == nil {
		t.Fatal("close errors must be returned")
	}
}
