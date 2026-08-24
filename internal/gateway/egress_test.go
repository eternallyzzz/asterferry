package gateway

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/transport"
)

type gatewayPipeStream struct {
	net.Conn
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func newGatewayPipeStream(conn net.Conn) *gatewayPipeStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &gatewayPipeStream{Conn: conn, ctx: ctx, cancel: cancel}
}

func (s *gatewayPipeStream) Context() context.Context { return s.ctx }

func (s *gatewayPipeStream) Cancel() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.cancel()
		_ = s.Conn.Close()
	})
}

func waitGatewayCall(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway UDP proxy did not finish")
	}
}

func TestGatewayEgressUDPProxyRoundTrip(t *testing.T) {
	g := configuredGatewayForHelpers()
	g.egress = newEgressProxy(g)
	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		buf := make([]byte, 256)
		n, addr, readErr := target.ReadFromUDP(buf)
		if readErr == nil {
			_, _ = target.WriteToUDP(buf[:n], addr)
		}
	}()

	streamConn, peer := net.Pipe()
	stream := newGatewayPipeStream(streamConn)
	defer peer.Close()
	defer stream.Close()
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	sess := gatewayHelperSession(g, transport.CapabilityEgressProxy)
	defer sess.cancel()
	done := make(chan struct{})
	go func() {
		g.egress.UDP(sess, stream, target.LocalAddr().String(), 9, config.ProfileStandard)
		close(done)
	}()

	open, err := transport.ReadFrame(peer, sess.maxFrame())
	if err != nil || open.Type != transport.TypeOpenOK || open.RequestID != 9 {
		t.Fatalf("UDP open response = %#v, err=%v", open, err)
	}
	dataFrame, err := transport.MessageFrame(transport.TypeData, 0, transport.NewData([]byte("ping"), config.ProfileStandard, sess.maxPadding()))
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteFrame(peer, dataFrame, sess.maxFrame()); err != nil {
		t.Fatal(err)
	}
	response, err := transport.ReadFrame(peer, sess.maxFrame())
	if err != nil || response.Type != transport.TypeData {
		t.Fatalf("UDP data response = %#v, err=%v", response, err)
	}
	data, err := transport.DecodeData(response, sess.maxUDP(), sess.maxPadding())
	if err != nil || string(data.Payload) != "ping" {
		t.Fatalf("UDP response payload = %q, err=%v", data.Payload, err)
	}
	if got := g.metrics.BytesIn.Load(); got != 4 {
		t.Fatalf("UDP inbound bytes = %d, want 4", got)
	}
	deadline := time.Now().Add(time.Second)
	for g.metrics.BytesOut.Load() != 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := g.metrics.BytesOut.Load(); got != 4 {
		t.Fatalf("UDP outbound bytes = %d, want 4", got)
	}
	_ = peer.Close()
	waitGatewayCall(t, done)
	waitGatewayCall(t, echoDone)
}

func TestGatewayEgressUDPProxyRejectsInvalidTarget(t *testing.T) {
	g := configuredGatewayForHelpers()
	g.egress = newEgressProxy(g)
	streamConn, peer := net.Pipe()
	stream := newGatewayPipeStream(streamConn)
	defer peer.Close()
	defer stream.Close()
	_ = peer.SetDeadline(time.Now().Add(2 * time.Second))
	sess := gatewayHelperSession(g, transport.CapabilityEgressProxy)
	defer sess.cancel()
	done := make(chan struct{})
	go func() {
		g.egress.UDP(sess, stream, "127.0.0.1:not-a-port:1", 12, config.ProfileStandard)
		close(done)
	}()
	f, err := transport.ReadFrame(peer, sess.maxFrame())
	if err != nil || f.Type != transport.TypeOpenError || f.RequestID != 12 {
		t.Fatalf("invalid UDP target response = %#v, err=%v", f, err)
	}
	waitGatewayCall(t, done)
}

func TestGatewayEgressUDPProxyRejectsBadDataFrame(t *testing.T) {
	g := configuredGatewayForHelpers()
	g.egress = newEgressProxy(g)
	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	streamConn, peer := net.Pipe()
	stream := newGatewayPipeStream(streamConn)
	defer peer.Close()
	defer stream.Close()
	_ = peer.SetDeadline(time.Now().Add(2 * time.Second))
	sess := gatewayHelperSession(g, transport.CapabilityEgressProxy)
	defer sess.cancel()
	done := make(chan struct{})
	go func() {
		g.egress.UDP(sess, stream, target.LocalAddr().String(), 13, config.ProfileStandard)
		close(done)
	}()
	if _, err := transport.ReadFrame(peer, sess.maxFrame()); err != nil {
		t.Fatal(err)
	}
	bad := transport.Frame{Type: transport.TypeData, Payload: []byte{0xff}}
	if err := transport.WriteFrame(peer, bad, sess.maxFrame()); err != nil {
		t.Fatal(err)
	}
	waitGatewayCall(t, done)
}

type failingGatewayStream struct{}

func (failingGatewayStream) Read([]byte) (int, error)    { return 0, errors.New("read failed") }
func (failingGatewayStream) Write([]byte) (int, error)   { return 0, errors.New("write failed") }
func (failingGatewayStream) Close() error                { return nil }
func (failingGatewayStream) SetDeadline(time.Time) error { return nil }
func (failingGatewayStream) Context() context.Context    { return context.Background() }
func (failingGatewayStream) Cancel()                     {}

func TestGatewayEgressUDPProxyOpenWriteFailure(t *testing.T) {
	g := configuredGatewayForHelpers()
	g.egress = newEgressProxy(g)
	sess := gatewayHelperSession(g, transport.CapabilityEgressProxy)
	defer sess.cancel()
	g.egress.UDP(sess, failingGatewayStream{}, "127.0.0.1:1", 14, config.ProfileStandard)
}
