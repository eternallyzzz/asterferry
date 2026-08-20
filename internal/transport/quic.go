package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/net/quic"

	"asterferry/internal/config"
)

func Listen(cfg *config.Config) (*quic.Endpoint, error) {
	if cfg == nil || cfg.Role != config.RoleGateway || cfg.Gateway == nil {
		return nil, errors.New("gateway configuration is required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.Gateway.TLS.CertFile, cfg.Gateway.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load gateway certificate: %w", err)
	}
	caBytes, err := os.ReadFile(cfg.Gateway.TLS.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read agent client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("gateway.tls.client_ca_file does not contain a certificate")
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{cfg.Transport.ALPN},
	}
	return quic.Listen("udp", cfg.Gateway.Listen, quicConfig(tlsCfg, cfg))
}

func ValidateAgentCredentials(cfg *config.Config) error {
	if cfg == nil || cfg.Role != config.RoleAgent || cfg.Agent == nil {
		return errors.New("agent configuration is required")
	}
	caBytes, err := os.ReadFile(cfg.Agent.TLS.CAFile)
	if err != nil {
		return fmt.Errorf("read gateway CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return errors.New("agent.tls.ca_file does not contain a certificate")
	}
	if _, err := tls.LoadX509KeyPair(cfg.Agent.TLS.CertFile, cfg.Agent.TLS.KeyFile); err != nil {
		return fmt.Errorf("load agent certificate: %w", err)
	}
	if _, err := config.ReadToken(cfg.Agent.TokenFile); err != nil {
		return err
	}
	return nil
}

func Dial(ctx context.Context, cfg *config.Config) (*quic.Endpoint, *quic.Conn, error) {
	if cfg == nil || cfg.Role != config.RoleAgent || cfg.Agent == nil {
		return nil, nil, errors.New("agent configuration is required")
	}
	caBytes, err := os.ReadFile(cfg.Agent.TLS.CAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read gateway CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, nil, errors.New("agent.tls.ca_file does not contain a certificate")
	}
	cert, err := tls.LoadX509KeyPair(cfg.Agent.TLS.CertFile, cfg.Agent.TLS.KeyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("load agent certificate: %w", err)
	}
	tlsCfg := &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   cfg.Agent.TLS.ServerName,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{cfg.Transport.ALPN},
	}
	ep, err := quic.Listen("udp", "0.0.0.0:0", quicConfig(tlsCfg, cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("create client endpoint: %w", err)
	}
	conn, err := ep.Dial(ctx, "udp", cfg.Agent.Server, quicConfig(tlsCfg, cfg))
	if err != nil {
		_ = ep.Close(context.Background())
		return nil, nil, fmt.Errorf("dial gateway: %w", err)
	}
	return ep, conn, nil
}

func quicConfig(tlsCfg *tls.Config, cfg *config.Config) *quic.Config {
	q := &quic.Config{
		TLSConfig:                tlsCfg,
		MaxBidiRemoteStreams:     cfg.Transport.MaxBidiRemoteStreams,
		MaxStreamReadBufferSize:  cfg.Transport.MaxStreamReadBuffer,
		MaxStreamWriteBufferSize: cfg.Transport.MaxStreamWriteBuffer,
		MaxConnReadBufferSize:    cfg.Transport.MaxConnReadBuffer,
		HandshakeTimeout:         time.Duration(cfg.Transport.HandshakeTimeoutSec) * time.Second,
		MaxIdleTimeout:           time.Duration(cfg.Transport.IdleTimeoutSec) * time.Second,
		KeepAlivePeriod:          time.Duration(cfg.Transport.KeepAliveSec) * time.Second,
		RequireAddressValidation: false,
	}
	return q
}
