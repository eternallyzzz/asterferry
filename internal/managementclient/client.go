// Package managementclient is the small HTTP client shared by the CLI and
// local bundle supervisor. It keeps token selection, TLS trust, and API error
// handling out of individual commands.
package managementclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"asterferry/internal/config"
)

type Scope uint8

const (
	Viewer Scope = iota + 1
	Admin
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func New(c *config.Config, scope Scope, timeout time.Duration) (*Client, error) {
	if c == nil {
		return nil, errors.New("management client requires configuration")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	tokenPath := c.Management.Auth.ViewerTokenFile
	if scope == Admin {
		tokenPath = c.Management.Auth.AdminTokenFile
	}
	token, err := config.ReadToken(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read management token: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	scheme := "http"
	if c.Management.TLS.CertFile != "" {
		scheme = "https"
		roots, rootErr := x509.SystemCertPool()
		if rootErr != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if c.Management.TLS.CAFile != "" {
			caBytes, readErr := os.ReadFile(c.Management.TLS.CAFile)
			if readErr != nil {
				return nil, fmt.Errorf("read management TLS CA: %w", readErr)
			}
			if !roots.AppendCertsFromPEM(caBytes) {
				return nil, errors.New("management.tls.ca_file does not contain a certificate")
			}
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}
	}
	return &Client{
		baseURL: scheme + "://" + c.Management.Listen,
		token:   string(token),
		http:    &http.Client{Timeout: timeout, Transport: transport},
	}, nil
}

func (c *Client) Do(ctx context.Context, method, path string, payload any) (*http.Response, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("management client is unavailable")
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode management request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.baseURL, "/")+"/"+strings.TrimLeft(path, "/"), body)
	if err != nil {
		return nil, fmt.Errorf("build management request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) JSON(ctx context.Context, method, path string, payload, result any) error {
	resp, err := c.Do(ctx, method, path, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return fmt.Errorf("read management response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(body)}
	}
	if result == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("parse management response: %w", err)
	}
	return nil
}

type APIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *APIError) Error() string {
	if e == nil {
		return "management API request failed"
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(e.Body), &envelope) == nil && envelope.Error.Message != "" {
		if envelope.Error.Code != "" {
			return fmt.Sprintf("management API returned HTTP %d (%s): %s", e.StatusCode, envelope.Error.Code, envelope.Error.Message)
		}
		return fmt.Sprintf("management API returned HTTP %d: %s", e.StatusCode, envelope.Error.Message)
	}
	return fmt.Sprintf("management API returned %s", e.Status)
}
