package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"asterferry/internal/bootstrap"
	"asterferry/internal/bundle"
	"asterferry/internal/configstore"
	"asterferry/internal/diagnostics"
	"asterferry/internal/managementclient"
	"asterferry/internal/supervisor"
	"github.com/spf13/cobra"
)

func newUpCommand() *cobra.Command {
	var detach bool
	cmd := &cobra.Command{
		Use:   "up [dir]",
		Short: "start both roles from a local bundle",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := openBundle(bundleArgument(args))
			if err != nil {
				return err
			}
			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolve asterferry executable: %w", err)
			}
			if detach {
				if err := supervisor.StartDetached(cmd.Context(), executable, b, io.Discard); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "started AsterFerry bundle in background\nrun: %s\nlogs: %s\n", b.RunDir, b.LogsDir)
				return err
			}
			return supervisor.Run(cmd.Context(), supervisor.Options{
				Executable: executable,
				Bundle:     b,
				Output:     cmd.OutOrStdout(),
				Errors:     cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().BoolVar(&detach, "detach", false, "run in the background and write logs under the bundle")
	return cmd
}

func newMigrateCommand() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate [dir]",
		Short: "migrate an older bundle to the current management configuration",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := bootstrap.Migrate(bootstrap.MigrateOptions{Dir: bundleArgument(args), DryRun: dryRun})
			if err != nil {
				return err
			}
			if len(result.Changed) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "bundle is already using the current management configuration")
				return err
			}
			if dryRun {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "migration preview (%d file(s)):\n%s", len(result.Changed), strings.Join(result.Changed, "\n"))
				_, _ = fmt.Fprintln(cmd.OutOrStdout())
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "migrated bundle; backups written alongside configuration files\nchanged:\n%s\n", strings.Join(result.Changed, "\n"))
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show migration changes without writing files")
	return cmd
}

func newDownCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "down [dir]",
		Short: "gracefully stop both roles in a local bundle",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := openBundle(bundleArgument(args))
			if err != nil {
				return err
			}
			return supervisor.Stop(cmd.Context(), b, 35*time.Second, cmd.OutOrStdout())
		},
	}
}

func newSupervisorCommand() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:    "supervise",
		Short:  "run the internal local bundle supervisor",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := openBundle(dir)
			if err != nil {
				return err
			}
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			return supervisor.Run(cmd.Context(), supervisor.Options{Executable: executable, Bundle: b, Output: io.Discard, Errors: io.Discard})
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "bundle directory")
	_ = cmd.MarkFlagRequired("dir")
	return cmd
}

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "inspect and apply a role configuration"}
	cmd.AddCommand(newConfigShowCommand(), newConfigValidateCommand(), newConfigApplyCommand(), newConfigRollbackCommand())
	return cmd
}

func newConfigShowCommand() *cobra.Command {
	var path string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "show a redacted configuration snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := configstore.New(path)
			if err != nil {
				return err
			}
			snapshot, err := manager.Snapshot()
			if err != nil {
				return err
			}
			if jsonOutput {
				data, err := json.MarshalIndent(snapshot, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), snapshot.YAML)
			return err
		},
	}
	addConfigFlag(cmd, &path)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "write the snapshot as JSON")
	return cmd
}

func newConfigValidateCommand() *cobra.Command {
	var path, candidatePath string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "validate a role configuration or candidate YAML",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if candidatePath == "" {
				c, err := loadConfig(path)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "valid %s configuration\n", c.Role)
				return err
			}
			manager, err := configstore.New(path)
			if err != nil {
				return err
			}
			snapshot, err := manager.Snapshot()
			if err != nil {
				return err
			}
			candidate, err := os.ReadFile(candidatePath)
			if err != nil {
				return fmt.Errorf("read candidate configuration: %w", err)
			}
			result, err := manager.Validate(snapshot.Revision, candidate)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "valid %s configuration; changed=%t\n%s", result.Role, result.Changed, result.Diff)
			return err
		},
	}
	addConfigFlag(cmd, &path)
	cmd.Flags().StringVar(&candidatePath, "file", "", "candidate YAML file")
	return cmd
}

func newConfigApplyCommand() *cobra.Command {
	var path, candidatePath string
	var yes bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "apply candidate YAML through the running role API",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if candidatePath == "" {
				return &codedError{code: 2, err: errors.New("--file is required")}
			}
			manager, err := configstore.New(path)
			if err != nil {
				return err
			}
			snapshot, err := manager.Snapshot()
			if err != nil {
				return err
			}
			candidate, err := os.ReadFile(candidatePath)
			if err != nil {
				return fmt.Errorf("read candidate configuration: %w", err)
			}
			validation, err := manager.Validate(snapshot.Revision, candidate)
			if err != nil {
				return err
			}
			if !validation.Changed {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "no configuration changes detected")
				return err
			}
			if err := confirm(cmd, yes, "Apply this configuration and request a graceful restart? "); err != nil {
				return err
			}
			c, err := loadConfig(path)
			if err != nil {
				return err
			}
			client, err := managementclient.New(c, managementclient.Admin, 5*time.Second)
			if err != nil {
				return err
			}
			var result map[string]any
			err = client.JSON(cmd.Context(), "POST", "/v1/config/apply", map[string]any{"base_revision": snapshot.Revision, "yaml": string(candidate)}, &result)
			if err != nil {
				return fmt.Errorf("apply configuration: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "configuration applied; supervisor restart requested\n")
			return err
		},
	}
	addConfigFlag(cmd, &path)
	cmd.Flags().StringVar(&candidatePath, "file", "", "candidate YAML file")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func newConfigRollbackCommand() *cobra.Command {
	var path string
	var yes bool
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "restore the previous configuration through the running role API",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := configstore.New(path)
			if err != nil {
				return err
			}
			snapshot, err := manager.Snapshot()
			if err != nil {
				return err
			}
			if !snapshot.BackupAvailable {
				return errors.New("no configuration backup is available")
			}
			if err := confirm(cmd, yes, "Restore the previous configuration and request a graceful restart? "); err != nil {
				return err
			}
			c, err := loadConfig(path)
			if err != nil {
				return err
			}
			client, err := managementclient.New(c, managementclient.Admin, 5*time.Second)
			if err != nil {
				return err
			}
			var result map[string]any
			if err := client.JSON(cmd.Context(), "POST", "/v1/config/rollback", map[string]any{"base_revision": snapshot.Revision}, &result); err != nil {
				return fmt.Errorf("rollback configuration: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "previous configuration restored; supervisor restart requested")
			return err
		},
	}
	addConfigFlag(cmd, &path)
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func confirm(cmd *cobra.Command, yes bool, prompt string) error {
	if yes {
		return nil
	}
	if cmd.InOrStdin() == nil {
		return errors.New("confirmation is required; rerun with --yes")
	}
	if _, err := fmt.Fprint(cmd.OutOrStdout(), prompt+"[y/N] "); err != nil {
		return err
	}
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
		return nil
	}
	return errors.New("operation cancelled")
}

func openBundle(path string) (bundle.Bundle, error) {
	b, err := bundle.Open(path)
	if err != nil {
		return bundle.Bundle{}, err
	}
	return b, nil
}

func bundleArgument(args []string) string {
	if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
		return args[0]
	}
	return "asterferry"
}

type bundleRoleStatus struct {
	Role   string `json:"role"`
	Config string `json:"config"`
	Status any    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

func queryBundleStatus(w io.Writer, b bundle.Bundle, jsonOutput bool, timeout time.Duration) error {
	result := make([]bundleRoleStatus, 0, 2)
	failed := 0
	for _, item := range []struct {
		role string
		path string
	}{{bundle.GatewayRole, b.GatewayConfig}, {bundle.AgentRole, b.AgentConfig}} {
		entry := bundleRoleStatus{Role: item.role, Config: item.path}
		c, err := loadConfig(item.path)
		if err == nil {
			client, clientErr := managementclient.New(c, managementclient.Viewer, timeout)
			if clientErr == nil {
				var value any
				err = client.JSON(context.Background(), "GET", "/v1/status", nil, &value)
				if err == nil {
					entry.Status = value
				}
			} else {
				err = clientErr
			}
		}
		if err != nil {
			entry.Error = err.Error()
			failed++
		}
		result = append(result, entry)
	}
	if jsonOutput {
		data, err := json.MarshalIndent(map[string]any{"bundle": b.Root, "roles": result}, "", "  ")
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, string(data)); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(w, "bundle: %s\n", b.Root); err != nil {
			return err
		}
		for _, entry := range result {
			if entry.Error != "" {
				if _, err := fmt.Fprintf(w, "%s: unavailable: %s\n", entry.Role, entry.Error); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(w, "%s:\n", entry.Role); err != nil {
				return err
			}
			if err := writeHumanStatus(w, entry.Status); err != nil {
				return err
			}
		}
	}
	if failed > 0 {
		return &codedError{code: 1, err: fmt.Errorf("%d bundle role(s) are unavailable", failed)}
	}
	return nil
}

func checkBundle(w io.Writer, b bundle.Bundle, jsonOutput, skipPorts bool) error {
	paths := []string{b.GatewayConfig, b.AgentConfig}
	combined := make([]diagnostics.Finding, 0)
	roles := make([]diagnostics.Report, 0, len(paths))
	for _, path := range paths {
		c, err := loadConfig(path)
		if err != nil {
			return err
		}
		report := diagnostics.Check(c, skipPorts)
		roles = append(roles, report)
		combined = append(combined, report.Findings...)
	}
	if jsonOutput {
		data, err := json.MarshalIndent(roles, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(data))
		return err
	}
	for index, report := range roles {
		if index > 0 {
			_, _ = fmt.Fprintln(w)
		}
		if err := writeDiagnosticReport(w, report, false); err != nil {
			return err
		}
	}
	errorsFound := 0
	warningsFound := 0
	for _, finding := range combined {
		if finding.Severity == diagnostics.SeverityError {
			errorsFound++
		}
		if finding.Severity == diagnostics.SeverityWarn {
			warningsFound++
		}
	}
	if errorsFound > 0 {
		return &codedError{code: 1, err: fmt.Errorf("doctor found %d error(s) and %d warning(s)", errorsFound, warningsFound)}
	}
	return nil
}
