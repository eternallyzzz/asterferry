package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"

	"asterferry/internal/config"
	"asterferry/internal/identity"
)

const closeErrorCode quic.ApplicationErrorCode = 0x100

// Listener, Session and Stream are the transport boundary used by the role
// runtimes. quic-go types stay inside this package; callers only depend on
// the operations required by the application protocol.
type Listener interface {
	Accept(context.Context) (Session, error)
	// StopAccepting releases the listener socket while preserving accepted
	// sessions. Close additionally tears down the underlying QUIC transport.
	StopAccepting() error
	Close() error
}

type Session interface {
	OpenStream(context.Context) (Stream, error)
	AcceptStream(context.Context) (Stream, error)
	Context() context.Context
	RemoteAddr() net.Addr
	PeerCertificates() []*x509.Certificate
	Close() error
}

type Stream interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
	Context() context.Context
	Cancel()
}

// ConnectionStats is the small diagnostic view exposed by the transport
// boundary. It intentionally contains counters and capability bits only; the
// runtime protocol does not depend on quic-go's concrete types.
type ConnectionStats struct {
	RTT             time.Duration
	BytesSent       uint64
	BytesReceived   uint64
	BytesLost       uint64
	PacketsSent     uint64
	PacketsReceived uint64
	PacketsLost     uint64
	GSO             bool
	Version         string
}

// StatsProvider is implemented by transports that can expose native QUIC
// diagnostics. It remains optional so test and alternate transports need not
// emulate quic-go internals.
type StatsProvider interface {
	Stats() ConnectionStats
}

type quicListener struct {
	listener  *quic.Listener
	closeFn   func() error
	stopOnce  sync.Once
	stopErr   error
	closeOnce sync.Once
	closeErr  error
}

func (l *quicListener) Accept(ctx context.Context) (Session, error) {
	if l == nil || l.listener == nil {
		return nil, errors.New("listener is closed")
	}
	conn, err := l.listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return &quicSession{conn: conn}, nil
}

func (l *quicListener) StopAccepting() error {
	if l == nil {
		return nil
	}
	l.stopOnce.Do(func() {
		if l.listener != nil {
			l.stopErr = l.listener.Close()
		}
	})
	return l.stopErr
}

func (l *quicListener) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		l.closeErr = l.StopAccepting()
		if l.closeFn != nil {
			if err := l.closeFn(); l.closeErr == nil {
				l.closeErr = err
			}
		}
	})
	return l.closeErr
}

type quicSession struct {
	conn      *quic.Conn
	closeFn   func() error
	closeOnce sync.Once
	closeErr  error
}

func (s *quicSession) OpenStream(ctx context.Context) (Stream, error) {
	if s == nil || s.conn == nil {
		return nil, errors.New("session is closed")
	}
	stream, err := s.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return &quicStream{stream: stream}, nil
}

func (s *quicSession) AcceptStream(ctx context.Context) (Stream, error) {
	if s == nil || s.conn == nil {
		return nil, errors.New("session is closed")
	}
	stream, err := s.conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &quicStream{stream: stream}, nil
}

func (s *quicSession) Context() context.Context {
	if s == nil || s.conn == nil {
		return context.Background()
	}
	return s.conn.Context()
}

func (s *quicSession) RemoteAddr() net.Addr {
	if s == nil || s.conn == nil {
		return nil
	}
	return s.conn.RemoteAddr()
}

func (s *quicSession) PeerCertificates() []*x509.Certificate {
	if s == nil || s.conn == nil {
		return nil
	}
	peer := s.conn.ConnectionState().TLS.PeerCertificates
	return append([]*x509.Certificate(nil), peer...)
}

func (s *quicSession) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.conn != nil {
			s.closeErr = s.conn.CloseWithError(closeErrorCode, "session closed")
		}
		if s.closeFn != nil {
			if err := s.closeFn(); s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}

func (s *quicSession) Stats() ConnectionStats {
	if s == nil || s.conn == nil {
		return ConnectionStats{}
	}
	state := s.conn.ConnectionState()
	stats := s.conn.ConnectionStats()
	return ConnectionStats{
		RTT:             stats.SmoothedRTT,
		BytesSent:       stats.BytesSent,
		BytesReceived:   stats.BytesReceived,
		BytesLost:       stats.BytesLost,
		PacketsSent:     stats.PacketsSent,
		PacketsReceived: stats.PacketsReceived,
		PacketsLost:     stats.PacketsLost,
		GSO:             state.GSO,
		Version:         state.Version.String(),
	}
}

// SessionStats returns native connection diagnostics when supported.
func SessionStats(session Session) (ConnectionStats, bool) {
	provider, ok := session.(StatsProvider)
	if !ok || provider == nil {
		return ConnectionStats{}, false
	}
	return provider.Stats(), true
}

type quicStream struct{ stream *quic.Stream }

func (s *quicStream) Read(p []byte) (int, error)  { return s.stream.Read(p) }
func (s *quicStream) Write(p []byte) (int, error) { return s.stream.Write(p) }
func (s *quicStream) Close() error                { return s.stream.Close() }
func (s *quicStream) SetDeadline(deadline time.Time) error {
	return s.stream.SetDeadline(deadline)
}
func (s *quicStream) Context() context.Context { return s.stream.Context() }
func (s *quicStream) Cancel() {
	s.stream.CancelRead(0)
	s.stream.CancelWrite(0)
}

// Listen creates the Gateway QUIC listener. TLS is deliberately kept outside
// quic.Config: quic-go accepts it as a separate argument to ListenAddr.
func Listen(cfg *config.GatewayOptions, metrics ...ObfuscationMetrics) (Listener, error) {
	if cfg == nil {
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
	addr, err := net.ResolveUDPAddr("udp", cfg.Gateway.Listen)
	if err != nil {
		return nil, fmt.Errorf("resolve gateway listen address: %w", err)
	}
	udpConn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen gateway UDP: %w", err)
	}
	if err := configureUDPBuffers(udpConn, cfg.Transport); err != nil {
		_ = udpConn.Close()
		return nil, err
	}
	packetConn, err := NewObfuscatingPacketConn(udpConn, cfg.TransportObfuscation, metrics...)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("configure transport obfuscation: %w", err)
	}
	qt := &quic.Transport{Conn: packetConn}
	listener, err := qt.Listen(tlsCfg, quicConfigWithObfuscation(cfg.Transport, cfg.TransportObfuscation))
	if err != nil {
		_ = closeTransport(qt, packetConn)
		return nil, err
	}
	return &quicListener{listener: listener, closeFn: func() error { return closeTransport(qt, packetConn) }}, nil
}

func ValidateAgentCredentials(cfg *config.AgentOptions) error {
	if cfg == nil {
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
	cert, err := tls.LoadX509KeyPair(cfg.Agent.TLS.CertFile, cfg.Agent.TLS.KeyFile)
	if err != nil {
		return fmt.Errorf("load agent certificate: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return errors.New("agent certificate is empty")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse agent certificate: %w", err)
	}
	certAgentID, ok := CertificateAgentID(leaf)
	if !ok || certAgentID != cfg.Agent.ID {
		return fmt.Errorf("agent certificate URI SAN must be %q", identity.AgentIdentityURI(cfg.Agent.ID))
	}
	if len(cfg.Token) < 32 {
		return errors.New("agent token must contain at least 32 bytes")
	}
	return nil
}

func Dial(ctx context.Context, cfg *config.AgentOptions, metrics ...ObfuscationMetrics) (Session, error) {
	if cfg == nil {
		return nil, errors.New("agent configuration is required")
	}
	caBytes, err := os.ReadFile(cfg.Agent.TLS.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read gateway CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("agent.tls.ca_file does not contain a certificate")
	}
	cert, err := tls.LoadX509KeyPair(cfg.Agent.TLS.CertFile, cfg.Agent.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load agent certificate: %w", err)
	}
	tlsCfg := &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   cfg.Agent.TLS.ServerName,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{cfg.Transport.ALPN},
	}
	remote, err := net.ResolveUDPAddr("udp", cfg.Agent.Server)
	if err != nil {
		return nil, fmt.Errorf("resolve gateway address: %w", err)
	}
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return nil, fmt.Errorf("open agent UDP socket: %w", err)
	}
	if err := configureUDPBuffers(udpConn, cfg.Transport); err != nil {
		_ = udpConn.Close()
		return nil, err
	}
	packetConn, err := NewObfuscatingPacketConn(udpConn, cfg.TransportObfuscation, metrics...)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("configure transport obfuscation: %w", err)
	}
	qt := &quic.Transport{Conn: packetConn}
	conn, err := qt.Dial(ctx, remote, tlsCfg, quicConfigWithObfuscation(cfg.Transport, cfg.TransportObfuscation))
	if err != nil {
		_ = closeTransport(qt, packetConn)
		return nil, fmt.Errorf("dial gateway: %w", err)
	}
	return &quicSession{conn: conn, closeFn: func() error { return closeTransport(qt, packetConn) }}, nil
}

func quicConfig(cfg config.TransportConfig) *quic.Config {
	return quicConfigWithObfuscation(cfg, config.TransportObfuscationOptions{Mode: config.TransportObfuscationStandard})
}

func quicConfigWithObfuscation(cfg config.TransportConfig, obfs config.TransportObfuscationOptions) *quic.Config {
	q := &quic.Config{
		MaxIncomingStreams:   cfg.MaxBidiRemoteStreams,
		HandshakeIdleTimeout: time.Duration(cfg.HandshakeTimeoutSec) * time.Second,
		MaxIdleTimeout:       time.Duration(cfg.IdleTimeoutSec) * time.Second,
		KeepAlivePeriod:      time.Duration(cfg.KeepAliveSec) * time.Second,
		Allow0RTT:            false,
	}
	if obfs.Mode == config.TransportObfuscationCamouflage {
		// The datagram wrapper changes packet sizes and cannot safely forward
		// QUIC's path-MTU probes through the outer shaping layer.
		q.DisablePathMTUDiscovery = true
	}
	if cfg.InitialStreamReceiveWindow > 0 {
		q.InitialStreamReceiveWindow = uint64(cfg.InitialStreamReceiveWindow)
		if cfg.MaxStreamReadBuffer == 0 {
			q.MaxStreamReceiveWindow = uint64(cfg.InitialStreamReceiveWindow)
		}
	}
	if cfg.InitialConnectionReceiveWindow > 0 {
		q.InitialConnectionReceiveWindow = uint64(cfg.InitialConnectionReceiveWindow)
		if cfg.MaxConnReadBuffer == 0 {
			q.MaxConnectionReceiveWindow = uint64(cfg.InitialConnectionReceiveWindow)
		}
	}
	if cfg.MaxStreamReadBuffer > 0 {
		q.MaxStreamReceiveWindow = uint64(cfg.MaxStreamReadBuffer)
	}
	if cfg.MaxConnReadBuffer > 0 {
		q.MaxConnectionReceiveWindow = uint64(cfg.MaxConnReadBuffer)
	}
	return q
}

// CertificateAgentID extracts the single AsterFerry URI identity. Other URI
// SANs are permitted for certificate tooling, but multiple AsterFerry
// identities are rejected to prevent ambiguous binding.
func CertificateAgentID(cert *x509.Certificate) (string, bool) {
	if cert == nil {
		return "", false
	}
	agentID := ""
	for _, uri := range cert.URIs {
		candidate, ok := identity.ParseAgentIdentityURI(uri)
		if !ok {
			continue
		}
		if agentID != "" {
			return "", false
		}
		agentID = candidate
	}
	return agentID, agentID != ""
}

func configureUDPBuffers(conn *net.UDPConn, cfg config.TransportConfig) error {
	if conn == nil {
		return errors.New("UDP socket is nil")
	}
	if cfg.UDPReadBufferBytes > 0 {
		if int64(int(cfg.UDPReadBufferBytes)) != cfg.UDPReadBufferBytes {
			return errors.New("transport.udp_read_buffer_bytes exceeds platform int size")
		}
		if err := conn.SetReadBuffer(int(cfg.UDPReadBufferBytes)); err != nil {
			return fmt.Errorf("set UDP read buffer: %w", err)
		}
	}
	if cfg.UDPWriteBufferBytes > 0 {
		if int64(int(cfg.UDPWriteBufferBytes)) != cfg.UDPWriteBufferBytes {
			return errors.New("transport.udp_write_buffer_bytes exceeds platform int size")
		}
		if err := conn.SetWriteBuffer(int(cfg.UDPWriteBufferBytes)); err != nil {
			return fmt.Errorf("set UDP write buffer: %w", err)
		}
	}
	return nil
}

func closeTransport(qt *quic.Transport, conn net.PacketConn) error {
	var first error
	if qt != nil {
		if err := qt.Close(); err != nil {
			first = err
		}
	}
	if conn != nil {
		if err := conn.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// SetStreamContext adapts context cancellation to quic-go's stream deadline
// and reset APIs. The returned function restores an unrestricted deadline and
// stops the cancellation callback.
func SetStreamContext(stream Stream, ctx context.Context) func() {
	if stream == nil {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	// Deadlines are sufficient for bounded handshakes. Avoid installing a
	// cancellation callback for them: canceling a completed handshake context
	// can race with the next stream phase and reset an otherwise healthy stream.
	stop := func() bool { return true }
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		stop = context.AfterFunc(ctx, func() {
			stream.Cancel()
		})
	}
	var once sync.Once
	streamDone := context.AfterFunc(stream.Context(), func() {
		once.Do(func() {
			stop()
			_ = stream.SetDeadline(time.Time{})
		})
	})
	return func() {
		once.Do(func() {
			stop()
			streamDone()
			_ = stream.SetDeadline(time.Time{})
		})
	}
}

func CloseSession(session Session) error {
	if session == nil {
		return nil
	}
	return session.Close()
}

func WaitConn(ctx context.Context, conn Session) error {
	if conn == nil {
		return errors.New("connection is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-conn.Context().Done():
		return conn.Context().Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
