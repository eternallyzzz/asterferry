package transport

import (
	"bytes"
	"crypto/sha256"
	"net"
	"testing"
	"time"

	"asterferry/internal/config"
)

func TestObfuscatingPacketConnRoundTrip(t *testing.T) {
	serverUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer serverUDP.Close()
	clientUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer clientUDP.Close()
	server, err := NewObfuscatingPacketConn(serverUDP, testObfuscationOptions([]byte("current-obfuscation-key-0123456789"), nil))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewObfuscatingPacketConn(clientUDP, testObfuscationOptions([]byte("current-obfuscation-key-0123456789"), nil))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	defer client.Close()

	payload := bytes.Repeat([]byte{0x42}, 1200)
	result := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 2048)
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		n, _, readErr := server.ReadFrom(buf)
		if readErr != nil {
			errCh <- readErr
			return
		}
		result <- append([]byte(nil), buf[:n]...)
	}()
	if _, err := client.WriteTo(payload, server.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	case got := <-result:
		if !bytes.Equal(got, payload) {
			t.Fatalf("payload mismatch: got %d bytes", len(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for obfuscated packet")
	}
}

func FuzzObfuscationDecodeDoesNotPanic(f *testing.F) {
	key := []byte("fuzz-obfuscation-key-01234567890123456789")
	digest := sha256.Sum256(key)
	f.Add([]byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, wire []byte) {
		conn := &obfuscationPacketConn{
			keys:    []obfuscationKey{{key: digest}},
			parts:   make(map[fragmentKey]*fragmentAssembly),
			sources: make(map[string]int),
			opts:    testObfuscationOptions(key, nil),
		}
		dst := make([]byte, maxObfuscationDatagram)
		_, _ = conn.decode(wire, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4433}, dst)
	})
}

func TestObfuscatingPacketConnHandshakeFragments(t *testing.T) {
	serverUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer serverUDP.Close()
	clientUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer clientUDP.Close()
	opts := testObfuscationOptions([]byte("current-obfuscation-key-0123456789"), nil)
	server, err := NewObfuscatingPacketConn(serverUDP, opts)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewObfuscatingPacketConn(clientUDP, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	defer client.Close()

	payload := bytes.Repeat([]byte{0x80}, 1200)
	result := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 2048)
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		n, _, readErr := server.ReadFrom(buf)
		if readErr != nil {
			errCh <- readErr
			return
		}
		result <- append([]byte(nil), buf[:n]...)
	}()
	if _, err := client.WriteTo(payload, server.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	case got := <-result:
		if !bytes.Equal(got, payload) {
			t.Fatalf("fragmented payload mismatch: got %d bytes", len(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fragmented packet")
	}
}

func TestObfuscatingPacketConnOversizedShortHeader(t *testing.T) {
	serverUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer serverUDP.Close()
	clientUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer clientUDP.Close()
	opts := testObfuscationOptions([]byte("current-obfuscation-key-0123456789"), nil)
	opts.HandshakeShaping = false
	server, err := NewObfuscatingPacketConn(serverUDP, opts)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewObfuscatingPacketConn(clientUDP, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	defer client.Close()

	payload := bytes.Repeat([]byte{0x40}, 1400)
	payload[0] = 0x41 // QUIC short-header form; this is not a handshake packet.
	result := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 2048)
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		n, _, readErr := server.ReadFrom(buf)
		if readErr != nil {
			errCh <- readErr
			return
		}
		result <- append([]byte(nil), buf[:n]...)
	}()
	if _, err := client.WriteTo(payload, server.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	case got := <-result:
		if !bytes.Equal(got, payload) {
			t.Fatalf("oversized short-header payload mismatch: got %d bytes", len(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for oversized short-header packet")
	}
}

func TestObfuscatingPacketConnPreviousKeyAndWrongKey(t *testing.T) {
	serverUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer serverUDP.Close()
	clientUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer clientUDP.Close()
	previous := []byte("previous-obfuscation-key-0123456789")
	current := []byte("current-obfuscation-key-0123456789")
	server, err := NewObfuscatingPacketConn(serverUDP, testObfuscationOptions(current, previous))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewObfuscatingPacketConn(clientUDP, testObfuscationOptions(previous, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	defer client.Close()

	result := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 128)
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		n, _, readErr := server.ReadFrom(buf)
		if readErr != nil {
			errCh <- readErr
			return
		}
		result <- append([]byte(nil), buf[:n]...)
	}()
	if _, err := client.WriteTo([]byte("previous-key"), server.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	case got := <-result:
		if string(got) != "previous-key" {
			t.Fatalf("previous key payload = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for previous-key packet")
	}

	wrongUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer wrongUDP.Close()
	wrong, err := NewObfuscatingPacketConn(wrongUDP, testObfuscationOptions([]byte("wrong-obfuscation-key-0123456789"), nil))
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err := wrong.WriteTo([]byte("wrong-key"), server.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	_ = server.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, _, err := server.ReadFrom(buf); err == nil {
		t.Fatal("wrong-key datagram should be silently discarded")
	}
}

func TestObfuscatingPacketConnSocketDelegationAndValidation(t *testing.T) {
	if _, err := NewObfuscatingPacketConn(nil, config.TransportObfuscationOptions{}); err == nil {
		t.Fatal("nil UDP socket should fail")
	}
	standardUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	standard, err := NewObfuscatingPacketConn(standardUDP, config.TransportObfuscationOptions{Mode: config.TransportObfuscationStandard})
	if err != nil {
		t.Fatal(err)
	}
	if standard != standardUDP {
		t.Fatal("standard obfuscation should preserve the native UDP socket")
	}
	_ = standard.Close()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := NewObfuscatingPacketConn(udp, testObfuscationOptions([]byte("delegation-key-01234567890123456789"), nil))
	if err != nil {
		t.Fatal(err)
	}
	conn := wrapped.(*obfuscationPacketConn)
	if conn.LocalAddr() == nil {
		t.Fatal("wrapped local address is nil")
	}
	if err := conn.SetReadBuffer(64 << 10); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteBuffer(64 << 10); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal("close should be idempotent: ", err)
	}
}

func TestFragmentEvictionReleasesAccounting(t *testing.T) {
	now := time.Now()
	conn := &obfuscationPacketConn{
		parts:   make(map[fragmentKey]*fragmentAssembly),
		sources: make(map[string]int),
	}
	expired := fragmentKey{source: "expired", id: 1}
	live := fragmentKey{source: "live", id: 2}
	conn.parts[expired] = &fragmentAssembly{
		total:    2,
		parts:    [][]byte{[]byte("old"), nil},
		bytes:    3,
		deadline: now.Add(-time.Second),
	}
	conn.parts[live] = &fragmentAssembly{
		total:    2,
		parts:    [][]byte{[]byte("new"), nil},
		bytes:    3,
		deadline: now.Add(time.Second),
	}
	conn.sources[expired.source] = 1
	conn.sources[live.source] = 1
	conn.bytes = 6

	conn.evictFragmentsLocked(now)
	if _, ok := conn.parts[expired]; ok || conn.sources[expired.source] != 0 || conn.bytes != 3 {
		t.Fatalf("expired fragment accounting = parts %#v sources %#v bytes %d", conn.parts, conn.sources, conn.bytes)
	}
	if _, ok := conn.parts[live]; !ok || conn.sources[live.source] != 1 {
		t.Fatalf("live fragment was evicted: %#v", conn.parts)
	}
}

func testObfuscationOptions(current, previous []byte) config.TransportObfuscationOptions {
	return config.TransportObfuscationOptions{
		Mode:               config.TransportObfuscationCamouflage,
		CurrentKey:         current,
		PreviousKey:        previous,
		HandshakeShaping:   true,
		MinFragmentBytes:   512,
		MaxFragmentBytes:   1200,
		MaxWirePacketBytes: 1280,
	}
}
