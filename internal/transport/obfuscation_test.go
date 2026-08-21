package transport

import (
	"bytes"
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
