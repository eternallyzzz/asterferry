package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"asterferry/internal/atomicfile"
	"asterferry/internal/jsonutil"
)

// Config is intentionally small: business state belongs in SQLite and is
// never read from a node YAML file.
type Config struct {
	HTTPListen      string `json:"http_listen"`
	GRPCListen      string `json:"grpc_listen"`
	DatabasePath    string `json:"database_path"`
	CAKeyPath       string `json:"ca_key_path"`
	CACertPath      string `json:"ca_cert_path"`
	TLSCertPath     string `json:"tls_cert_path"`
	TLSKeyPath      string `json:"tls_key_path"`
	MasterKeyPath   string `json:"master_key_path"`
	DashboardEnable bool   `json:"dashboard_enable"`
	LogLevel        string `json:"log_level"`
	// SourcePath is process-local provenance used by backup helpers. It is not
	// serialized into controller.json and does not affect configuration
	// validation.
	SourcePath string `json:"-"`
}

func DefaultConfig(dir string) Config {
	dir = filepath.Clean(dir)
	return Config{
		HTTPListen:      ":8443",
		GRPCListen:      ":9443",
		DatabasePath:    filepath.Join(dir, "controller.db"),
		CAKeyPath:       filepath.Join(dir, "ca", "ca.key"),
		CACertPath:      filepath.Join(dir, "ca", "ca.crt"),
		TLSCertPath:     filepath.Join(dir, "tls", "controller.crt"),
		TLSKeyPath:      filepath.Join(dir, "tls", "controller.key"),
		MasterKeyPath:   filepath.Join(dir, "master.key"),
		DashboardEnable: true,
		LogLevel:        "info",
	}
}

func (c Config) Validate() error {
	if err := validateListenAddress(c.HTTPListen, "http_listen"); err != nil {
		return err
	}
	if err := validateListenAddress(c.GRPCListen, "grpc_listen"); err != nil {
		return err
	}
	for field, value := range map[string]string{"database_path": c.DatabasePath, "ca_key_path": c.CAKeyPath, "ca_cert_path": c.CACertPath, "tls_cert_path": c.TLSCertPath, "tls_key_path": c.TLSKeyPath, "master_key_path": c.MasterKeyPath} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("controller %s is required", field)
		}
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	path = filepath.Clean(path)
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := jsonutil.DecodeStrict(b, &config); err != nil {
		if errors.Is(err, jsonutil.ErrTrailingJSON) {
			return Config{}, errors.New("decode controller config: trailing JSON")
		}
		return Config{}, fmt.Errorf("decode controller config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	config = resolveConfigPaths(config, filepath.Dir(path))
	config.SourcePath = path
	return config, nil
}

func resolveConfigPaths(config Config, base string) Config {
	base = filepath.Clean(base)
	for field := range map[*string]struct{}{
		&config.DatabasePath:  {},
		&config.CAKeyPath:     {},
		&config.CACertPath:    {},
		&config.TLSCertPath:   {},
		&config.TLSKeyPath:    {},
		&config.MasterKeyPath: {},
	} {
		if *field == "" || filepath.IsAbs(*field) || strings.HasPrefix(*field, "file:") || *field == ":memory:" {
			continue
		}
		*field = filepath.Join(base, *field)
	}
	return config
}

func validateListenAddress(value, field string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("controller %s is required", field)
	}
	if _, _, err := net.SplitHostPort(value); err != nil {
		return fmt.Errorf("controller %s must be host:port: %w", field, err)
	}
	_, portText, _ := net.SplitHostPort(value)
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("controller %s port must be between 0 and 65535", field)
	}
	return nil
}

func SaveConfig(path string, config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.AtomicWrite(path, append(data, '\n'), 0o600)
}
