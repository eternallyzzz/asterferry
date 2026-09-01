package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootExposesOnlyCurrentGenerationCommands(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand(&out, &out)
	for _, name := range []string{"controller", "enroll-token", "node", "gateway", "agent", "healthcheck", "version", "completion"} {
		if command, _, err := root.Find([]string{name}); err != nil || command == root {
			t.Fatalf("missing command %q", name)
		}
	}
	for _, name := range []string{"init", "migrate", "up", "down", "config", "supervise", "doctor", "status", "validate"} {
		if command, _, err := root.Find([]string{name}); err == nil && command != root {
			t.Fatalf("retired command %q is still exposed", name)
		}
	}
	if command, _, err := root.Find([]string{"controller", "migrate"}); err != nil || command == root {
		t.Fatalf("controller migrate command is missing: command=%v err=%v", command, err)
	}
}

func TestNodeRunRequiresBootstrap(t *testing.T) {
	err := run([]string{"node", "run"})
	if err == nil || !strings.Contains(err.Error(), "--bootstrap is required") {
		t.Fatalf("generic node run error = %v", err)
	}
	err = run([]string{"gateway", "run"})
	if err == nil || !strings.Contains(err.Error(), "--bootstrap is required") {
		t.Fatalf("gateway run error = %v", err)
	}
	err = run([]string{"agent", "run", "--config", "old.yaml"})
	if err == nil {
		t.Fatal("legacy --config flag was accepted")
	}
}

func TestVersionUsesWireGenerationString(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand(&out, &out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "AFDP/2") || strings.Contains(out.String(), "protocol: v6") {
		t.Fatalf("unexpected version output: %s", out.String())
	}
}
