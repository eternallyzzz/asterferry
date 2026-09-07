package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func runHealthcheckWithOptions(out io.Writer, target string, timeout time.Duration, insecureTLS bool) error {
	parsed, err := parseHealthcheckURL(target)
	if err != nil {
		return err
	}
	return runHealthcheckURL(out, parsed, timeout, insecureTLS)
}

func parseHealthcheckURL(target string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(target))
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("healthcheck URL must be an absolute http or https URL")
	}
	return parsed, nil
}

func runHealthcheckURL(out io.Writer, parsed *url.URL, timeout time.Duration, insecureTLS bool) error {
	if timeout <= 0 {
		return fmt.Errorf("healthcheck timeout must be positive")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if parsed.Scheme == "https" && insecureTLS {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true} //nolint:gosec // explicitly requested for local probes
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return fmt.Errorf("build healthcheck request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("healthcheck request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("healthcheck returned HTTP %s", response.Status)
	}
	if out != nil {
		_, _ = fmt.Fprintln(out, "ok")
	}
	return nil
}

func healthcheckURLIsSafe(target string) bool {
	_, err := parseHealthcheckURL(target)
	return err == nil
}
