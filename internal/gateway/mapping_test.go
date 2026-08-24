package gateway

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/transport"
)

type mappingFakeSession struct {
	peers    chan net.Conn
	openErr  error
	streamFn func(context.Context) (transport.Stream, error)
	ctx      context.Context
	mu       sync.Mutex
	streams  []transport.Stream
}

func (s *mappingFakeSession) OpenStream(ctx context.Context) (transport.Stream, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	if s.streamFn != nil {
		return s.streamFn(ctx)
	}
	server, peer := net.Pipe()
	stream := newGatewayPipeStream(server)
	s.mu.Lock()
	s.streams = append(s.streams, stream)
	s.mu.Unlock()
	select {
	case s.peers <- peer:
		return stream, nil
	case <-ctx.Done():
		_ = peer.Close()
		_ = stream.Close()
		return nil, ctx.Err()
	}
}

func (s *mappingFakeSession) AcceptStream(context.Context) (transport.Stream, error) {
	return nil, errors.New("accept stream unavailable in test")
}

func (s *mappingFakeSession) Context() context.Context { return s.ctx }

func (s *mappingFakeSession) RemoteAddr() net.Addr { return nil }

func (s *mappingFakeSession) PeerCertificates() []*x509.Certificate { return nil }

func (s *mappingFakeSession) Close() error {
	s.mu.Lock()
	streams := append([]transport.Stream(nil), s.streams...)
	s.mu.Unlock()
	for _, stream := range streams {
		_ = stream.Close()
	}
	return nil
}

func newTestUDPMapping(t *testing.T) (*Gateway, *Session, *mappingFakeSession, *Mapping, *net.UDPConn, *net.UDPAddr) {
	t.Helper()
	g := configuredGatewayForHelpers()
	sess := gatewayHelperSession(g, transport.CapabilityReverseUDP)
	fake := &mappingFakeSession{peers: make(chan net.Conn, 4), ctx: sess.ctx}
	sess.conn = fake
	spec := transport.TunnelRegistration{Name: "dns", Protocol: "udp", GatewayPort: 0, Profile: config.ProfileStandard}
	m, err := newMapping(g, sess, spec)
	if err != nil {
		t.Fatal(err)
	}
	local := m.udp.LocalAddr().(*net.UDPAddr)
	network := "udp6"
	loopback := net.IPv6loopback
	if local.IP.To4() != nil {
		network = "udp4"
		loopback = net.IPv4(127, 0, 0, 1)
	}
	client, err := net.ListenUDP(network, &net.UDPAddr{IP: loopback})
	if err != nil {
		_ = m.Close()
		t.Fatal(err)
	}
	target := &net.UDPAddr{IP: loopback, Port: local.Port}
	t.Cleanup(func() {
		_ = m.Close()
		_ = client.Close()
		sess.cancel()
		_ = fake.Close()
	})
	return g, sess, fake, m, client, target
}

func runTestMapping(t *testing.T, m *Mapping) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		m.Run()
		close(done)
	}()
	return done
}

func readMappingFrame(t *testing.T, peer net.Conn) transport.Frame {
	t.Helper()
	_ = peer.SetDeadline(time.Now().Add(2 * time.Second))
	f, err := transport.ReadFrame(peer, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func writeMappingFrame(t *testing.T, peer net.Conn, frame transport.Frame) {
	t.Helper()
	_ = peer.SetDeadline(time.Now().Add(2 * time.Second))
	if err := transport.WriteFrame(peer, frame, 4096); err != nil {
		t.Fatal(err)
	}
}

func waitMappingDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("UDP mapping did not stop")
	}
}

func waitMappingPeer(t *testing.T, fake *mappingFakeSession) net.Conn {
	t.Helper()
	select {
	case peer := <-fake.peers:
		return peer
	case <-time.After(2 * time.Second):
		t.Fatal("UDP mapping did not open a reverse stream")
		return nil
	}
}

func sendMappingUDP(t *testing.T, client *net.UDPConn, target *net.UDPAddr, payload string) {
	t.Helper()
	if _, err := client.WriteToUDP([]byte(payload), target); err != nil {
		t.Fatal(err)
	}
}

func expectNoMappingPeer(t *testing.T, fake *mappingFakeSession) {
	t.Helper()
	select {
	case peer := <-fake.peers:
		_ = peer.Close()
		t.Fatal("mapping opened a stream unexpectedly")
	case <-time.After(450 * time.Millisecond):
	}
}

func openMappingAssociation(t *testing.T, fake *mappingFakeSession) net.Conn {
	t.Helper()
	peer := waitMappingPeer(t, fake)
	open := readMappingFrame(t, peer)
	if open.Type != transport.TypeOpenReverse {
		t.Fatalf("reverse open frame = %#v", open)
	}
	var request transport.OpenReverse
	if err := transport.DecodeMessage(open, &request); err != nil || request.Name != "dns" || request.Protocol != "udp" || request.Profile != config.ProfileStandard {
		t.Fatalf("reverse open payload = %#v, err=%v", request, err)
	}
	ok, err := transport.MessageFrame(transport.TypeOpenOK, 1, transport.OpenResult{})
	if err != nil {
		t.Fatal(err)
	}
	writeMappingFrame(t, peer, ok)
	return peer
}

func TestMappingRunUDPRoundTripAndAssociationReuse(t *testing.T) {
	g, _, fake, m, client, target := newTestUDPMapping(t)
	done := runTestMapping(t, m)

	sendMappingUDP(t, client, target, "ping")
	peer := openMappingAssociation(t, fake)
	defer peer.Close()
	data := readMappingFrame(t, peer)
	decoded, err := transport.DecodeData(data, 512, 64)
	if err != nil || string(decoded.Payload) != "ping" {
		t.Fatalf("reverse UDP request = %q, err=%v", decoded.Payload, err)
	}
	response, err := transport.MessageFrame(transport.TypeData, 0, transport.NewData([]byte("pong"), config.ProfileStandard, 64))
	if err != nil {
		t.Fatal(err)
	}
	writeMappingFrame(t, peer, response)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "pong" {
		t.Fatalf("gateway UDP response = %q, err=%v", buf[:n], err)
	}

	sendMappingUDP(t, client, target, "again")
	reused := readMappingFrame(t, peer)
	if reused.Type != transport.TypeData {
		t.Fatalf("reused association frame = %#v", reused)
	}
	decoded, err = transport.DecodeData(reused, 512, 64)
	if err != nil || string(decoded.Payload) != "again" {
		t.Fatalf("reused association payload = %q, err=%v", decoded.Payload, err)
	}
	if g.metrics.BytesOut.Load() < 4 {
		t.Fatalf("mapping outbound bytes = %d", g.metrics.BytesOut.Load())
	}

	_ = m.Close()
	waitMappingDone(t, done)
}

func TestMappingRunUDPRejectsNewAssociations(t *testing.T) {
	t.Run("draining", func(t *testing.T) {
		_, _, fake, m, client, target := newTestUDPMapping(t)
		done := runTestMapping(t, m)
		m.BeginDrain()
		sendMappingUDP(t, client, target, "ignored")
		expectNoMappingPeer(t, fake)
		_ = m.Close()
		waitMappingDone(t, done)
	})

	t.Run("connection limit", func(t *testing.T) {
		_, sess, fake, m, client, target := newTestUDPMapping(t)
		sess.connSem <- struct{}{}
		done := runTestMapping(t, m)
		sendMappingUDP(t, client, target, "limited")
		expectNoMappingPeer(t, fake)
		<-sess.connSem
		_ = m.Close()
		waitMappingDone(t, done)
	})

	t.Run("open failure", func(t *testing.T) {
		_, _, fake, m, client, target := newTestUDPMapping(t)
		fake.openErr = errors.New("open failed")
		done := runTestMapping(t, m)
		sendMappingUDP(t, client, target, "failed")
		expectNoMappingPeer(t, fake)
		_ = m.Close()
		waitMappingDone(t, done)
	})
}

func TestMappingRunUDPRejectsReverseHandshake(t *testing.T) {
	for _, response := range []struct {
		name  string
		frame func() transport.Frame
	}{
		{name: "open error", frame: func() transport.Frame {
			f, _ := transport.MessageFrame(transport.TypeOpenError, 1, transport.OpenResult{Error: transport.NewProtocolError(transport.ErrorPolicyDenied, "denied", false)})
			return f
		}},
		{name: "open ok with error", frame: func() transport.Frame {
			f, _ := transport.MessageFrame(transport.TypeOpenOK, 1, transport.OpenResult{Error: transport.NewProtocolError(transport.ErrorInternal, "broken", true)})
			return f
		}},
	} {
		t.Run(response.name, func(t *testing.T) {
			_, _, fake, m, client, target := newTestUDPMapping(t)
			done := runTestMapping(t, m)
			sendMappingUDP(t, client, target, "handshake")
			peer := waitMappingPeer(t, fake)
			_ = readMappingFrame(t, peer)
			writeMappingFrame(t, peer, response.frame())
			_ = peer.Close()
			expectNoMappingPeer(t, fake)
			_ = m.Close()
			waitMappingDone(t, done)
		})
	}
}

func TestMappingRunUDPCleansUpFailedAndIdleAssociations(t *testing.T) {
	t.Run("bad frame", func(t *testing.T) {
		_, sess, fake, m, client, target := newTestUDPMapping(t)
		done := runTestMapping(t, m)
		sendMappingUDP(t, client, target, "first")
		peer := openMappingAssociation(t, fake)
		_ = readMappingFrame(t, peer)
		writeMappingFrame(t, peer, transport.Frame{Type: transport.TypeData, Payload: []byte{0xff}})
		_ = peer.Close()
		waitForMappingConnectionSlot(t, sess)

		sendMappingUDP(t, client, target, "second")
		peer = openMappingAssociation(t, fake)
		defer peer.Close()
		if got := readMappingFrame(t, peer).Type; got != transport.TypeData {
			t.Fatalf("new association frame type = %v", got)
		}
		_ = m.Close()
		waitMappingDone(t, done)
	})

	t.Run("idle timeout", func(t *testing.T) {
		_, sess, fake, m, client, target := newTestUDPMapping(t)
		done := runTestMapping(t, m)
		sendMappingUDP(t, client, target, "idle")
		peer := openMappingAssociation(t, fake)
		_ = readMappingFrame(t, peer)
		_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
		waitForMappingConnectionSlot(t, sess)
		_ = peer.Close()

		sendMappingUDP(t, client, target, "fresh")
		peer = openMappingAssociation(t, fake)
		defer peer.Close()
		if got := readMappingFrame(t, peer).Type; got != transport.TypeData {
			t.Fatalf("idle replacement frame type = %v", got)
		}
		_ = m.Close()
		waitMappingDone(t, done)
	})
}

func waitForMappingConnectionSlot(t *testing.T, sess *Session) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if release, ok := sess.acquireConnection(); ok {
			release()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("mapping connection slot was not released")
}

func TestSessionRegistryCloseAllClosesSessions(t *testing.T) {
	g := configuredGatewayForHelpers()
	registry := newSessionRegistry(g.nodeID, g.owners)
	g.sessions = registry
	one := gatewayHelperSession(g)
	two := gatewayHelperSession(g)
	one.agentID, one.sessionID = "one", "session-one"
	two.agentID, two.sessionID = "two", "session-two"
	if _, ok := registry.Add(one, 4); !ok {
		t.Fatal("failed to add first session")
	}
	if _, ok := registry.Add(two, 4); !ok {
		t.Fatal("failed to add second session")
	}
	registry.CloseAll()
	if registry.Count() != 0 {
		t.Fatalf("session registry count = %d", registry.Count())
	}
	for _, sess := range []*Session{one, two} {
		select {
		case <-sess.ctx.Done():
		default:
			t.Fatalf("session %s was not canceled", sess.agentID)
		}
	}
}
