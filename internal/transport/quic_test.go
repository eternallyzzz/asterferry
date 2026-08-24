package transport

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"testing"

	"asterferry/internal/config"
)

type statsSession struct{}

func (statsSession) OpenStream(context.Context) (Stream, error) {
	return nil, errors.New("not implemented")
}
func (statsSession) AcceptStream(context.Context) (Stream, error) {
	return nil, errors.New("not implemented")
}
func (statsSession) Context() context.Context              { return context.Background() }
func (statsSession) RemoteAddr() net.Addr                  { return nil }
func (statsSession) PeerCertificates() []*x509.Certificate { return nil }
func (statsSession) Close() error                          { return nil }
func (statsSession) Stats() ConnectionStats                { return ConnectionStats{BytesSent: 7} }

func TestTransportNilBoundariesAndStats(t *testing.T) {
	if _, ok := SessionStats(statsSession{}); !ok {
		t.Fatal("stats provider was not detected")
	}
	if _, ok := SessionStats(nil); ok {
		t.Fatal("nil session should not provide stats")
	}
	var listener *quicListener
	if _, err := listener.Accept(context.Background()); err == nil {
		t.Fatal("nil listener accepted a session")
	}
	if err := listener.StopAccepting(); err != nil || listener.Close() != nil {
		t.Fatal("nil listener methods should be harmless")
	}
	var session *quicSession
	if _, err := session.OpenStream(context.Background()); err == nil {
		t.Fatal("nil session opened a stream")
	}
	if _, err := session.AcceptStream(context.Background()); err == nil {
		t.Fatal("nil session accepted a stream")
	}
	if session.Context() == nil || session.Stats() != (ConnectionStats{}) || session.Close() != nil {
		t.Fatal("nil session boundary behavior is incorrect")
	}
	if _, err := Listen(nil); err == nil {
		t.Fatal("nil gateway config should fail")
	}
	if _, err := Dial(context.Background(), nil); err == nil {
		t.Fatal("nil agent config should fail")
	}
}

func TestCertificateAgentIdentityBinding(t *testing.T) {
	identity, err := url.Parse(AgentIdentityURI("edge-a"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := CertificateAgentID(&x509.Certificate{URIs: []*url.URL{identity}}); !ok || got != "edge-a" {
		t.Fatalf("certificate identity = %q, %v", got, ok)
	}
	other, _ := url.Parse(AgentIdentityURI("edge-b"))
	if _, ok := CertificateAgentID(&x509.Certificate{URIs: []*url.URL{identity, other}}); ok {
		t.Fatal("multiple AsterFerry identities should be rejected")
	}
	if _, ok := CertificateAgentID(&x509.Certificate{}); ok {
		t.Fatal("certificate without URI identity should be rejected")
	}
}

func TestQUICListenerAndSessionCloseCallbacks(t *testing.T) {
	var listenerCalls, sessionCalls int
	l := &quicListener{closeFn: func() error { listenerCalls++; return errors.New("listener close") }}
	if err := l.Close(); err == nil || listenerCalls != 1 {
		t.Fatalf("listener close = %v calls=%d", err, listenerCalls)
	}
	if err := l.Close(); err == nil || listenerCalls != 1 {
		t.Fatalf("listener close should be idempotent: %v calls=%d", err, listenerCalls)
	}
	s := &quicSession{closeFn: func() error { sessionCalls++; return errors.New("session close") }}
	if err := s.Close(); err == nil || sessionCalls != 1 {
		t.Fatalf("session close = %v calls=%d", err, sessionCalls)
	}
	if err := s.Close(); err == nil || sessionCalls != 1 {
		t.Fatalf("session close should be idempotent: %v calls=%d", err, sessionCalls)
	}
}

func TestTransportBufferConfigurationAndCloseAlias(t *testing.T) {
	if err := configureUDPBuffers(nil, config.TransportConfig{}); err == nil {
		t.Fatal("nil UDP socket should fail buffer configuration")
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := configureUDPBuffers(conn, config.TransportConfig{UDPReadBufferBytes: 64 << 10, UDPWriteBufferBytes: 64 << 10}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CloseConn(nil); err != nil {
		t.Fatal(err)
	}
	session := statsSession{}
	if err := CloseConn(session); err != nil {
		t.Fatal(err)
	}
}
