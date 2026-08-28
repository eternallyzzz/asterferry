package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"asterferry/internal/controller"
	"asterferry/internal/domain"
	"asterferry/internal/node"
	"github.com/spf13/cobra"
)

func newControllerCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "controller", Short: "run and administer the AsterFerry Controller"}
	cmd.AddCommand(newControllerInitCommand(), newControllerRunCommand(), newControllerBackupCommand(), newControllerRestoreCommand())
	return cmd
}

func newControllerInitCommand() *cobra.Command {
	var dir, username, password, passwordFile, httpListen, grpcListen string
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "initialize Controller CA, SQLite database and first Admin account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if passwordFile != "" {
				data, err := os.ReadFile(passwordFile)
				if err != nil {
					return err
				}
				password = strings.TrimSpace(string(data))
			}
			generated := false
			if password == "" {
				var err error
				password, err = generateInitialPassword()
				if err != nil {
					return err
				}
				generated = true
			}
			result, err := controller.Init(cmd.Context(), controller.InitOptions{Dir: dir, HTTPListen: httpListen, GRPCListen: grpcListen, Username: username, Password: password, Force: force})
			if err != nil {
				return err
			}
			if generated {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "controller initialized in %s\nconfig: %s\nadmin username: %s\ninitial admin password (shown once): %s\n", dir, result.ConfigPath, result.Admin.Username, password)
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "controller initialized in %s\nconfig: %s\nadmin username: %s\n", dir, result.ConfigPath, result.Admin.Username)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "controller", "Controller data directory")
	cmd.Flags().StringVar(&username, "username", "admin", "initial Admin username")
	cmd.Flags().StringVar(&password, "password", "", "initial Admin password (prefer --password-file or an interactive secret)")
	cmd.Flags().StringVar(&passwordFile, "password-file", "", "read the initial Admin password from a protected file")
	cmd.Flags().StringVar(&httpListen, "http-listen", "", "HTTPS listen address (default :8443)")
	cmd.Flags().StringVar(&grpcListen, "grpc-listen", "", "mTLS gRPC listen address (default :9443)")
	cmd.Flags().BoolVar(&force, "force", false, "initialize an empty or existing directory")
	return cmd
}

func newControllerRunCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "run the HTTPS REST and mTLS gRPC Controller servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := controller.LoadConfig(path)
			if err != nil {
				return err
			}
			instance, err := controller.New(config)
			if err != nil {
				return err
			}
			defer instance.Close()
			if err := instance.Start(cmd.Context()); err != nil {
				return err
			}
			<-cmd.Context().Done()
			return nil
		},
	}
	cmd.Flags().StringVarP(&path, "config", "c", filepath.Join("controller", "controller.json"), "Controller JSON configuration")
	return cmd
}

func newControllerBackupCommand() *cobra.Command {
	var path, output string
	cmd := &cobra.Command{Use: "backup", Short: "create a consistent Controller backup", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		config, err := controller.LoadConfig(path)
		if err != nil {
			return err
		}
		result, err := controller.Backup(cmd.Context(), config, output)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "controller backup written to %s\n", result)
		return err
	}}
	cmd.Flags().StringVarP(&path, "config", "c", filepath.Join("controller", "controller.json"), "Controller JSON configuration")
	cmd.Flags().StringVarP(&output, "output", "o", "", "backup directory")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

func newControllerRestoreCommand() *cobra.Command {
	var path, source, destination string
	cmd := &cobra.Command{Use: "restore", Short: "restore a Controller SQLite backup", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		config, err := controller.LoadConfig(path)
		if err != nil {
			return err
		}
		if err := controller.Restore(config, source, destination); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "controller backup restored from %s\n", source)
		return err
	}}
	cmd.Flags().StringVarP(&path, "config", "c", filepath.Join("controller", "controller.json"), "Controller JSON configuration")
	cmd.Flags().StringVarP(&source, "source", "s", "", "backup directory")
	cmd.Flags().StringVar(&destination, "destination", "", "destination Controller data directory")
	_ = cmd.MarkFlagRequired("source")
	return cmd
}

func newEnrollTokenCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "enroll-token", Short: "manage one-time node enrollment tokens"}
	cmd.AddCommand(newEnrollTokenCreateCommand(), newEnrollTokenRevokeCommand())
	return cmd
}

func newEnrollTokenCreateCommand() *cobra.Command {
	var path, role string
	var ttl time.Duration
	cmd := &cobra.Command{Use: "create", Short: "create a single-use, role-bound enrollment token", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		config, err := controller.LoadConfig(path)
		if err != nil {
			return err
		}
		masterKey, err := controller.LoadOrCreateMasterKey(config.MasterKeyPath)
		if err != nil {
			return err
		}
		store, err := controller.OpenStore(config.DatabasePath, masterKey)
		if err != nil {
			return err
		}
		defer store.Close()
		plain, token, err := store.CreateEnrollmentToken(cmd.Context(), role, ttl)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "token: %s\nrole: %s\nexpires: %s\nid: %s\n", plain, token.Role, token.ExpiresAt.Format(time.RFC3339), token.ID)
		return err
	}}
	cmd.Flags().StringVarP(&path, "config", "c", filepath.Join("controller", "controller.json"), "Controller JSON configuration")
	cmd.Flags().StringVar(&role, "role", "", "node role: gateway or agent")
	cmd.Flags().DurationVar(&ttl, "ttl", controller.EnrollmentTTL, "token lifetime (maximum 15m)")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

func newEnrollTokenRevokeCommand() *cobra.Command {
	var path, id string
	cmd := &cobra.Command{Use: "revoke", Short: "revoke an unused enrollment token", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		config, err := controller.LoadConfig(path)
		if err != nil {
			return err
		}
		masterKey, err := controller.LoadOrCreateMasterKey(config.MasterKeyPath)
		if err != nil {
			return err
		}
		store, err := controller.OpenStore(config.DatabasePath, masterKey)
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.RevokeEnrollmentToken(cmd.Context(), id); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "enrollment token revoked")
		return err
	}}
	cmd.Flags().StringVarP(&path, "config", "c", filepath.Join("controller", "controller.json"), "Controller JSON configuration")
	cmd.Flags().StringVar(&id, "id", "", "enrollment token id")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func newNodeEnrollCommand(role string) *cobra.Command {
	var controllerAddress, token, nodeID, output, caPath, cachePath, serverName string
	var insecure bool
	cmd := &cobra.Command{Use: "enroll", Short: "generate node bootstrap material for Controller enrollment", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.TrimSpace(controllerAddress) == "" || strings.TrimSpace(token) == "" || strings.TrimSpace(nodeID) == "" {
			return errors.New("--controller, --token and --node-id are required")
		}
		if output == "" {
			output = nodeID + "-bootstrap.json"
		}
		_, err := node.Enroll(cmd.Context(), node.EnrollOptions{ControllerAddress: controllerAddress, Token: token, NodeID: nodeID, Role: role, CAPath: caPath, ServerName: serverName, InsecureSkipVerify: insecure, CachePath: cachePath, OutputPath: output})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "node bootstrap written to %s\nkeep it private and complete enrollment before starting the node\n", output)
		return err
	}}
	cmd.Flags().StringVar(&controllerAddress, "controller", "", "Controller gRPC address")
	cmd.Flags().StringVar(&caPath, "ca", "", "Controller CA PEM path")
	cmd.Flags().StringVar(&serverName, "server-name", "", "TLS server name used to verify the Controller certificate")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "skip Controller certificate verification for a local bootstrap only")
	cmd.Flags().StringVar(&token, "token", "", "one-time enrollment token")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "immutable node id")
	cmd.Flags().StringVar(&cachePath, "cache", "", "encrypted desired-state cache path")
	cmd.Flags().StringVar(&output, "output", "", "bootstrap JSON output path")
	return cmd
}

func newGatewayCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "gateway", Short: "enroll or run a Gateway data-plane node", Args: cobra.NoArgs}
	cmd.AddCommand(newNodeEnrollCommand(domain.RoleGateway), newGatewayRunCommand())
	return cmd
}

func newAgentCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "enroll or run an Agent data-plane node", Args: cobra.NoArgs}
	cmd.AddCommand(newNodeEnrollCommand(domain.RoleAgent), newAgentRunCommand())
	return cmd
}

func newGatewayRunCommand() *cobra.Command {
	var bootstrapPath string
	cmd := &cobra.Command{Use: "run", Short: "run a Gateway data-plane node", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.TrimSpace(bootstrapPath) == "" {
			return &codedError{code: 2, err: errors.New("--bootstrap is required; business configuration is owned by the Controller")}
		}
		return runNodeBootstrap(cmd.Context(), bootstrapPath, domain.RoleGateway, cmd.ErrOrStderr())
	}}
	cmd.Flags().StringVar(&bootstrapPath, "bootstrap", "", "Controller-enrolled node bootstrap JSON")
	return cmd
}

func newAgentRunCommand() *cobra.Command {
	var bootstrapPath string
	cmd := &cobra.Command{Use: "run", Short: "run an Agent data-plane node", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.TrimSpace(bootstrapPath) == "" {
			return &codedError{code: 2, err: errors.New("--bootstrap is required; business configuration is owned by the Controller")}
		}
		return runNodeBootstrap(cmd.Context(), bootstrapPath, domain.RoleAgent, cmd.ErrOrStderr())
	}}
	cmd.Flags().StringVar(&bootstrapPath, "bootstrap", "", "Controller-enrolled node bootstrap JSON")
	return cmd
}

func runNodeBootstrap(ctx context.Context, path, role string, errorsOut io.Writer) error {
	bootstrap, err := node.LoadBootstrap(path)
	if err != nil {
		return err
	}
	if bootstrap.Role != role {
		return fmt.Errorf("bootstrap role %q cannot run as %s", bootstrap.Role, role)
	}
	runtime, err := node.NewRuntime(bootstrap, node.RuntimeOptions{BootstrapPath: path, Logger: slog.New(slog.NewTextHandler(errorsOut, &slog.HandlerOptions{Level: slog.LevelInfo}))})
	if err != nil {
		return err
	}
	return runtime.Run(ctx)
}

func generateInitialPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return "Af-" + base64.RawURLEncoding.EncodeToString(b), nil
}
