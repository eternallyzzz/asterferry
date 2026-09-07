package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newControllerUpdateCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "update", Short: "inspect or apply stable Controller and Node upgrade state"}
	cmd.AddCommand(newControllerUpdateStatusCommand(), newControllerUpdateCheckCommand(), newControllerUpdateApplyCommand(), newNodeUpdateCommand())
	return cmd
}

type controllerUpdateCLIFlags struct {
	URL         string
	Token       string
	InsecureTLS bool
}

func (flags *controllerUpdateCLIFlags) bind(command *cobra.Command) {
	command.Flags().StringVar(&flags.URL, "url", "", "Controller HTTPS base URL (or ASTERFERRY_CONTROLLER_URL)")
	command.Flags().StringVar(&flags.Token, "token", "", "Controller API token (or ASTERFERRY_API_TOKEN)")
	command.Flags().BoolVar(&flags.InsecureTLS, "insecure-tls", false, "skip Controller certificate verification for a local probe")
}

func newControllerUpdateStatusCommand() *cobra.Command {
	var flags controllerUpdateCLIFlags
	command := &cobra.Command{
		Use:   "status",
		Short: "show Controller stable-release status",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runControllerUpdateCLI(command.Context(), command.OutOrStdout(), flags, http.MethodGet, "/api/v1/controller/update", nil)
		},
	}
	flags.bind(command)
	return command
}

func newControllerUpdateCheckCommand() *cobra.Command {
	var flags controllerUpdateCLIFlags
	command := &cobra.Command{
		Use:   "check",
		Short: "check GitHub for the newest stable release",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runControllerUpdateCLI(command.Context(), command.OutOrStdout(), flags, http.MethodPost, "/api/v1/controller/update/check", []byte("{}"))
		},
	}
	flags.bind(command)
	return command
}

func newControllerUpdateApplyCommand() *cobra.Command {
	var flags controllerUpdateCLIFlags
	var version string
	command := &cobra.Command{
		Use:   "apply",
		Short: "confirm and apply the newest stable Controller release",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			body := []byte("{}")
			if strings.TrimSpace(version) != "" {
				encoded, err := json.Marshal(map[string]string{"version": strings.TrimPrefix(strings.TrimSpace(version), "v")})
				if err != nil {
					return err
				}
				body = encoded
			}
			return runControllerUpdateCLI(command.Context(), command.OutOrStdout(), flags, http.MethodPost, "/api/v1/controller/update/apply", body)
		},
	}
	flags.bind(command)
	command.Flags().StringVar(&version, "version", "", "expected stable version; empty means the latest detected version")
	return command
}

func newNodeUpdateCommand() *cobra.Command {
	var flags controllerUpdateCLIFlags
	var nodeID string
	command := &cobra.Command{
		Use:   "node",
		Short: "confirm and dispatch a self-upgrade to one Node",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if strings.TrimSpace(nodeID) == "" {
				return errors.New("--node-id is required")
			}
			path := "/api/v1/nodes/" + url.PathEscape(strings.TrimSpace(nodeID)) + "/actions/upgrade"
			return runControllerUpdateCLI(command.Context(), command.OutOrStdout(), flags, http.MethodPost, path, []byte("{}"))
		},
	}
	flags.bind(command)
	command.Flags().StringVar(&nodeID, "node-id", "", "Node identity to upgrade")
	return command
}

func runControllerUpdateCLI(ctx context.Context, output io.Writer, flags controllerUpdateCLIFlags, method, path string, body []byte) error {
	base := strings.TrimSpace(flags.URL)
	if base == "" {
		base = strings.TrimSpace(os.Getenv("ASTERFERRY_CONTROLLER_URL"))
	}
	if base == "" {
		base = "https://127.0.0.1:8443"
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("--url must be an absolute HTTPS Controller URL without credentials or query parameters")
	}
	token := strings.TrimSpace(flags.Token)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("ASTERFERRY_API_TOKEN"))
	}
	if token == "" {
		return errors.New("--token or ASTERFERRY_API_TOKEN is required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if flags.InsecureTLS {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if readErr != nil {
		return readErr
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("controller update request failed with HTTP %s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	var formatted any
	if err := json.Unmarshal(data, &formatted); err != nil {
		_, _ = output.Write(data)
		_, _ = fmt.Fprintln(output)
		return nil
	}
	encoded, err := json.MarshalIndent(formatted, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, string(encoded))
	return err
}
