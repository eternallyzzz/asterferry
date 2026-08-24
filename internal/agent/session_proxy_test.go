package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/transport"
)

type agentPipeStream struct {
	net.Conn
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func newAgentPipeStream(conn net.Conn, parent context.Context) *agentPipeStream {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &agentPipeStream{Conn: conn, ctx: ctx, cancel: cancel}
}

func (s *agentPipeStream) Context() context.Context { return s.ctx }

func (s *agentPipeStream) Cancel() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.cancel()
		_ = s.Conn.Close()
	})
}

type openProxyFakeSession struct {
	peers     chan net.Conn
	openErr   error
	streamFn  func(context.Context) (transport.Stream, error)
	ctx       context.Context
	closeOnce sync.Once
}

func (s *openProxyFakeSession) OpenStream(ctx context.Context) (transport.Stream, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	if s.streamFn != nil {
		return s.streamFn(ctx)
	}
	server, peer := net.Pipe()
	s.peers <- peer
	return newAgentPipeStream(server, ctx), nil
}

func (s *openProxyFakeSession) AcceptStream(context.Context) (transport.Stream, error) {
	return nil, errors.New("accept stream unavailable in test")
}

func (s *openProxyFakeSession) Context() context.Context { return s.ctx }

func (s *openProxyFakeSession) RemoteAddr() net.Addr { return nil }

func (s *openProxyFakeSession) PeerCertificates() []*x509.Certificate { return nil }

func (s *openProxyFakeSession) Close() error {
	s.closeOnce.Do(func() {})
	return nil
}

type openProxyResult struct {
	stream transport.Stream
	err    error
}

func newAgentSessionForTest(t *testing.T, caps ...transport.Capability) (*Agent, *Session, *openProxyFakeSession) {
	t.Helper()
	a := newProxyTestAgent(t)
	a.cfg.Transport.HandshakeTimeoutSec = 2
	a.sessions = newSessionManager(a.ctx, nil, a.logger)
	sessionCtx, cancel := context.WithCancel(a.ctx)
	fake := &openProxyFakeSession{peers: make(chan net.Conn, 1), ctx: sessionCtx}
	sess := &Session{
		agent:     a,
		conn:      fake,
		caps:      caps,
		limits:    transport.Limits{MaxFrameBytes: 4096, MaxRecordBytes: 1024, MaxUDPBytes: 4096, MaxStreams: 1},
		ctx:       sessionCtx,
		cancel:    cancel,
		streamSem: make(chan struct{}, 1),
	}
	if !a.sessions.set(sess) {
		t.Fatal("test session was not admitted")
	}
	t.Cleanup(func() {
		cancel()
		_ = a.sessions.Close()
	})
	return a, sess, fake
}

func readAgentFrame(t *testing.T, conn net.Conn) transport.Frame {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	f, err := transport.ReadFrame(conn, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func writeAgentFrame(t *testing.T, conn net.Conn, f transport.Frame) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := transport.WriteFrame(conn, f, 4096); err != nil {
		t.Fatal(err)
	}
}

func startOpenProxy(t *testing.T, a *Agent, fake *openProxyFakeSession) (<-chan openProxyResult, net.Conn) {
	t.Helper()
	result := make(chan openProxyResult, 1)
	go func() {
		stream, err := a.OpenProxy(context.Background(), "tcp", "127.0.0.1", 8080)
		result <- openProxyResult{stream: stream, err: err}
	}()
	select {
	case peer := <-fake.peers:
		return result, peer
	case <-time.After(2 * time.Second):
		t.Fatal("OpenProxy did not open a transport stream")
		return nil, nil
	}
}

func TestAgentOpenProxySuccessAndLeaseRelease(t *testing.T) {
	a, sess, fake := newAgentSessionForTest(t, transport.CapabilityEgressProxy)
	result, peer := startOpenProxy(t, a, fake)
	defer peer.Close()
	open := readAgentFrame(t, peer)
	if open.Type != transport.TypeOpenProxy || open.RequestID == 0 {
		t.Fatalf("OpenProxy request = %#v", open)
	}
	var payload transport.OpenProxy
	if err := transport.DecodeMessage(open, &payload); err != nil || payload.Network != "tcp" || payload.Address != "127.0.0.1" || payload.Port != 8080 {
		t.Fatalf("OpenProxy payload = %#v, err=%v", payload, err)
	}
	ok, err := transport.MessageFrame(transport.TypeOpenOK, 1, transport.OpenResult{})
	if err != nil {
		t.Fatal(err)
	}
	writeAgentFrame(t, peer, ok)
	opened := <-result
	if opened.err != nil || opened.stream == nil {
		t.Fatalf("OpenProxy result = %#v", opened)
	}
	if err := opened.stream.Close(); err != nil {
		t.Fatal(err)
	}
	if release, ok := sess.acquireStream(context.Background()); !ok {
		t.Fatal("leased stream did not release its session slot")
	} else {
		release()
	}
}

func TestAgentOpenProxyNegotiationFailuresReleaseCapacity(t *testing.T) {
	t.Run("missing capability", func(t *testing.T) {
		a, _, _ := newAgentSessionForTest(t)
		if _, err := a.OpenProxy(context.Background(), "tcp", "127.0.0.1", 80); err == nil {
			t.Fatal("missing egress capability should fail")
		}
	})

	t.Run("stream limit", func(t *testing.T) {
		a, sess, _ := newAgentSessionForTest(t, transport.CapabilityEgressProxy)
		sess.streamSem <- struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, err := a.OpenProxy(ctx, "tcp", "127.0.0.1", 80); err == nil {
			t.Fatal("full stream budget should fail")
		}
	})

	t.Run("open failure", func(t *testing.T) {
		a, sess, fake := newAgentSessionForTest(t, transport.CapabilityEgressProxy)
		fake.openErr = errors.New("transport open failed")
		if _, err := a.OpenProxy(context.Background(), "tcp", "127.0.0.1", 80); err == nil {
			t.Fatal("transport open failure should be returned")
		}
		if release, ok := sess.acquireStream(context.Background()); !ok {
			t.Fatal("failed OpenProxy did not release capacity")
		} else {
			release()
		}
	})

	t.Run("write failure", func(t *testing.T) {
		a, sess, fake := newAgentSessionForTest(t, transport.CapabilityEgressProxy)
		fake.streamFn = func(context.Context) (transport.Stream, error) {
			return failingAgentStream{}, nil
		}
		if _, err := a.OpenProxy(context.Background(), "tcp", "127.0.0.1", 80); err == nil {
			t.Fatal("stream write failure should be returned")
		}
		if release, ok := sess.acquireStream(context.Background()); !ok {
			t.Fatal("write failure did not release capacity")
		} else {
			release()
		}
	})

	for _, tc := range []struct {
		name string
		resp func() transport.Frame
	}{
		{name: "open error", resp: func() transport.Frame {
			f, _ := transport.MessageFrame(transport.TypeOpenError, 1, transport.OpenResult{Error: transport.NewProtocolError(transport.ErrorPolicyDenied, "denied", false)})
			return f
		}},
		{name: "protocol error", resp: func() transport.Frame {
			f, _ := transport.MessageFrame(transport.TypeError, 1, transport.NewProtocolError(transport.ErrorInternal, "broken", true))
			return f
		}},
		{name: "unexpected response", resp: func() transport.Frame {
			f, _ := transport.MessageFrame(transport.TypePong, 1, nil)
			return f
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, sess, fake := newAgentSessionForTest(t, transport.CapabilityEgressProxy)
			result, peer := startOpenProxy(t, a, fake)
			defer peer.Close()
			_ = readAgentFrame(t, peer)
			writeAgentFrame(t, peer, tc.resp())
			opened := <-result
			if opened.err == nil {
				t.Fatal("failed OpenProxy response should return an error")
			}
			if release, ok := sess.acquireStream(context.Background()); !ok {
				t.Fatal("failed handshake did not release capacity")
			} else {
				release()
			}
		})
	}

	t.Run("read failure", func(t *testing.T) {
		a, sess, fake := newAgentSessionForTest(t, transport.CapabilityEgressProxy)
		result, peer := startOpenProxy(t, a, fake)
		_ = readAgentFrame(t, peer)
		_ = peer.Close()
		opened := <-result
		if opened.err == nil {
			t.Fatal("closed handshake stream should fail")
		}
		if release, ok := sess.acquireStream(context.Background()); !ok {
			t.Fatal("read failure did not release capacity")
		} else {
			release()
		}
	})
}

type failingAgentStream struct{}

func (failingAgentStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (failingAgentStream) Write([]byte) (int, error)   { return 0, errors.New("write failed") }
func (failingAgentStream) Close() error                { return nil }
func (failingAgentStream) SetDeadline(time.Time) error { return nil }
func (failingAgentStream) Context() context.Context    { return context.Background() }
func (failingAgentStream) Cancel()                     {}

func newReverseSession(t *testing.T) (*Agent, *Session) {
	t.Helper()
	a := newProxyTestAgent(t)
	a.cfg.Transport.HandshakeTimeoutSec = 2
	a.cfg.Obfuscation.ReverseProfile = config.ProfileStandard
	ctx, cancel := context.WithCancel(a.ctx)
	sess := &Session{agent: a, ctx: ctx, cancel: cancel, limits: transport.Limits{MaxFrameBytes: 4096, MaxRecordBytes: 1024, MaxUDPBytes: 4096, MaxStreams: 1}}
	t.Cleanup(cancel)
	return a, sess
}

func TestAgentReverseUDPRoundTripAndBadFrame(t *testing.T) {
	a, sess := newReverseSession(t)
	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		buf := make([]byte, 256)
		n, addr, readErr := target.ReadFromUDP(buf)
		if readErr == nil {
			_, _ = target.WriteToUDP(buf[:n], addr)
		}
	}()

	streamConn, peer := net.Pipe()
	stream := newAgentPipeStream(streamConn, sess.ctx)
	defer peer.Close()
	done := make(chan struct{})
	go func() {
		sess.reverseUDP(stream, config.Tunnel{Name: "udp", Protocol: "udp", Local: target.LocalAddr().String()}, 1)
		close(done)
	}()
	open := readAgentFrame(t, peer)
	if open.Type != transport.TypeOpenOK {
		t.Fatalf("reverse UDP open response = %#v", open)
	}
	data, _ := transport.MessageFrame(transport.TypeData, 0, transport.NewData([]byte("ping"), config.ProfileStandard, sess.maxPadding()))
	writeAgentFrame(t, peer, data)
	response := readAgentFrame(t, peer)
	decoded, err := transport.DecodeData(response, sess.maxUDP(), sess.maxPadding())
	if err != nil || string(decoded.Payload) != "ping" {
		t.Fatalf("reverse UDP payload = %q, err=%v", decoded.Payload, err)
	}
	bad := transport.Frame{Type: transport.TypeData, Payload: []byte{0xff}}
	writeAgentFrame(t, peer, bad)
	_ = peer.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reverse UDP did not finish")
	}
	_ = a
}

func TestAgentReverseUDPRejectsInvalidDestination(t *testing.T) {
	_, sess := newReverseSession(t)
	streamConn, peer := net.Pipe()
	stream := newAgentPipeStream(streamConn, sess.ctx)
	defer peer.Close()
	done := make(chan struct{})
	go func() {
		sess.reverseUDP(stream, config.Tunnel{Protocol: "udp", Local: "127.0.0.1:not-a-port"}, 1)
		close(done)
	}()
	f := readAgentFrame(t, peer)
	if f.Type != transport.TypeOpenError {
		t.Fatalf("invalid reverse UDP response = %#v", f)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("invalid reverse UDP did not finish")
	}
}

func TestAgentHandleReverseRejectsCapabilityAndMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []transport.Capability
		maps map[string]config.Tunnel
		code transport.ErrorCode
	}{
		{name: "capability", maps: map[string]config.Tunnel{"udp": {Name: "udp", Protocol: "udp", Local: "127.0.0.1:1"}}, code: transport.ErrorCapabilityMismatch},
		{name: "mapping", caps: []transport.Capability{transport.CapabilityReverseUDP}, maps: map[string]config.Tunnel{}, code: transport.ErrorMappingRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, sess := newReverseSession(t)
			sess.caps = tc.caps
			a.mappings = tc.maps
			streamConn, peer := net.Pipe()
			stream := newAgentPipeStream(streamConn, sess.ctx)
			defer peer.Close()
			released := make(chan struct{})
			done := make(chan struct{})
			go func() {
				sess.handleReverse(stream, func() { close(released) })
				close(done)
			}()
			open, _ := transport.MessageFrame(transport.TypeOpenReverse, 1, transport.OpenReverse{Name: "udp", Protocol: "udp", Profile: config.ProfileStandard})
			writeAgentFrame(t, peer, open)
			response := readAgentFrame(t, peer)
			if response.Type != transport.TypeOpenError {
				t.Fatalf("reverse rejection response = %#v", response)
			}
			var result transport.OpenResult
			if err := transport.DecodeMessage(response, &result); err != nil || result.Error == nil || result.Error.Code != tc.code {
				t.Fatalf("reverse rejection error = %#v, err=%v", result.Error, err)
			}
			select {
			case <-released:
			case <-time.After(time.Second):
				t.Fatal("reverse stream release was not called")
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("reverse handler did not finish")
			}
		})
	}
}

func TestAgentSessionFallbackHelpers(t *testing.T) {
	a, sess, _ := newAgentSessionForTest(t, transport.CapabilityEgressProxy)
	if a.currentSessionID() != sess.sessionID {
		t.Fatalf("current session id = %q, want %q", a.currentSessionID(), sess.sessionID)
	}
	var nilSession *Session
	if nilSession.maxFrame() != transport.DefaultMaxFrame || nilSession.maxRecord() != 0 || nilSession.maxUDP() != 0 || nilSession.maxPadding() != 0 || nilSession.relayProfile("standard").Name != "" {
		t.Fatal("nil session helper fallback changed")
	}
}
