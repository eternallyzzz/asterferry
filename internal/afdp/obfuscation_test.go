package afdp

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestObfuscatingPacketConnRoundTripAndTamperRejection(t *testing.T) {
	left, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	right, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		left.Close()
		t.Fatal(err)
	}
	key := []byte("controller-issued-obfuscation-key-material")
	options := ObfuscationOptions{Mode: ObfuscationCamouflage, CurrentKey: key, HandshakeShaping: true, MinFragmentBytes: 64, MaxFragmentBytes: 128, MaxWirePacketBytes: 256}
	l, err := NewObfuscatingPacketConn(left, options)
	if err != nil {
		right.Close()
		t.Fatal(err)
	}
	r, err := NewObfuscatingPacketConn(right, options)
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	defer l.Close()
	defer r.Close()
	message := bytes.Repeat([]byte("AFDP"), 80)
	if _, err := l.WriteTo(message, right.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(message)+32)
	n, _, err := r.ReadFrom(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:n], message) {
		t.Fatalf("reassembled payload mismatch: got %d bytes", n)
	}

	// A handshake-shaped packet that fits in one fragment must use the normal
	// authenticated data frame; advertising two fragments for it would leave
	// the receiver waiting forever for a fragment that is never sent.
	small := []byte{0x80}
	if _, err := l.WriteTo(small, right.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	got = make([]byte, len(small)+1)
	n, _, err = r.ReadFrom(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:n], small) {
		t.Fatalf("single-fragment payload mismatch: got %d bytes", n)
	}

	// A packet with a modified authenticated tail must be discarded rather
	// than returned to quic-go. Use the underlying socket to inject a validly
	// shaped but unauthenticated datagram.
	bad := bytes.Repeat([]byte{0x7f}, 32)
	if _, err := left.WriteToUDP(bad, right.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	_ = r.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	if n, _, err := r.ReadFrom(make([]byte, 64)); err == nil || n != 0 {
		t.Fatalf("tampered packet was accepted: n=%d err=%v", n, err)
	}
}
