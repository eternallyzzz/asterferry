package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"asterferry/internal/atomicfile"
	"asterferry/internal/jsonutil"
)

const (
	DatabaseDriverSQLite    = "sqlite"
	DatabaseDriverPostgres  = "postgres"
	defaultPostgresPoolSize = 16
	maxDatabasePoolSize     = 256
)

// Config is intentionally small: business state belongs in the configured
// Controller database and is never read from a node YAML file. SQLite remains
// the zero-dependency default; PostgreSQL is the production-scale backend.
type Config struct {
	HTTPListen           string `json:"http_listen"`
	GRPCListen           string `json:"grpc_listen"`
	GRPCAdvertise        string `json:"grpc_advertise,omitempty"`
	ReleaseBaseURL       string `json:"release_base_url,omitempty"`
	ReleaseVersion       string `json:"release_version,omitempty"`
	DatabaseDriver       string `json:"database_driver,omitempty"`
	DatabasePath         string `json:"database_path,omitempty"`
	DatabaseURL          string `json:"database_url,omitempty"`
	DatabaseMaxOpenConns int    `json:"database_max_open_conns,omitempty"`
	CAKeyPath            string `json:"ca_key_path"`
	CACertPath           string `json:"ca_cert_path"`
	TLSCertPath          string `json:"tls_cert_path"`
	TLSKeyPath           string `json:"tls_key_path"`
	MasterKeyPath        string `json:"master_key_path"`
	DashboardEnable      bool   `json:"dashboard_enable"`
	LogLevel             string `json:"log_level"`
	// History retention is expressed in operator-friendly units rather than
	// time.Duration's nanosecond JSON representation. Zero disables cleanup for
	// the corresponding table.
	IdempotencyRetentionHours int64 `json:"idempotency_retention_hours"`
	AuditRetentionDays        int64 `json:"audit_retention_days"`
	// SourcePath is process-local provenance used by backup helpers. It is not
	// serialized into controller.json and does not affect configuration
	// validation.
	SourcePath string `json:"-"`
}

func DefaultConfig(dir string) Config {
	dir = filepath.Clean(dir)
	return Config{
		HTTPListen:                ":8443",
		GRPCListen:                ":9443",
		ReleaseBaseURL:            "https://github.com/eternallyzzz/asterferry/releases/download",
		DatabaseDriver:            DatabaseDriverSQLite,
		DatabasePath:              filepath.Join(dir, "controller.db"),
		CAKeyPath:                 filepath.Join(dir, "ca", "ca.key"),
		CACertPath:                filepath.Join(dir, "ca", "ca.crt"),
		TLSCertPath:               filepath.Join(dir, "tls", "controller.crt"),
		TLSKeyPath:                filepath.Join(dir, "tls", "controller.key"),
		MasterKeyPath:             filepath.Join(dir, "master.key"),
		DashboardEnable:           true,
		LogLevel:                  "info",
		IdempotencyRetentionHours: 24,
		AuditRetentionDays:        90,
	}
}

func (c Config) Validate() error {
	if err := validateListenAddress(c.HTTPListen, "http_listen"); err != nil {
		return err
	}
	if err := validateListenAddress(c.GRPCListen, "grpc_listen"); err != nil {
		return err
	}
	if strings.TrimSpace(c.GRPCAdvertise) != "" {
		if err := validateAdvertisedAddress(c.GRPCAdvertise, "grpc_advertise"); err != nil {
			return err
		}
	}
	if strings.TrimSpace(c.ReleaseBaseURL) != "" {
		parsed, err := url.Parse(strings.TrimSpace(c.ReleaseBaseURL))
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("controller release_base_url must be an absolute http(s) URL without query or fragment")
		}
	}
	if strings.TrimSpace(c.ReleaseVersion) != "" && !validReleaseVersion(c.ReleaseVersion) {
		return errors.New("controller release_version must be a semantic version such as 1.0.0")
	}
	databaseDriver := normalizeDatabaseDriver(c.DatabaseDriver)
	if databaseDriver != DatabaseDriverSQLite && databaseDriver != DatabaseDriverPostgres {
		return fmt.Errorf("controller database_driver must be %q or %q", DatabaseDriverSQLite, DatabaseDriverPostgres)
	}
	if c.DatabaseMaxOpenConns < 0 || c.DatabaseMaxOpenConns > maxDatabasePoolSize {
		return fmt.Errorf("controller database_max_open_conns must be between 0 and %d", maxDatabasePoolSize)
	}
	if databaseDriver == DatabaseDriverSQLite {
		if strings.TrimSpace(c.DatabasePath) == "" {
			return errors.New("controller database_path is required for sqlite")
		}
		if strings.TrimSpace(c.DatabaseURL) != "" {
			return errors.New("controller database_url is only valid for postgres")
		}
	} else {
		if strings.TrimSpace(c.DatabaseURL) == "" {
			return errors.New("controller database_url is required for postgres")
		}
		if strings.ContainsAny(c.DatabaseURL, "\x00\r\n") || len(c.DatabaseURL) > 4096 {
			return errors.New("controller database_url is invalid")
		}
	}
	for field, value := range map[string]string{"ca_key_path": c.CAKeyPath, "ca_cert_path": c.CACertPath, "tls_cert_path": c.TLSCertPath, "tls_key_path": c.TLSKeyPath, "master_key_path": c.MasterKeyPath} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("controller %s is required", field)
		}
	}
	if c.IdempotencyRetentionHours < 0 || c.AuditRetentionDays < 0 {
		return errors.New("controller history retention values cannot be negative")
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
	// Preserve the new defaults for configurations generated before retention
	// settings existed while still allowing an explicit zero to disable a
	// cleanup job.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err == nil {
		defaults := DefaultConfig(filepath.Dir(path))
		if _, ok := fields["database_driver"]; !ok || strings.TrimSpace(config.DatabaseDriver) == "" {
			config.DatabaseDriver = defaults.DatabaseDriver
		}
		if _, ok := fields["release_base_url"]; !ok {
			config.ReleaseBaseURL = defaults.ReleaseBaseURL
		}
		if _, ok := fields["idempotency_retention_hours"]; !ok {
			config.IdempotencyRetentionHours = defaults.IdempotencyRetentionHours
		}
		if _, ok := fields["audit_retention_days"]; !ok {
			config.AuditRetentionDays = defaults.AuditRetentionDays
		}
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	config = resolveConfigPaths(config, filepath.Dir(path))
	config.SourcePath = path
	return config, nil
}

func normalizeDatabaseDriver(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return DatabaseDriverSQLite
	}
	if value == "postgresql" {
		return DatabaseDriverPostgres
	}
	return value
}

func databasePoolSize(config Config) int {
	if normalizeDatabaseDriver(config.DatabaseDriver) == DatabaseDriverSQLite {
		return 1
	}
	if config.DatabaseMaxOpenConns > 0 {
		return config.DatabaseMaxOpenConns
	}
	if normalizeDatabaseDriver(config.DatabaseDriver) == DatabaseDriverPostgres {
		return defaultPostgresPoolSize
	}
	return 1
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

func validateAdvertisedAddress(value, field string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("controller %s is required", field)
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil || strings.TrimSpace(host) == "" {
		return fmt.Errorf("controller %s must be host:port", field)
	}
	if parsed := net.ParseIP(strings.Trim(host, "[]")); parsed != nil && parsed.IsUnspecified() {
		return fmt.Errorf("controller %s must identify a reachable host, not an unspecified address", field)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("controller %s port must be between 1 and 65535", field)
	}
	return nil
}

func SaveConfig(path string, config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	data, err := marshalConfig(config)
	if err != nil {
		return err
	}
	return atomicfile.AtomicWrite(path, append(data, '\n'), 0o600)
}

func marshalConfig(config Config) ([]byte, error) {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
