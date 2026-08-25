package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"asterferry/internal/bootstrap"
	"asterferry/internal/config"
	"asterferry/internal/lifecycle"
)

type fakeService struct {
	startErr  error
	started   atomic.Bool
	shutdowns atomic.Int32
	closed    atomic.Int32
}

func (s *fakeService) Start() error {
	s.started.Store(true)
	return s.startErr
}

func (s *fakeService) Shutdown(context.Context) error {
	s.shutdowns.Add(1)
	return nil
}

func (s *fakeService) Close() error {
	s.closed.Add(1)
	return nil
}

func TestRunOwnsCommonLifecycle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := bootstrap.Generate(bootstrap.Options{Dir: root, Profile: bootstrap.ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeService{}
	if err := Run(context.Background(), Options{
		ConfigPath:   result.GatewayConfig,
		ExpectedRole: "gateway",
		Errors:       &bytes.Buffer{},
		Factory: func(_ *config.Config, deps Dependencies) (Service, StartInfo, error) {
			if !deps.ShutdownTrigger.Request() {
				t.Fatal("shutdown trigger was already requested")
			}
			return service, StartInfo{Message: "test started"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !service.started.Load() || service.shutdowns.Load() != 1 || service.closed.Load() != 1 {
		t.Fatalf("lifecycle state started=%t shutdowns=%d closed=%d", service.started.Load(), service.shutdowns.Load(), service.closed.Load())
	}
}

func TestRunClosesServiceWhenStartFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	result, err := bootstrap.Generate(bootstrap.Options{Dir: root, Profile: bootstrap.ProfileDev})
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeService{startErr: errors.New("start failed")}
	err = Run(context.Background(), Options{
		ConfigPath:   result.AgentConfig,
		ExpectedRole: "agent",
		Errors:       &bytes.Buffer{},
		Factory: func(_ *config.Config, _ Dependencies) (Service, StartInfo, error) {
			return service, StartInfo{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "start failed") || service.closed.Load() != 1 {
		t.Fatalf("start failure err=%v closed=%d", err, service.closed.Load())
	}
}

func TestWaitSignalsGracefulFirstSignal(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	var forced atomic.Bool
	err := waitSignals(context.Background(), time.Second, func(ctx context.Context) error {
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
	err := waitSignals(context.Background(), time.Second, func(context.Context) error {
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
	err := waitSignals(context.Background(), time.Second, func(ctx context.Context) error {
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

func TestWaitForTriggerReturnsRestartExitCode(t *testing.T) {
	trigger := lifecycle.NewShutdownTrigger()
	if !trigger.RequestRestart() {
		t.Fatal("restart request was not accepted")
	}
	signals := make(chan os.Signal, 1)
	err := waitForTriggerSignals(context.Background(), time.Second, func(context.Context) error {
		return nil
	}, func() error {
		t.Fatal("restart request must not force close")
		return nil
	}, signals, trigger)
	var coded interface{ ExitCode() int }
	if !errors.As(err, &coded) || coded.ExitCode() != RestartRequestedExitCode {
		t.Fatalf("restart exit code = %v, err=%v", coded.ExitCode(), err)
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
