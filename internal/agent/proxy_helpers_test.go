package agent

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"testing"

	"asterferry/internal/config"
	"asterferry/internal/transport"
)

func TestSOCKSAddressHelpersRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		wire []byte
		host string
		port uint16
	}{
		{"ipv4", append(append([]byte{1}, net.ParseIP("192.0.2.1").To4()...), 0x01, 0xbb), "192.0.2.1", 443},
		{"domain", append(append([]byte{3, 11}, []byte("example.com")...), 0x01, 0xbb), "example.com", 443},
		{"ipv6", append(append([]byte{4}, net.ParseIP("2001:db8::1").To16()...), 0x00, 0x35), "2001:db8::1", 53},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := readSocksAddress(bytes.NewBuffer(tc.wire))
			if err != nil || host != tc.host || port != tc.port {
				t.Fatalf("read address = %q:%d, err=%v", host, port, err)
			}
		})
	}
	for _, wire := range [][]byte{{9}, {3, 0}, {1, 192, 0}, {4, 0, 1}} {
		if _, _, err := readSocksAddress(bytes.NewBuffer(wire)); err == nil {
			t.Fatalf("invalid SOCKS address %#v was accepted", wire)
		}
	}

	for _, target := range []string{"192.0.2.1:443", "example.com:443", "[2001:db8::1]:53"} {
		wire := socksDatagram(target, []byte("payload"))
		host, port, payload, ok := parseSocksDatagram(wire)
		if !ok || port == 0 || !bytes.Equal(payload, []byte("payload")) {
			t.Fatalf("datagram %q parsed as %q:%d %q ok=%v", target, host, port, payload, ok)
		}
	}
	if socksDatagram("example.com:443", nil) == nil {
		t.Fatal("domain datagram should be encoded")
	}
	if socksDatagram(""+string(bytes.Repeat([]byte{'x'}, 256))+":443", nil) != nil {
		t.Fatal("oversized domain datagram should be rejected")
	}
	for _, wire := range [][]byte{nil, {0, 0, 1}, {0, 0, 0, 9}, {0, 0, 0, 3, 5, 'a'}} {
		if _, _, _, ok := parseSocksDatagram(wire); ok {
			t.Fatalf("invalid datagram %#v was accepted", wire)
		}
	}
}

func TestSOCKSAuthenticationAndReply(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	responseCh := make(chan []byte, 1)
	go func() {
		_, _ = client.Write([]byte{1, 4, 'u', 's', 'e', 'r', 4, 'p', 'a', 's', 's'})
		response := make([]byte, 2)
		_, _ = io.ReadFull(client, response)
		responseCh <- response
	}()
	if err := socksAuth(bufio.NewReader(server), server, config.Inbound{User: "user", Password: "pass"}); err != nil {
		t.Fatal(err)
	}
	response := <-responseCh
	if !bytes.Equal(response, []byte{1, 0}) {
		t.Fatalf("successful auth response = %x", response)
	}

	server2, client2 := net.Pipe()
	defer server2.Close()
	defer client2.Close()
	responseCh = make(chan []byte, 1)
	go func() {
		_, _ = client2.Write([]byte{1, 1, 'x', 1, 'y'})
		response := make([]byte, 2)
		_, _ = io.ReadFull(client2, response)
		responseCh <- response
	}()
	if err := socksAuth(bufio.NewReader(server2), server2, config.Inbound{User: "user", Password: "pass"}); err == nil {
		t.Fatal("invalid credentials should fail")
	}
	response = <-responseCh
	if !bytes.Equal(response, []byte{1, 1}) {
		t.Fatalf("failed auth response = %x", response)
	}
}

func TestAgentHelpersAndStatus(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil agent config should fail")
	}
	if _, err := New(&config.AgentOptions{}); err == nil {
		t.Fatal("agent config without credentials should fail")
	}
	a := testAgentRuntime()
	a.cfg.Agent.ID = "edge"
	a.cfg.Agent.Proxy.DefaultRoute = config.RouteDirect
	a.cfg.TransportObfuscation.Mode = config.TransportObfuscationStandard
	a.cfg.Obfuscation.MaxPaddingBytes = 4096
	a.cfg.Limits.MaxRecordBytes = 1024
	value, ok := a.Status().(status)
	if !ok || value.Mode != config.RoleAgent || value.AgentID != "edge" || value.Ready {
		t.Fatalf("agent status = %#v", a.Status())
	}
	if got := a.route("socks", "does-not-exist.invalid"); got != config.RouteGateway {
		t.Fatalf("unresolvable target route = %q", got)
	}
	if profile := a.relayProfileWithLimits("unknown", transportLimits(1024)); profile.Name != config.ProfileStandard {
		t.Fatalf("invalid relay profile fallback = %#v", profile)
	}
	if profile := a.relayProfileWithLimits(config.ProfileBalanced, transportLimits(1024)); profile.MaxPadding > 1016 {
		t.Fatalf("relay padding exceeded record limit: %#v", profile)
	}
	if agentErrorKind(context.Canceled) != "canceled" {
		t.Fatal("agent error kind did not preserve lifecycle classification")
	}
	if a.currentSessionID() != "" || a.IsReady() {
		t.Fatal("agent without session should not be ready")
	}
	_ = a.Close()
	_ = a.Close()
}

func transportLimits(maxRecord int64) transport.Limits {
	return transport.Limits{MaxRecordBytes: maxRecord, MaxFrameBytes: 4096, MaxUDPBytes: 1024, MaxStreams: 4}
}
