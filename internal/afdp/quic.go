package afdp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/quic-go/quic-go"
)

type QUICOptions struct {
	MaxStreams       int64
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	KeepAlive        time.Duration
}

func DefaultQUICOptions() QUICOptions {
	return QUICOptions{MaxStreams: 256, HandshakeTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, KeepAlive: 30 * time.Second}
}

func NewQUICConfig(options QUICOptions) *quic.Config {
	defaults := DefaultQUICOptions()
	if options.MaxStreams <= 0 {
		options.MaxStreams = defaults.MaxStreams
	} else if options.MaxStreams > 1<<20 {
		options.MaxStreams = 1 << 20
	}
	if options.HandshakeTimeout <= 0 {
		options.HandshakeTimeout = defaults.HandshakeTimeout
	} else if options.HandshakeTimeout > 24*time.Hour {
		options.HandshakeTimeout = 24 * time.Hour
	}
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = defaults.IdleTimeout
	} else if options.IdleTimeout > 7*24*time.Hour {
		options.IdleTimeout = 7 * 24 * time.Hour
	}
	if options.KeepAlive <= 0 {
		options.KeepAlive = defaults.KeepAlive
	} else if options.KeepAlive > 24*time.Hour {
		options.KeepAlive = 24 * time.Hour
	}
	return &quic.Config{MaxIncomingStreams: options.MaxStreams, HandshakeIdleTimeout: options.HandshakeTimeout, MaxIdleTimeout: options.IdleTimeout, KeepAlivePeriod: options.KeepAlive, Allow0RTT: false, EnableDatagrams: true}
}

func ServerTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load data-plane certificate: %w", err)
	}
	pool, err := readCAPool(caFile)
	if err != nil {
		return nil, err
	}
	return ServerTLSConfigFromPEM(certificate, pool), nil
}

// ServerTLSConfigFromPEM builds the same strict AFDP server configuration as
// ServerTLSConfig without forcing a node runtime to write bootstrap key
// material to temporary files.
func ServerTLSConfigFromPEM(certificate tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientCAs: clientCAs, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{ALPN}}
}

func ClientTLSConfig(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load data-plane certificate: %w", err)
	}
	pool, err := readCAPool(caFile)
	if err != nil {
		return nil, err
	}
	return ClientTLSConfigFromPEM(certificate, pool, serverName), nil
}

// ClientTLSConfigFromPEM is the in-memory counterpart to
// ClientTLSConfig. A blank serverName intentionally verifies the certificate
// chain without DNS-name matching; AFDP node certificates identify a peer by
// its Controller-issued SPIFFE URI/CN rather than by the public endpoint DNS
// name. Callers that have a separate endpoint certificate may provide a
// normal serverName for hostname verification.
func ClientTLSConfigFromPEM(certificate tls.Certificate, roots *x509.CertPool, serverName string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: roots, ServerName: serverName, NextProtos: []string{ALPN}}
}

func Listen(addr string, tlsConfig *tls.Config, options QUICOptions) (*quic.Listener, error) {
	if tlsConfig == nil {
		return nil, errors.New("data-plane TLS config is required")
	}
	// AFDP/1 has one reserved ALPN. Never let a caller accidentally expose a
	// listener that negotiates an unrelated protocol on the data endpoint.
	tlsConfig = tlsConfig.Clone()
	tlsConfig.NextProtos = []string{ALPN}
	return quic.ListenAddr(addr, tlsConfig, NewQUICConfig(options))
}

// ListenWithObfuscation binds one UDP socket and gives quic-go the AFDP
// packet wrapper. The wrapper is deliberately constructed before the QUIC
// listener so malformed or unauthenticated packets are discarded below the
// session layer.
func ListenWithObfuscation(addr string, tlsConfig *tls.Config, options QUICOptions, obfuscation ObfuscationOptions) (*quic.Listener, error) {
	if tlsConfig == nil {
		return nil, errors.New("data-plane TLS config is required")
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve data-plane address: %w", err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	packetConn, err := NewObfuscatingPacketConn(conn, obfuscation)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	tlsConfig = tlsConfig.Clone()
	tlsConfig.NextProtos = []string{ALPN}
	listener, err := quic.Listen(packetConn, tlsConfig, NewQUICConfig(options))
	if err != nil {
		_ = packetConn.Close()
		return nil, err
	}
	return listener, nil
}

func Dial(ctx context.Context, addr string, tlsConfig *tls.Config, options QUICOptions) (*quic.Conn, error) {
	if tlsConfig == nil {
		return nil, errors.New("data-plane TLS config is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tlsConfig = tlsConfig.Clone()
	tlsConfig.NextProtos = []string{ALPN}
	return quic.DialAddr(ctx, addr, tlsConfig, NewQUICConfig(options))
}

// DialWithObfuscation is the client counterpart to ListenWithObfuscation. The
// returned QUIC connection owns the packet wrapper's lifetime; callers should
// close the connection and then the returned PacketConn when the session ends.
func DialWithObfuscation(ctx context.Context, addr string, tlsConfig *tls.Config, options QUICOptions, obfuscation ObfuscationOptions) (*quic.Conn, net.PacketConn, error) {
	if tlsConfig == nil {
		return nil, nil, errors.New("data-plane TLS config is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	remote, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve data-plane address: %w", err)
	}
	local, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, nil, err
	}
	packetConn, err := NewObfuscatingPacketConn(local, obfuscation)
	if err != nil {
		_ = local.Close()
		return nil, nil, err
	}
	tlsConfig = tlsConfig.Clone()
	tlsConfig.NextProtos = []string{ALPN}
	connection, err := quic.Dial(ctx, packetConn, remote, tlsConfig, NewQUICConfig(options))
	if err != nil {
		_ = packetConn.Close()
		return nil, nil, err
	}
	return connection, packetConn, nil
}

func readCAPool(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, errors.New("data-plane CA path is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, errors.New("data-plane CA has no PEM certificates")
	}
	return pool, nil
}
