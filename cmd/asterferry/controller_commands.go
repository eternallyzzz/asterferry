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
	"asterferry/internal/node"
	"github.com/spf13/cobra"
)

func newControllerCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "controller", Short: "run and administer the AsterFerry Controller"}
	cmd.AddCommand(newControllerInitCommand(), newControllerConfigureCommand(), newControllerRunCommand(), newControllerBackupCommand(), newControllerRestoreCommand(), newControllerMigrateCommand())
	return cmd
}

func newControllerInitCommand() *cobra.Command {
	var dir, username, password, passwordFile, httpListen, grpcListen, grpcAdvertise, releaseBaseURL, releaseVersion string
	var databaseDriver, databaseURL string
	var databaseMaxOpenConns int
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "initialize Controller CA, database and first Admin account",
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
			result, err := controller.Init(cmd.Context(), controller.InitOptions{Dir: dir, HTTPListen: httpListen, GRPCListen: grpcListen, GRPCAdvertise: grpcAdvertise, ReleaseBaseURL: releaseBaseURL, ReleaseVersion: releaseVersion, DatabaseDriver: databaseDriver, DatabaseURL: databaseURL, DatabaseMaxOpenConns: databaseMaxOpenConns, Username: username, Password: password, Force: force})
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
	cmd.Flags().StringVar(&grpcAdvertise, "grpc-advertise", "", "public Controller gRPC address used by generated node install commands")
	cmd.Flags().StringVar(&releaseBaseURL, "release-base-url", "", "official release download base URL used by generated node install commands")
	cmd.Flags().StringVar(&releaseVersion, "release-version", "", "release version used by generated node install commands (defaults to the binary version)")
	cmd.Flags().StringVar(&databaseDriver, "database-driver", controller.DatabaseDriverSQLite, "Controller database backend (sqlite or postgres)")
	cmd.Flags().StringVar(&databaseURL, "database-url", "", "PostgreSQL connection URL (required with --database-driver=postgres)")
	cmd.Flags().IntVar(&databaseMaxOpenConns, "database-max-open-conns", 0, "maximum PostgreSQL connections (default 16; ignored for SQLite)")
	cmd.Flags().BoolVar(&force, "force", false, "initialize an empty or existing directory")
	_ = cmd.MarkFlagRequired("grpc-advertise")
	return cmd
}

func newControllerConfigureCommand() *cobra.Command {
	var path, grpcAdvertise, releaseBaseURL, releaseVersion string
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "update Controller advertised address and reissue its server certificate",
		Long:  "update a stopped Controller configuration without replacing its database, CA or master key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := controller.Configure(cmd.Context(), controller.ConfigureOptions{ConfigPath: path, GRPCAdvertise: grpcAdvertise, ReleaseBaseURL: releaseBaseURL, ReleaseVersion: releaseVersion})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "controller grpc_advertise updated to %s; restart Controller to load the new certificate\n", config.GRPCAdvertise)
			return err
		},
	}
	cmd.Flags().StringVarP(&path, "config", "c", filepath.Join("controller", "controller.json"), "Controller JSON configuration")
	cmd.Flags().StringVar(&grpcAdvertise, "grpc-advertise", "", "reachable Controller gRPC address used by Nodes")
	cmd.Flags().StringVar(&releaseBaseURL, "release-base-url", "", "HTTPS release download base URL used by generated node install commands")
	cmd.Flags().StringVar(&releaseVersion, "release-version", "", "published semantic release version used by generated node install commands")
	_ = cmd.MarkFlagRequired("grpc-advertise")
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
			return instance.Wait()
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
	cmd := &cobra.Command{Use: "restore", Short: "restore a Controller backup", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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

func newControllerMigrateCommand() *cobra.Command {
	var path, targetURL, outputConfig string
	var maxOpenConns int
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "migrate a stopped SQLite Controller database to PostgreSQL",
		Long: "Validate a SQLite source and an empty PostgreSQL target, then copy the Controller state in one transaction. " +
			"Use --dry-run to validate and count rows without changing the target.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := controller.LoadConfig(path)
			if err != nil {
				return err
			}
			report, err := controller.MigrateSQLiteToPostgres(cmd.Context(), controller.SQLiteToPostgresMigrationOptions{
				SourceConfig: config, TargetURL: targetURL, OutputConfigPath: outputConfig,
				MaxOpenConns: maxOpenConns, DryRun: dryRun,
			})
			if err != nil {
				return err
			}
			mode := "migrated"
			if dryRun {
				mode = "validated"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %d rows across %d tables", mode, report.TotalRows, len(report.RowsByTable))
			if !dryRun {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "; config: %s", outputConfig)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout())
			return err
		},
	}
	cmd.Flags().StringVarP(&path, "config", "c", filepath.Join("controller", "controller.json"), "SQLite source Controller JSON configuration")
	cmd.Flags().StringVar(&targetURL, "target-url", "", "empty PostgreSQL connection URL")
	cmd.Flags().StringVar(&outputConfig, "output-config", "", "write the PostgreSQL Controller configuration here (required unless --dry-run)")
	cmd.Flags().IntVar(&maxOpenConns, "database-max-open-conns", 0, "maximum PostgreSQL connections (default 16)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate source/target and count rows without writing")
	_ = cmd.MarkFlagRequired("target-url")
	return cmd
}

func newEnrollTokenCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "enroll-token", Short: "manage one-time node enrollment tokens"}
	cmd.AddCommand(newEnrollTokenCreateCommand(), newEnrollTokenRevokeCommand())
	return cmd
}

func newEnrollTokenCreateCommand() *cobra.Command {
	var path, nodeID string
	var ttl time.Duration
	cmd := &cobra.Command{Use: "create", Short: "create a single-use node enrollment token", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		config, err := controller.LoadConfig(path)
		if err != nil {
			return err
		}
		masterKey, err := controller.LoadOrCreateMasterKey(config.MasterKeyPath)
		if err != nil {
			return err
		}
		store, err := controller.OpenStoreWithConfig(config, masterKey)
		if err != nil {
			return err
		}
		defer store.Close()
		var plain string
		var token controller.EnrollmentToken
		if strings.TrimSpace(nodeID) != "" {
			plain, token, err = store.CreateNodeEnrollmentToken(cmd.Context(), nodeID, ttl)
		} else {
			plain, token, err = store.CreateEnrollmentToken(cmd.Context(), ttl)
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "token: %s\nexpires: %s\nid: %s\n", plain, token.ExpiresAt.Format(time.RFC3339), token.ID)
		return err
	}}
	cmd.Flags().StringVarP(&path, "config", "c", filepath.Join("controller", "controller.json"), "Controller JSON configuration")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "bind the token to an existing generic Node identity")
	cmd.Flags().DurationVar(&ttl, "ttl", controller.EnrollmentTTL, "token lifetime (maximum 15m)")
	return cmd
}

func newNodeCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "node", Short: "enroll and run the generic AsterFerry node daemon"}
	cmd.AddCommand(newNodeEnrollCommand(), newNodeRunCommand())
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
		store, err := controller.OpenStoreWithConfig(config, masterKey)
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

func newNodeEnrollCommand() *cobra.Command {
	var controllerAddress, token, nodeID, output, caPath, cachePath, serverName string
	var insecure bool
	cmd := &cobra.Command{Use: "enroll", Short: "generate node bootstrap material for Controller enrollment", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.TrimSpace(controllerAddress) == "" || strings.TrimSpace(token) == "" || strings.TrimSpace(nodeID) == "" {
			return errors.New("--controller, --token and --node-id are required")
		}
		if output == "" {
			output = nodeID + "-bootstrap.json"
		}
		_, err := node.Enroll(cmd.Context(), node.EnrollOptions{ControllerAddress: controllerAddress, Token: token, NodeID: nodeID, CAPath: caPath, ServerName: serverName, InsecureSkipVerify: insecure, CachePath: cachePath, OutputPath: output})
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

func newNodeRunCommand() *cobra.Command {
	var bootstrapPath string
	cmd := &cobra.Command{Use: "run", Short: "run the generic Node daemon", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.TrimSpace(bootstrapPath) == "" {
			return &codedError{code: 2, err: errors.New("--bootstrap is required; configure behavior in the Controller Dashboard")}
		}
		return runNodeBootstrap(cmd.Context(), bootstrapPath, cmd.ErrOrStderr())
	}}
	cmd.Flags().StringVar(&bootstrapPath, "bootstrap", "", "Controller-enrolled node bootstrap JSON")
	return cmd
}

func runNodeBootstrap(ctx context.Context, path string, errorsOut io.Writer) error {
	bootstrap, err := node.LoadBootstrap(path)
	if err != nil {
		return err
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
