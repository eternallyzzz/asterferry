package supervisor

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"asterferry/internal/bundle"
)

func TestPrefixWriterPrefixesEveryLine(t *testing.T) {
	var output bytes.Buffer
	writer := &prefixWriter{prefix: "[agent] ", dst: &output}
	if n, err := writer.Write([]byte("first\nsecond")); err != nil || n != len("first\nsecond") {
		t.Fatalf("first write = %d, %v", n, err)
	}
	if n, err := writer.Write([]byte("\nthird\n")); err != nil || n != len("\nthird\n") {
		t.Fatalf("second write = %d, %v", n, err)
	}
	if got, want := output.String(), "[agent] first\n[agent] second\n[agent] third\n"; got != want {
		t.Fatalf("prefixed output = %q, want %q", got, want)
	}
}

func TestProcessExitCode(t *testing.T) {
	if got := processExitCode(nil); got != 0 {
		t.Fatalf("nil exit code = %d", got)
	}
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd.exe", "/c", "exit", "75")
	} else {
		command = exec.Command("sh", "-c", "exit 75")
	}
	err := command.Run()
	if got := processExitCode(err); got != 75 {
		t.Fatalf("exit code = %d for %v", got, err)
	}
	if got := processExitCode(errors.New("not a process exit")); got != 1 {
		t.Fatalf("generic exit code = %d", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	b := bundle.Bundle{Root: t.TempDir()}
	b.RunDir = filepath.Join(b.Root, "run")
	b.LogsDir = filepath.Join(b.Root, "logs")
	if err := b.EnsureRuntimeDirs(); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Truncate(time.Microsecond)
	if err := writeState(b, started, map[string]*child{}); err != nil {
		t.Fatal(err)
	}
	state, err := readState(b)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != stateVersion || !state.StartedAt.Equal(started) || len(state.Processes) != 0 {
		t.Fatalf("state = %#v", state)
	}

	updated := started.Add(time.Second)
	if err := writeState(b, updated, map[string]*child{}); err != nil {
		t.Fatal(err)
	}
	state, err = readState(b)
	if err != nil {
		t.Fatal(err)
	}
	if !state.StartedAt.Equal(updated) {
		t.Fatalf("overwritten state started_at = %s, want %s", state.StartedAt, updated)
	}
}
