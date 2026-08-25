package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"asterferry/internal/managementclient"
	"asterferry/internal/runner"
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
	root.Example = `  asterferry init ./asterferry --profile dev
  asterferry up ./asterferry
  asterferry status ./asterferry
  asterferry doctor ./asterferry
  asterferry config show ./asterferry --role gateway
  asterferry gateway --config ./asterferry/config/gateway.yaml`

	root.AddCommand(
		newVersionCommand(),
		newCompletionCommand(),
		newInitCommand(),
		newMigrateCommand(),
		newUpCommand(),
		newDownCommand(),
		newValidateCommand(),
		newDoctorCommand(),
		newStatusCommand(),
		newConfigCommand(),
		newHealthcheckCommand(),
		newGatewayCommand(),
		newAgentCommand(),
		newSupervisorCommand(),
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
		Use:   "init [dir]",
		Short: "generate a secure Gateway-Agent configuration bundle",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if cmd.Flags().Changed("dir") {
					return &codedError{code: 2, err: errors.New("provide the bundle directory either as an argument or with --dir, not both")}
				}
				dir = args[0]
			}
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
	var path, role string
	cmd := &cobra.Command{
		Use:   "validate [dir]",
		Short: "validate YAML structure and configuration semantics",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			path, err = resolveRoleConfig(args, path, role, cmd.Flags().Changed("config"))
			if err != nil {
				return err
			}
			c, err := loadConfig(path)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "valid %s configuration\n", c.Role)
			return err
		},
	}
	addRoleConfigFlags(cmd, &path, &role)
	return cmd
}

func newDoctorCommand() *cobra.Command {
	var (
		path       string
		jsonOutput bool
		skipPorts  bool
	)
	cmd := &cobra.Command{
		Use:   "doctor [dir]",
		Short: "check local files, TLS material, secrets and listener ports",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if cmd.Flags().Changed("config") {
					return &codedError{code: 2, err: errors.New("provide a bundle directory argument or --config, not both")}
				}
				b, err := openBundle(args[0])
				if err != nil {
					return err
				}
				return checkBundle(cmd.OutOrStdout(), b, jsonOutput, skipPorts)
			}
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
		Use:   "status [dir]",
		Short: "query the local protected management status endpoint",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if cmd.Flags().Changed("config") {
					return &codedError{code: 2, err: errors.New("provide a bundle directory argument or --config, not both")}
				}
				b, err := openBundle(args[0])
				if err != nil {
					return err
				}
				return queryBundleStatus(cmd.OutOrStdout(), b, jsonOutput, timeout)
			}
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
		target      string
		timeout     time.Duration
		insecureTLS bool
	)
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "check an HTTP health endpoint without a configuration file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !healthcheckURLIsSafe(target) {
				return &codedError{code: 2, err: errors.New("--url must be an absolute http or https URL")}
			}
			return runHealthcheckWithOptions(cmd.OutOrStdout(), target, timeout, insecureTLS)
		},
	}
	cmd.Flags().StringVar(&target, "url", "", "health endpoint URL")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Second, "HTTP request timeout")
	cmd.Flags().BoolVar(&insecureTLS, "insecure-tls", false, "skip HTTPS certificate verification for a local probe")
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
			return runRole(cmd.Context(), path, config.RoleGateway, cmd.ErrOrStderr())
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
			return runRole(cmd.Context(), path, config.RoleAgent, cmd.ErrOrStderr())
		},
	}
	addConfigFlag(cmd, &path)
	return cmd
}

func runRole(ctx context.Context, path, expectedRole string, errorsOut io.Writer) error {
	err := runner.Run(ctx, runner.Options{
		ConfigPath:   path,
		ExpectedRole: expectedRole,
		Errors:       errorsOut,
		Factory: func(c *config.Config, deps runner.Dependencies) (runner.Service, runner.StartInfo, error) {
			switch expectedRole {
			case config.RoleGateway:
				opts, err := c.ResolveGateway()
				if err != nil {
					return nil, runner.StartInfo{}, runtimeConfigError(err, path)
				}
				service, err := gateway.NewWithOptions(opts, gateway.RuntimeOptions{
					Logger:          deps.Logger,
					Events:          deps.Events,
					ShutdownTrigger: deps.ShutdownTrigger,
					Config:          deps.Config,
				})
				if err != nil {
					return nil, runner.StartInfo{}, err
				}
				return service, runner.StartInfo{
					Message: "gateway started",
					Attributes: []any{
						"data_listen", opts.Gateway.Listen,
						"management_listen", opts.Management.Listen,
						"node_id", opts.Cluster.NodeID,
						"transport_obfuscation", opts.TransportObfuscation.Mode,
						"agent_count", len(opts.Agents),
					},
				}, nil
			case config.RoleAgent:
				opts, err := c.ResolveAgent()
				if err != nil {
					return nil, runner.StartInfo{}, runtimeConfigError(err, path)
				}
				service, err := agent.NewWithOptions(opts, agent.RuntimeOptions{
					Logger:          deps.Logger,
					Events:          deps.Events,
					ShutdownTrigger: deps.ShutdownTrigger,
					Config:          deps.Config,
				})
				if err != nil {
					return nil, runner.StartInfo{}, err
				}
				return service, runner.StartInfo{
					Message: "agent started",
					Attributes: []any{
						"gateway", opts.Agent.Server,
						"management_listen", opts.Management.Listen,
						"node_id", opts.Cluster.NodeID,
						"agent_id", opts.Agent.ID,
						"transport_obfuscation", opts.TransportObfuscation.Mode,
						"proxy_inbounds", len(opts.Agent.Proxy.Inbounds),
					},
				}, nil
			default:
				return nil, runner.StartInfo{}, fmt.Errorf("unsupported runtime role %q", expectedRole)
			}
		},
	})
	return err
}

func addConfigFlag(cmd *cobra.Command, path *string) {
	cmd.Flags().StringVarP(path, "config", "c", "config.yaml", "configuration YAML file")
}

func loadConfig(path string) (*config.Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, &codedError{code: 2, err: errors.New("configuration path must not be empty")}
	}
	c, err := config.LoadRuntime(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("configuration %q was not found: %w; create it or run asterferry init", path, err)
		}
		if strings.Contains(err.Error(), "auth_token_file") {
			return nil, fmt.Errorf("configuration %q uses the removed management.auth_token_file field: %w; run asterferry migrate <bundle>", path, err)
		}
		return nil, fmt.Errorf("configuration %q is invalid: %w; run asterferry doctor after fixing YAML errors", path, err)
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
	client, err := managementclient.New(c, managementclient.Viewer, timeout)
	if err != nil {
		return fmt.Errorf("prepare management client: %w; run asterferry doctor --config %q", err, path)
	}
	var value any
	if err := client.JSON(context.Background(), "GET", "/v1/status", nil, &value); err != nil {
		var apiErr *managementclient.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 401 {
			return errors.New("status request was unauthorized; verify management.auth.viewer_token_file")
		}
		return fmt.Errorf("status endpoint %s is unavailable: %w; ensure the role is running", c.Management.Listen, err)
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
