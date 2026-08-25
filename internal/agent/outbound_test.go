package agent

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"asterferry/internal/proxy"
)

func TestAgentOutboundValidationAndDirectStream(t *testing.T) {
	var unavailable agentOutbound
	if _, err := unavailable.OpenStream(context.Background(), proxy.Target{}, proxy.PathDirect); err == nil {
		t.Fatal("nil agent outbound should fail")
	}
	a := testAgentRuntime()
	a.cfg.Limits.DialTimeoutSec = 1
	outbound := agentOutbound{agent: a}
	if _, err := outbound.OpenStream(context.Background(), proxy.Target{}, proxy.Path("invalid")); err == nil {
		t.Fatal("invalid proxy path should fail")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	target := proxy.Target{Network: "tcp", Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}
	stream, err := outbound.OpenStream(context.Background(), target, proxy.PathDirect)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Write([]byte("direct")); err != nil {
		t.Fatal(err)
	}
	if err := stream.(net.Conn).SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("direct"))
	if _, err := io.ReadFull(stream, got); err != nil || string(got) != "direct" {
		t.Fatalf("direct echo = %q, err=%v", got, err)
	}
}

func TestAgentOutboundDirectDatagram(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		buf := make([]byte, 64)
		n, addr, readErr := udp.ReadFromUDP(buf)
		if readErr == nil {
			_, _ = udp.WriteToUDP(buf[:n], addr)
		}
	}()
	a := testAgentRuntime()
	a.cfg.Limits.DialTimeoutSec = 1
	outbound := agentOutbound{agent: a}
	target := proxy.Target{Network: "udp", Host: "127.0.0.1", Port: uint16(udp.LocalAddr().(*net.UDPAddr).Port)}
	datagram, err := outbound.OpenDatagram(context.Background(), target, proxy.PathDirect)
	if err != nil {
		t.Fatal(err)
	}
	defer datagram.Close()
	if _, err := datagram.Write([]byte("udp")); err != nil {
		t.Fatal(err)
	}
	if err := datagram.(net.Conn).SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 16)
	n, err := datagram.Read(got)
	if err != nil || string(got[:n]) != "udp" {
		t.Fatalf("direct UDP echo = %q, err=%v", got[:n], err)
	}
}

func TestAgentOutboundDirectStreamFallsBackAcrossResolvedCandidates(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	a := testAgentRuntime()
	a.cfg.Limits.DialTimeoutSec = 1
	outbound := agentOutbound{agent: a}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	target := proxy.Target{Network: "tcp", Host: "example.invalid", Port: port, ResolvedIPs: []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("127.0.0.1")}}
	stream, err := outbound.OpenStream(context.Background(), target, proxy.PathDirect)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Write([]byte("fallback")); err != nil {
		t.Fatal(err)
	}
	_ = stream.(net.Conn).SetDeadline(time.Now().Add(time.Second))
	got := make([]byte, len("fallback"))
	if _, err := io.ReadFull(stream, got); err != nil || string(got) != "fallback" {
		t.Fatalf("fallback echo = %q, err=%v", got, err)
	}
}
