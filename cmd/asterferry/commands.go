package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"asterferry/internal/agent"
	"asterferry/internal/bootstrap"
	"asterferry/internal/buildinfo"
	"asterferry/internal/config"
	"asterferry/internal/diagnostics"
	"asterferry/internal/gateway"
	"asterferry/internal/lifecycle"
	"asterferry/internal/logging"
	"asterferry/internal/observability"
	"github.com/spf13/cobra"
)

type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }
func (e *codedError) ExitCode() int { return e.code }

func newRootCommand(out, errOut io.Writer) *cobra.Command {
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	info := buildinfo.Current()
	root := &cobra.Command{
		Use:           "asterferry",
		Short:         "secure Gateway-Agent relay",
		Long:          "AsterFerry provides authenticated QUIC proxying and reverse mappings between a Gateway and Agents.",
		Version:       info.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return &codedError{code: 2, err: errors.New(`a command is required; run "asterferry --help" for usage`)}
		},
	}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &codedError{code: 2, err: fmt.Errorf("%w; run %q for help", err, "asterferry "+cmd.Name()+" --help")}
	})
	root.Example = `  asterferry init --dir ./asterferry --profile dev
  asterferry doctor --config ./asterferry/config/gateway.yaml
  asterferry gateway --config ./asterferry/config/gateway.yaml`

	root.AddCommand(
		newVersionCommand(),
		newCompletionCommand(),
		newInitCommand(),
		newValidateCommand(),
		newDoctorCommand(),
		newStatusCommand(),
		newHealthcheckCommand(),
		newGatewayCommand(),
		newAgentCommand(),
	)
	return root
}

func newVersionCommand() *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "show build and protocol version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Current()
			if short {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), info.Version)
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "asterferry %s\ncommit: %s\nbuilt: %s\nprotocol: v%d\ngo: %s\nplatform: %s/%s\n", info.Version, info.Commit, info.BuildDate, info.Protocol, info.GoVersion, info.OS, info.Architecture)
			return err
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print only the application version")
	return cmd
}

func newCompletionCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "completion <bash|powershell|zsh>",
		Short:     "generate shell completion scripts",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "powershell", "zsh"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			switch strings.ToLower(args[0]) {
			case "bash":
				return root.GenBashCompletion(cmd.OutOrStdout())
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			case "zsh":
				return root.GenZshCompletion(cmd.OutOrStdout())
			default:
				return &codedError{code: 2, err: fmt.Errorf("unsupported shell %q; choose bash, powershell or zsh", args[0])}
			}
		},
	}
}

func newInitCommand() *cobra.Command {
	var (
		dir         string
		profile     string
		agentID     string
		gatewayHost string
		gatewayPort int
		force       bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "generate a secure Gateway-Agent configuration bundle",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if profile == "" {
				return &codedError{code: 2, err: errors.New(`--profile is required; choose "dev" for local testing or "prod" for PKI CSRs`)}
			}
			result, err := bootstrap.Generate(bootstrap.Options{Dir: dir, Profile: bootstrap.Profile(profile), AgentID: agentID, GatewayHost: gatewayHost, GatewayPort: gatewayPort, Force: force})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "created %s bundle in %s\ngateway config: %s\nagent config: %s\nrun doctor before starting; keep secrets/ private\n", result.Profile, result.Dir, result.GatewayConfig, result.AgentConfig)
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "asterferry", "output directory")
	cmd.Flags().StringVar(&profile, "profile", "", "initialization profile: dev or prod")
	cmd.Flags().StringVar(&agentID, "agent-id", "edge-a", "Agent identity")
	cmd.Flags().StringVar(&gatewayHost, "gateway-host", "127.0.0.1", "Gateway hostname or IP used by the Agent")
	cmd.Flags().IntVar(&gatewayPort, "gateway-port", 4433, "Gateway UDP port")
	cmd.Flags().BoolVar(&force, "force", false, "replace files in an existing init bundle")
	return cmd
}

func newValidateCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "validate YAML structure and configuration semantics",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := loadConfig(path)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "valid %s configuration\n", c.Role)
			return err
		},
	}
	addConfigFlag(cmd, &path)
	return cmd
}

func newDoctorCommand() *cobra.Command {
	var (
		path       string
		jsonOutput bool
		skipPorts  bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "check local files, TLS material, secrets and listener ports",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := loadConfig(path)
			if err != nil {
				return err
			}
			report := diagnostics.Check(c, skipPorts)
			if err := writeDiagnosticReport(cmd.OutOrStdout(), report, jsonOutput); err != nil {
				return err
			}
			if report.Errors() > 0 {
				return &codedError{code: 1, err: fmt.Errorf("doctor found %d error(s) and %d warning(s)", report.Errors(), report.Warnings())}
			}
			return nil
		},
	}
	addConfigFlag(cmd, &path)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "write machine-readable JSON")
	cmd.Flags().BoolVar(&skipPorts, "skip-ports", false, "skip temporary listener availability checks")
	return cmd
}

func newStatusCommand() *cobra.Command {
	var (
		path       string
		jsonOutput bool
		timeout    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "query the local protected management status endpoint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return queryStatus(cmd.OutOrStdout(), path, jsonOutput, timeout)
		},
	}
	addConfigFlag(cmd, &path)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "write the status as formatted JSON")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "HTTP request timeout")
	return cmd
}

func newHealthcheckCommand() *cobra.Command {
	var (
		target  string
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "check an HTTP health endpoint without a configuration file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !healthcheckURLIsSafe(target) {
				return &codedError{code: 2, err: errors.New("--url must be an absolute http or https URL")}
			}
			return runHealthcheck(cmd.OutOrStdout(), target, timeout)
		},
	}
	cmd.Flags().StringVar(&target, "url", "", "health endpoint URL")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Second, "HTTP request timeout")
	_ = cmd.MarkFlagRequired("url")
	return cmd
}

func newGatewayCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "run the Gateway role",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := loadConfig(path)
			if err != nil {
				return err
			}
			if c.Role != config.RoleGateway {
				return errors.New("configuration role is not gateway; choose a gateway configuration")
			}
			opts, err := c.ResolveGateway()
			if err != nil {
				return runtimeConfigError(err, path)
			}
			events := observability.NewEventHub(0)
			trigger := lifecycle.NewShutdownTrigger()
			logger, closeLog, err := logging.New(opts.Logging, c.Role, cmd.ErrOrStderr(), events)
			if err != nil {
				return err
			}
			defer closeLog()
			s, err := gateway.NewWithOptions(opts, gateway.RuntimeOptions{Logger: logger, Events: events, ShutdownTrigger: trigger})
			if err != nil {
				return err
			}
			if err = s.Start(); err != nil {
				return err
			}
			logGatewayStarted(logger, path, opts)
			return wait(opts.Shutdown.GracePeriod, s.Shutdown, s.Close, trigger.C())
		},
	}
	addConfigFlag(cmd, &path)
	return cmd
}

func newAgentCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "run the Agent role",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := loadConfig(path)
			if err != nil {
				return err
			}
			if c.Role != config.RoleAgent {
				return errors.New("configuration role is not agent; choose an agent configuration")
			}
			opts, err := c.ResolveAgent()
			if err != nil {
				return runtimeConfigError(err, path)
			}
			events := observability.NewEventHub(0)
			trigger := lifecycle.NewShutdownTrigger()
			logger, closeLog, err := logging.New(opts.Logging, c.Role, cmd.ErrOrStderr(), events)
			if err != nil {
				return err
			}
			defer closeLog()
			a, err := agent.NewWithOptions(opts, agent.RuntimeOptions{Logger: logger, Events: events, ShutdownTrigger: trigger})
			if err != nil {
				return err
			}
			if err = a.Start(); err != nil {
				return err
			}
			logAgentStarted(logger, path, opts)
			return wait(opts.Shutdown.GracePeriod, a.Shutdown, a.Close, trigger.C())
		},
	}
	addConfigFlag(cmd, &path)
	return cmd
}

func addConfigFlag(cmd *cobra.Command, path *string) {
	cmd.Flags().StringVarP(path, "config", "c", "config.yaml", "configuration YAML file")
}

func loadConfig(path string) (*config.Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, &codedError{code: 2, err: errors.New("configuration path must not be empty")}
	}
	c, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("configuration %q was not found: %w; create it or run asterferry init", path, err)
		}
		return nil, fmt.Errorf("configuration %q is invalid: %w; run asterferry doctor after fixing YAML errors", path, err)
	}
	if err := config.ApplyEnv(c); err != nil {
		return nil, fmt.Errorf("configuration environment overrides are invalid: %w", err)
	}
	return c, nil
}

func runtimeConfigError(err error, path string) error {
	return fmt.Errorf("cannot prepare runtime from %q: %w; run asterferry doctor --config %q", path, err, path)
}

func writeDiagnosticReport(w io.Writer, report diagnostics.Report, jsonOutput bool) error {
	if jsonOutput {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	}
	if _, err := fmt.Fprintf(w, "doctor: role=%s errors=%d warnings=%d\n", report.Role, report.Errors(), report.Warnings()); err != nil {
		return err
	}
	for _, finding := range report.Findings {
		label := strings.ToUpper(string(finding.Severity))
		if _, err := fmt.Fprintf(w, "%s [%s] %s: %s\n", label, finding.Code, finding.Path, finding.Message); err != nil {
			return err
		}
		if finding.Hint != "" {
			if _, err := fmt.Fprintf(w, "  hint: %s\n", finding.Hint); err != nil {
				return err
			}
		}
	}
	return nil
}

func queryStatus(w io.Writer, path string, jsonOutput bool, timeout time.Duration) error {
	if timeout <= 0 {
		return &codedError{code: 2, err: errors.New("status timeout must be positive")}
	}
	c, err := loadConfig(path)
	if err != nil {
		return err
	}
	token, err := config.ReadToken(c.Management.AuthTokenFile)
	if err != nil {
		return fmt.Errorf("read management token: %w; run asterferry doctor --config %q", err, path)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+c.Management.Listen+"/v1/status", nil)
	if err != nil {
		return fmt.Errorf("build status request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("status endpoint %s is unavailable: %w; ensure the role is running", c.Management.Listen, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read status response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return errors.New("status request was unauthorized; verify management.auth_token_file")
		}
		return fmt.Errorf("status endpoint returned HTTP %s", resp.Status)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return fmt.Errorf("parse status response: %w", err)
	}
	if jsonOutput {
		formatted, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(formatted))
		return err
	}
	return writeHumanStatus(w, value)
}

func writeHumanStatus(w io.Writer, value any) error {
	fields, ok := value.(map[string]any)
	if !ok {
		b, _ := json.Marshal(value)
		_, err := fmt.Fprintln(w, string(b))
		return err
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := fmt.Fprintf(w, "%s: %v\n", key, fields[key]); err != nil {
			return err
		}
	}
	return nil
}

func logGatewayStarted(logger interface{ Info(string, ...any) }, path string, opts *config.GatewayOptions) {
	logger.Info("gateway started", "event", "runtime.started", "config", path, "data_listen", opts.Gateway.Listen, "management_listen", opts.Management.Listen, "node_id", opts.Cluster.NodeID, "transport_obfuscation", opts.TransportObfuscation.Mode, "agent_count", len(opts.Agents))
}

func logAgentStarted(logger interface{ Info(string, ...any) }, path string, opts *config.AgentOptions) {
	logger.Info("agent started", "event", "runtime.started", "config", path, "gateway", opts.Agent.Server, "management_listen", opts.Management.Listen, "node_id", opts.Cluster.NodeID, "agent_id", opts.Agent.ID, "transport_obfuscation", opts.TransportObfuscation.Mode, "proxy_inbounds", len(opts.Agent.Proxy.Inbounds))
}
