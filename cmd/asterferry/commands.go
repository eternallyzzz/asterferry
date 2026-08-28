package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"asterferry/internal/buildinfo"
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
		Short:         "AsterFerry Controller and AFDP/1 data-plane node",
		Long:          "AsterFerry keeps identity, desired state and scheduling in the Controller; Gateway and Agent processes only run AFDP/1 data-plane state.",
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
	root.Example = `  asterferry controller init --dir ./controller
  asterferry enroll-token create --config ./controller/controller.json --role gateway
  asterferry gateway enroll --controller controller.example:9443 --token <one-time-token> --node-id gw-east --ca ./controller/ca/ca.crt
  asterferry gateway run --bootstrap gw-east-bootstrap.json`
	root.AddCommand(
		newVersionCommand(),
		newCompletionCommand(),
		newHealthcheckCommand(),
		newControllerCommand(),
		newEnrollTokenCommand(),
		newGatewayCommand(),
		newAgentCommand(),
	)
	return root
}

func newVersionCommand() *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "show build and AFDP/control wire versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Current()
			if short {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), info.Version)
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "asterferry %s\ncommit: %s\nbuilt: %s\nprotocol: %s\ngo: %s\nplatform: %s/%s\n", info.Version, info.Commit, info.BuildDate, info.Protocol, info.GoVersion, info.OS, info.Architecture)
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
			switch strings.ToLower(args[0]) {
			case "bash":
				return cmd.Root().GenBashCompletion(cmd.OutOrStdout())
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			default:
				return &codedError{code: 2, err: fmt.Errorf("unsupported shell %q; choose bash, powershell or zsh", args[0])}
			}
		},
	}
}

func newHealthcheckCommand() *cobra.Command {
	var target string
	var timeout time.Duration
	var insecureTLS bool
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
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Second, "healthcheck timeout")
	cmd.Flags().BoolVar(&insecureTLS, "insecure-tls", false, "skip HTTPS certificate verification for a local probe")
	_ = cmd.MarkFlagRequired("url")
	return cmd
}
