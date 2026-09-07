//go:build windows

// Package windowsservice runs daemons as console processes or Windows services.
package windowsservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
)

// Run executes a daemon in console mode or hosts it through the Windows
// Service Control Manager when the process was started as a service.
func Run(ctx context.Context, name string, run func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if run == nil {
		return errors.New("windows service runner requires a daemon function")
	}
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service mode: %w", err)
	}
	if !isService {
		return run(ctx)
	}
	if name == "" {
		return errors.New("windows service name must not be empty")
	}
	return svc.Run(name, &handler{name: name, run: run})
}

type handler struct {
	name string
	run  func(context.Context) error
}

func (h *handler) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	statuses <- svc.Status{State: svc.StartPending}

	serviceContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- h.run(serviceContext)
	}()

	statuses <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case err := <-result:
			statuses <- svc.Status{State: svc.StopPending}
			if err != nil {
				logServiceFailure(h.name, err)
				return true, 1
			}
			return false, 0
		case request, ok := <-requests:
			if !ok {
				cancel()
				err := <-result
				statuses <- svc.Status{State: svc.StopPending}
				if err != nil {
					logServiceFailure(h.name, err)
					return true, 1
				}
				return false, 0
			}
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				statuses <- svc.Status{State: svc.StopPending}
				cancel()
				err := <-result
				if err != nil {
					logServiceFailure(h.name, err)
					return true, 1
				}
				return false, 0
			}
		}
	}
}

func logServiceFailure(name string, cause error) {
	if cause == nil {
		return
	}
	root := os.Getenv("ProgramData")
	if strings.TrimSpace(root) == "" {
		root = os.TempDir()
	}
	cleanName := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, name)
	if cleanName == "" {
		cleanName = "asterferry-service"
	}
	path := filepath.Join(root, "AsterFerry", "logs", cleanName+".log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = fmt.Fprintf(file, "%s service failed: %v\r\n", time.Now().UTC().Format(time.RFC3339), cause)
}
