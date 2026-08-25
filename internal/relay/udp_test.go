package relay

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"asterferry/internal/transport"
)

type udpPumpStream struct {
	net.Conn
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func newUDPPumpStream(conn net.Conn) *udpPumpStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &udpPumpStream{Conn: conn, ctx: ctx, cancel: cancel}
}

func (s *udpPumpStream) Context() context.Context { return s.ctx }

func (s *udpPumpStream) Cancel() {
	s.once.Do(func() {
		s.cancel()
		_ = s.Conn.Close()
	})
}

func newPumpUDPConn(t *testing.T) (*net.UDPConn, *net.UDPConn) {
	t.Helper()
	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := net.DialUDP("udp", nil, target.LocalAddr().(*net.UDPAddr))
	if err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	return remote, target
}

func TestBidirectionalUDPRoundTrip(t *testing.T) {
	remote, target := newPumpUDPConn(t)
	defer remote.Close()
	defer target.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		buf := make([]byte, 512)
		n, addr, err := target.ReadFromUDP(buf)
		if err == nil {
			_, _ = target.WriteToUDP(buf[:n], addr)
		}
	}()
	server, peer := net.Pipe()
	stream := newUDPPumpStream(server)
	defer peer.Close()
	var in, out atomic.Uint64
	done := make(chan error, 1)
	go func() {
		done <- BidirectionalUDP(context.Background(), stream, remote, UDPPumpOptions{
			MaxFrameBytes: 4096, MaxUDPBytes: 512, MaxPaddingBytes: 64, Profile: "standard", IdleTimeout: time.Second,
			Counters: Counters{In: func(n uint64) { in.Add(n) }, Out: func(n uint64) { out.Add(n) }},
		})
	}()
	frame := transport.MustMessageFrame(transport.TypeData, 0, transport.NewData([]byte("ping"), "standard", 0))
	if err := transport.WriteFrame(peer, frame, 4096); err != nil {
		t.Fatal(err)
	}
	response, err := transport.ReadFrame(peer, 4096)
	if err != nil {
		t.Fatal(err)
	}
	data, err := transport.DecodeData(response, 512, 64)
	if err != nil || string(data.Payload) != "ping" {
		t.Fatalf("UDP response = %q, %v", data.Payload, err)
	}
	deadline := time.Now().Add(time.Second)
	for out.Load() != 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if in.Load() != 4 || out.Load() != 4 {
		t.Fatalf("UDP counters = %d/%d", in.Load(), out.Load())
	}
	_ = peer.Close()
	select {
	case <-echoDone:
	case <-time.After(time.Second):
		t.Fatal("UDP echo did not finish")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("UDP pump did not stop")
	}
}

func TestBidirectionalUDPRejectsBadFrameAndLimits(t *testing.T) {
	if err := BidirectionalUDP(context.Background(), nil, nil, UDPPumpOptions{}); err == nil {
		t.Fatal("nil UDP endpoints should fail")
	}
	remote, target := newPumpUDPConn(t)
	defer remote.Close()
	defer target.Close()
	server, peer := net.Pipe()
	stream := newUDPPumpStream(server)
	done := make(chan error, 1)
	go func() {
		done <- BidirectionalUDP(context.Background(), stream, remote, UDPPumpOptions{MaxUDPBytes: 512})
	}()
	bad := transport.MustMessageFrame(transport.TypePing, 1, nil)
	if err := transport.WriteFrame(peer, bad, transport.DefaultMaxFrame); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("bad UDP frame should fail")
		}
	case <-time.After(time.Second):
		t.Fatal("bad UDP frame did not stop the pump")
	}
	_ = peer.Close()
	if err := BidirectionalUDP(context.Background(), stream, remote, UDPPumpOptions{MaxUDPBytes: 0}); err == nil {
		t.Fatal("invalid UDP limit should fail")
	}
}
