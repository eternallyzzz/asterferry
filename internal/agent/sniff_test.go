package agent

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

func TestSniffTLSClientHelloAndReplay(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	go func() {
		_ = tls.Client(client, &tls.Config{ServerName: "Example.COM", InsecureSkipVerify: true}).Handshake()
	}()
	result, replay := sniffTLS(server, bufio.NewReader(server), 16<<10, time.Second)
	if result.Protocol != "tls_sni" || result.Domain != "example.com" {
		t.Fatalf("unexpected sniff result: %#v", result)
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(replay, header); err != nil {
		t.Fatal(err)
	}
	record := make([]byte, int(header[3])<<8|int(header[4]))
	if _, err := io.ReadFull(replay, record); err != nil {
		t.Fatal(err)
	}
}

func TestSniffTLSMalformedIsTransparent(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	want := []byte{22, 3, 3, 0, 2, 1, 2}
	go func() {
		_, _ = client.Write(want)
		_ = client.Close()
	}()
	result, replay := sniffTLS(server, bufio.NewReader(server), 1024, time.Second)
	if result.Domain != "" {
		t.Fatalf("malformed data produced a domain: %#v", result)
	}
	got, err := io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("sniff did not replay bytes: %x != %x", got, want)
	}
}
