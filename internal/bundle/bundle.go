// Package bundle describes the portable two-role directory produced by init.
// It contains no process management; callers can use it from the CLI,
// supervisor, migration tools, and deployment helpers without duplicating
// path conventions.
package bundle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	GatewayRole = "gateway"
	AgentRole   = "agent"
)

type Bundle struct {
	Root          string
	GatewayConfig string
	AgentConfig   string
	RunDir        string
	LogsDir       string
}

func Open(root string) (Bundle, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return Bundle{}, errors.New("bundle directory is required")
	}
	root = filepath.Clean(root)
	abs, err := filepath.Abs(root)
	if err != nil {
		return Bundle{}, fmt.Errorf("resolve bundle directory: %w", err)
	}
	if err := requireDirectory(abs, "bundle directory", true); err != nil {
		return Bundle{}, err
	}
	b := Bundle{
		Root:          abs,
		GatewayConfig: filepath.Join(abs, "config", "gateway.yaml"),
		AgentConfig:   filepath.Join(abs, "config", "agent.yaml"),
		RunDir:        filepath.Join(abs, "run"),
		LogsDir:       filepath.Join(abs, "logs"),
	}
	if err := requireRegularFile(b.GatewayConfig, "gateway configuration"); err != nil {
		return Bundle{}, err
	}
	if err := requireRegularFile(b.AgentConfig, "agent configuration"); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

func (b Bundle) Config(role string) string {
	if role == AgentRole {
		return b.AgentConfig
	}
	return b.GatewayConfig
}

func (b Bundle) EnsureRuntimeDirs() error {
	if b.Root == "" {
		return errors.New("bundle is empty")
	}
	for _, path := range []string{b.RunDir, b.LogsDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create bundle runtime directory %q: %w", path, err)
		}
		if err := rejectSymlink(path, "bundle runtime directory"); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure bundle runtime directory %q: %w", path, err)
		}
	}
	return nil
}

func requireDirectory(path, label string, rejectSymlinkValue bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s %q was not found", label, path)
		}
		return fmt.Errorf("inspect %s %q: %w", label, path, err)
	}
	if rejectSymlinkValue && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse symlink %s %q", label, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %q is not a directory", label, path)
	}
	return nil
}

func requireRegularFile(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s %q was not found; run asterferry init first", label, path)
		}
		return fmt.Errorf("inspect %s %q: %w", label, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s %q must be a regular file", label, path)
	}
	return nil
}

func rejectSymlink(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s %q: %w", label, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse symlink %s %q", label, path)
	}
	return nil
}
