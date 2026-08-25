// Package supervisor runs the two role processes belonging to a local Bundle.
// It is intentionally a small development/small-deployment coordinator; real
// production deployments can continue to use systemd, Docker, or Kubernetes.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"asterferry/internal/bundle"
	"asterferry/internal/config"
	"asterferry/internal/managementclient"
)

const (
	stateVersion     = 1
	defaultStartWait = 15 * time.Second
	defaultStopWait  = 35 * time.Second
)

type Options struct {
	Executable      string
	Bundle          bundle.Bundle
	Output          io.Writer
	Errors          io.Writer
	StartupTimeout  time.Duration
	ShutdownTimeout time.Duration
}

type State struct {
	Version       int                     `json:"version"`
	SupervisorPID int                     `json:"supervisor_pid"`
	StartedAt     time.Time               `json:"started_at"`
	Processes     map[string]ProcessState `json:"processes"`
}

type ProcessState struct {
	Role      string    `json:"role"`
	PID       int       `json:"pid"`
	Config    string    `json:"config"`
	StartedAt time.Time `json:"started_at"`
}

type child struct {
	role    string
	config  string
	command *exec.Cmd
	wait    <-chan childResult
	logFile *os.File
}

type childResult struct {
	child *child
	err   error
}

func Run(ctx context.Context, options Options) error {
	if options.Executable == "" {
		return errors.New("supervisor executable is required")
	}
	if options.Output == nil {
		options.Output = io.Discard
	}
	if options.Errors == nil {
		options.Errors = options.Output
	}
	if options.StartupTimeout <= 0 {
		options.StartupTimeout = defaultStartWait
	}
	if options.ShutdownTimeout <= 0 {
		options.ShutdownTimeout = defaultStopWait
	}
	if err := options.Bundle.EnsureRuntimeDirs(); err != nil {
		return err
	}
	if err := validateBundle(options.Bundle); err != nil {
		return err
	}

	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	startedAt := time.Now().UTC()
	children := make(map[string]*child, 2)
	events := make(chan childResult, 4)
	writeState := func() error { return writeState(options.Bundle, startedAt, children) }
	removeState := func() { _ = os.Remove(filepath.Join(options.Bundle.RunDir, "state.json")) }
	defer removeState()

	startRole := func(role string) (*child, error) {
		configPath := options.Bundle.Config(role)
		var logFile *os.File
		if options.Output == io.Discard {
			path := filepath.Join(options.Bundle.LogsDir, role+".log")
			var err error
			logFile, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return nil, fmt.Errorf("open %s log: %w", role, err)
			}
		}
		command := exec.Command(options.Executable, role, "--config", configPath)
		var output io.Writer = options.Output
		if logFile != nil {
			output = logFile
		}
		command.Stdout = &prefixWriter{prefix: "[" + role + "] ", dst: output}
		command.Stderr = &prefixWriter{prefix: "[" + role + "] ", dst: output}
		if err := command.Start(); err != nil {
			if logFile != nil {
				_ = logFile.Close()
			}
			return nil, fmt.Errorf("start %s: %w", role, err)
		}
		result := make(chan childResult, 1)
		current := &child{role: role, config: configPath, command: command, wait: result, logFile: logFile}
		go func() {
			value := childResult{child: current, err: command.Wait()}
			result <- value
			events <- value
		}()
		return current, nil
	}

	startAndWait := func(role string) (*child, error) {
		current, err := startRole(role)
		if err != nil {
			return nil, err
		}
		children[role] = current
		if err := waitForManagement(options.Bundle.Config(role), options.StartupTimeout); err != nil {
			_ = terminateChild(current)
			if current.logFile != nil {
				_ = current.logFile.Close()
			}
			delete(children, role)
			return nil, fmt.Errorf("wait for %s management endpoint: %w", role, err)
		}
		if err := writeState(); err != nil {
			_ = terminateChild(current)
			if current.logFile != nil {
				_ = current.logFile.Close()
			}
			delete(children, role)
			return nil, err
		}
		return current, nil
	}

	if _, err := startAndWait(bundle.GatewayRole); err != nil {
		return err
	}
	if _, err := startAndWait(bundle.AgentRole); err != nil {
		stopChildren(ctx, options.Bundle, children, options.ShutdownTimeout, options.Errors)
		return err
	}
	_, _ = fmt.Fprintf(options.Output, "AsterFerry bundle is running\n  gateway: %s\n  agent:   %s\n", options.Bundle.GatewayConfig, options.Bundle.AgentConfig)

	for len(children) > 0 {
		select {
		case <-ctx.Done():
			stopChildren(context.Background(), options.Bundle, children, options.ShutdownTimeout, options.Errors)
			return nil
		case result := <-events:
			current := result.child
			delete(children, current.role)
			if current.logFile != nil {
				_ = current.logFile.Close()
			}
			code := processExitCode(result.err)
			if code == 75 {
				_, _ = fmt.Fprintf(options.Output, "%s requested configuration restart\n", current.role)
				if _, err := startAndWait(current.role); err != nil {
					stopChildren(context.Background(), options.Bundle, children, options.ShutdownTimeout, options.Errors)
					return err
				}
				continue
			}
			stopChildren(context.Background(), options.Bundle, children, options.ShutdownTimeout, options.Errors)
			if result.err == nil || code == 0 {
				return nil
			}
			return fmt.Errorf("%s exited with code %d: %w", current.role, code, result.err)
		}
	}
	return nil
}

func StartDetached(ctx context.Context, executable string, b bundle.Bundle, output io.Writer) error {
	if err := b.EnsureRuntimeDirs(); err != nil {
		return err
	}
	if err := validateBundle(b); err != nil {
		return err
	}
	if output == nil {
		output = io.Discard
	}
	statePath := filepath.Join(b.RunDir, "state.json")
	if state, stateErr := readState(b); stateErr == nil {
		if state.SupervisorPID > 0 {
			if process, findErr := os.FindProcess(state.SupervisorPID); findErr == nil && processExists(process) {
				return errors.New("bundle is already running")
			}
		}
		if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale supervisor state: %w", err)
		}
	} else if !os.IsNotExist(stateErr) {
		return stateErr
	}
	command := exec.Command(executable, "supervise", "--dir", b.Root)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		return fmt.Errorf("start detached supervisor: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	deadline := time.Now().Add(defaultStartWait)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statePath); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			_ = command.Process.Kill()
			return ctx.Err()
		case err := <-done:
			if err == nil {
				return errors.New("detached supervisor exited before writing state")
			}
			return fmt.Errorf("detached supervisor exited before writing state: %w", err)
		case <-ticker.C:
		}
	}
	_ = command.Process.Kill()
	<-done
	return errors.New("timed out waiting for detached supervisor state")
}

func Stop(ctx context.Context, b bundle.Bundle, timeout time.Duration, output io.Writer) error {
	if timeout <= 0 {
		timeout = defaultStopWait
	}
	state, err := readState(b)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	roles := []string{bundle.AgentRole, bundle.GatewayRole}
	for _, role := range roles {
		path := b.Config(role)
		c, loadErr := config.Load(path)
		if loadErr != nil {
			continue
		}
		client, clientErr := managementclient.New(c, managementclient.Admin, timeout)
		if clientErr != nil {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		response, requestErr := client.Do(requestCtx, "POST", "/v1/actions/shutdown", nil)
		if response != nil {
			_ = response.Body.Close()
		}
		cancel()
		if requestErr == nil && output != nil {
			_, _ = fmt.Fprintf(output, "%s shutdown requested\n", role)
		}
	}
	if state.SupervisorPID > 0 {
		if process, findErr := os.FindProcess(state.SupervisorPID); findErr == nil {
			deadline := time.Now().Add(timeout)
			for time.Now().Before(deadline) {
				if !processExists(process) {
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
			_ = process.Kill()
		}
	}
	_ = os.Remove(filepath.Join(b.RunDir, "state.json"))
	return nil
}

func validateBundle(b bundle.Bundle) error {
	for _, path := range []string{b.GatewayConfig, b.AgentConfig} {
		c, err := config.Load(path)
		if err != nil {
			return fmt.Errorf("validate %s: %w", path, err)
		}
		if err := config.ApplyEnv(c); err != nil {
			return fmt.Errorf("validate environment for %s: %w", path, err)
		}
	}
	return nil
}

func waitForManagement(path string, timeout time.Duration) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", c.Management.Listen, 250*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("management endpoint %s did not start", c.Management.Listen)
}

func stopChildren(ctx context.Context, b bundle.Bundle, children map[string]*child, timeout time.Duration, output io.Writer) {
	for _, role := range []string{bundle.AgentRole, bundle.GatewayRole} {
		current := children[role]
		if current == nil {
			continue
		}
		path := current.config
		if c, err := config.Load(path); err == nil {
			if client, clientErr := managementclient.New(c, managementclient.Admin, timeout); clientErr == nil {
				requestCtx, cancel := context.WithTimeout(ctx, timeout)
				response, requestErr := client.Do(requestCtx, "POST", "/v1/actions/shutdown", nil)
				if response != nil {
					_ = response.Body.Close()
				}
				cancel()
				if requestErr == nil && output != nil {
					_, _ = fmt.Fprintf(output, "%s shutdown requested\n", role)
				}
			}
		}
	}
	deadline := time.Now().Add(timeout)
	for len(children) > 0 && time.Now().Before(deadline) {
		for role, current := range children {
			select {
			case result := <-current.wait:
				if current.logFile != nil {
					_ = current.logFile.Close()
				}
				delete(children, role)
				_ = result
			default:
			}
		}
		if len(children) > 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	for _, current := range children {
		_ = terminateChild(current)
	}
	clear(children)
	_ = os.Remove(filepath.Join(b.RunDir, "state.json"))
}

func terminateChild(current *child) error {
	if current == nil || current.command == nil || current.command.Process == nil {
		return nil
	}
	if err := current.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func writeState(b bundle.Bundle, startedAt time.Time, children map[string]*child) error {
	processes := make(map[string]ProcessState, len(children))
	for role, current := range children {
		if current == nil || current.command == nil || current.command.Process == nil {
			continue
		}
		processes[role] = ProcessState{Role: role, PID: current.command.Process.Pid, Config: current.config, StartedAt: startedAt}
	}
	state := State{Version: stateVersion, SupervisorPID: os.Getpid(), StartedAt: startedAt, Processes: processes}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(b.RunDir, ".state-*")
	if err != nil {
		return fmt.Errorf("create supervisor state: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(b.RunDir, "state.json")); err != nil {
		return fmt.Errorf("publish supervisor state: %w", err)
	}
	return nil
}

func readState(b bundle.Bundle) (State, error) {
	data, err := os.ReadFile(filepath.Join(b.RunDir, "state.json"))
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("parse supervisor state: %w", err)
	}
	if state.Version != stateVersion {
		return State{}, errors.New("unsupported supervisor state version")
	}
	return state, nil
}

type prefixWriter struct {
	mu     sync.Mutex
	prefix string
	dst    io.Writer
	line   bool
}

func (w *prefixWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	originalLength := len(data)
	for len(data) > 0 {
		if !w.line {
			if _, err := io.WriteString(w.dst, w.prefix); err != nil {
				return 0, err
			}
			w.line = true
		}
		index := strings.IndexByte(string(data), '\n')
		if index < 0 {
			_, err := w.dst.Write(data)
			return originalLength, err
		}
		index++
		if _, err := w.dst.Write(data[:index]); err != nil {
			return 0, err
		}
		data = data[index:]
		w.line = false
	}
	return originalLength, nil
}
