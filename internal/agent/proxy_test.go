package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/proxy"
	"asterferry/internal/routing"
	"asterferry/internal/transport"
)

type proxyTestOutbound struct {
	streamFn   func(context.Context, proxy.Target, proxy.Path) (io.ReadWriteCloser, error)
	datagramFn func(context.Context, proxy.Target, proxy.Path) (io.ReadWriteCloser, error)
}

func (o *proxyTestOutbound) OpenStream(ctx context.Context, target proxy.Target, path proxy.Path) (io.ReadWriteCloser, error) {
	if o == nil || o.streamFn == nil {
		return nil, errors.New("test stream unavailable")
	}
	return o.streamFn(ctx, target, path)
}

func (o *proxyTestOutbound) OpenDatagram(ctx context.Context, target proxy.Target, path proxy.Path) (io.ReadWriteCloser, error) {
	if o == nil || o.datagramFn == nil {
		return nil, errors.New("test datagram unavailable")
	}
	return o.datagramFn(ctx, target, path)
}

func newProxyTestAgent(t *testing.T) *Agent {
	t.Helper()
	a := testAgentRuntime()
	a.cfg = &config.AgentOptions{
		Limits: config.Limits{
			MaxFrameBytes:      4096,
			MaxRecordBytes:     1024,
			MaxUDPBytes:        4096,
			MaxStreamsPerAgent: 4,
			DialTimeoutSec:     1,
			UDPIdleTimeoutSec:  1,
		},
		Obfuscation: config.ObfuscationConfig{
			ProxyProfile:    config.ProfileStandard,
			MaxPaddingBytes: 16,
		},
		Agent: config.AgentRuntime{Proxy: config.ProxyOptions{
			DefaultRoute: config.RouteDirect,
		}},
		StreamLimit: 4,
	}
	r, err := routing.New(config.ProxyConfig{DefaultRoute: config.RouteDirect})
	if err != nil {
		t.Fatal(err)
	}
	a.router = r
	t.Cleanup(func() {
		a.cancel()
		_ = r.Close()
	})
	return a
}

func writeProxyBytes(t *testing.T, conn net.Conn, data []byte) {
	t.Helper()
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
}

func readProxyBytes(t *testing.T, conn net.Conn, size int) []byte {
	t.Helper()
	result := make([]byte, size)
	if _, err := io.ReadFull(conn, result); err != nil {
		t.Fatal(err)
	}
	return result
}

func waitProxyHandler(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy handler did not finish")
	}
}

func TestHandleSOCKSConnectRelaysData(t *testing.T) {
	a := newProxyTestAgent(t)
	remote, peer := net.Pipe()
	defer peer.Close()
	a.outbound = &proxyTestOutbound{
		streamFn: func(_ context.Context, target proxy.Target, path proxy.Path) (io.ReadWriteCloser, error) {
			if target.Network != "tcp" || target.Host != "127.0.0.1" || target.Port != 443 || path != proxy.PathDirect {
				t.Fatalf("unexpected SOCKS target: %#v path=%q", target, path)
			}
			return remote, nil
		},
	}

	remoteDone := make(chan struct{})
	go func() {
		defer close(remoteDone)
		buf := make([]byte, 4)
		if _, err := io.ReadFull(peer, buf); err == nil {
			_, _ = peer.Write([]byte("pong"))
		}
		_ = peer.Close()
	}()

	server, client := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	done := make(chan struct{})
	go func() {
		a.handleSOCKS(server, config.Inbound{Tag: "socks"})
		close(done)
	}()

	writeProxyBytes(t, client, []byte{5, 1, 0})
	if got := readProxyBytes(t, client, 2); string(got) != string([]byte{5, 0}) {
		t.Fatalf("SOCKS method response = %x", got)
	}
	writeProxyBytes(t, client, []byte{5, 1, 0, 1, 127, 0, 0, 1, 1, 187})
	reply := readProxyBytes(t, client, 10)
	if reply[1] != 0 {
		t.Fatalf("SOCKS connect reply = %x", reply)
	}
	writeProxyBytes(t, client, []byte("ping"))
	if got := string(readProxyBytes(t, client, 4)); got != "pong" {
		t.Fatalf("SOCKS relay response = %q", got)
	}
	_ = client.Close()
	waitProxyHandler(t, done)
	waitProxyHandler(t, remoteDone)
}

func TestHandleSOCKSAuthenticationAndRejectsRequests(t *testing.T) {
	t.Run("authenticated connect", func(t *testing.T) {
		a := newProxyTestAgent(t)
		remote, peer := net.Pipe()
		defer peer.Close()
		a.outbound = &proxyTestOutbound{streamFn: func(context.Context, proxy.Target, proxy.Path) (io.ReadWriteCloser, error) {
			return remote, nil
		}}
		go func() {
			_, _ = io.Copy(io.Discard, peer)
			_ = peer.Close()
		}()
		server, client := net.Pipe()
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		done := make(chan struct{})
		go func() {
			a.handleSOCKS(server, config.Inbound{Tag: "auth", User: "user", Password: "pass"})
			close(done)
		}()
		writeProxyBytes(t, client, []byte{5, 1, 2})
		if got := readProxyBytes(t, client, 2); string(got) != string([]byte{5, 2}) {
			t.Fatalf("auth method response = %x", got)
		}
		writeProxyBytes(t, client, []byte{1, 4, 'u', 's', 'e', 'r', 4, 'p', 'a', 's', 's'})
		if got := readProxyBytes(t, client, 2); string(got) != string([]byte{1, 0}) {
			t.Fatalf("auth response = %x", got)
		}
		writeProxyBytes(t, client, []byte{5, 1, 0, 1, 127, 0, 0, 1, 0, 80})
		if reply := readProxyBytes(t, client, 10); reply[1] != 0 {
			t.Fatalf("authenticated connect reply = %x", reply)
		}
		_ = client.Close()
		waitProxyHandler(t, done)
	})

	t.Run("unsupported authentication method", func(t *testing.T) {
		a := newProxyTestAgent(t)
		server, client := net.Pipe()
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		done := make(chan struct{})
		go func() {
			a.handleSOCKS(server, config.Inbound{User: "user", Password: "pass"})
			close(done)
		}()
		writeProxyBytes(t, client, []byte{5, 1, 0})
		if got := readProxyBytes(t, client, 2); string(got) != string([]byte{5, 0xff}) {
			t.Fatalf("unsupported method response = %x", got)
		}
		waitProxyHandler(t, done)
	})

	t.Run("invalid command and address", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body []byte
			code byte
		}{
			{name: "command", body: []byte{5, 2, 0, 1, 127, 0, 0, 1, 0, 80}, code: 7},
			{name: "address", body: []byte{5, 1, 0, 9}, code: 8},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a := newProxyTestAgent(t)
				server, client := net.Pipe()
				defer client.Close()
				_ = client.SetDeadline(time.Now().Add(2 * time.Second))
				done := make(chan struct{})
				go func() {
					a.handleSOCKS(server, config.Inbound{Tag: "reject"})
					close(done)
				}()
				writeProxyBytes(t, client, []byte{5, 1, 0})
				_ = readProxyBytes(t, client, 2)
				writeProxyBytes(t, client, tc.body)
				if reply := readProxyBytes(t, client, 10); reply[1] != tc.code {
					t.Fatalf("reject reply = %x, code=%d", reply, tc.code)
				}
				waitProxyHandler(t, done)
			})
		}
	})
}

func TestHandleSOCKSOpenFailure(t *testing.T) {
	a := newProxyTestAgent(t)
	a.outbound = &proxyTestOutbound{streamFn: func(context.Context, proxy.Target, proxy.Path) (io.ReadWriteCloser, error) {
		return nil, errors.New("upstream unavailable")
	}}
	server, client := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	done := make(chan struct{})
	go func() {
		a.handleSOCKS(server, config.Inbound{Tag: "socks"})
		close(done)
	}()
	writeProxyBytes(t, client, []byte{5, 1, 0})
	_ = readProxyBytes(t, client, 2)
	writeProxyBytes(t, client, []byte{5, 1, 0, 1, 127, 0, 0, 1, 1, 187})
	if reply := readProxyBytes(t, client, 10); reply[1] != 5 {
		t.Fatalf("failed SOCKS reply = %x", reply)
	}
	waitProxyHandler(t, done)
}

func TestHandleHTTPForwardsRequestAndStripsProxyCredentials(t *testing.T) {
	a := newProxyTestAgent(t)
	remote, peer := net.Pipe()
	defer peer.Close()
	a.outbound = &proxyTestOutbound{streamFn: func(_ context.Context, target proxy.Target, path proxy.Path) (io.ReadWriteCloser, error) {
		if target.Host != "127.0.0.1" || target.Port != 8080 || path != proxy.PathDirect {
			t.Fatalf("unexpected HTTP target: %#v path=%q", target, path)
		}
		return remote, nil
	}}
	remoteDone := make(chan struct{})
	go func() {
		defer close(remoteDone)
		req, err := http.ReadRequest(bufio.NewReader(peer))
		if err == nil {
			if value := req.Header.Get("Proxy-Authorization"); value != "" {
				t.Errorf("proxy credentials reached upstream: %q", value)
			}
			_, _ = peer.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
		}
		_ = peer.Close()
	}()

	server, client := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	done := make(chan struct{})
	go func() {
		a.handleHTTP(server, config.Inbound{Tag: "http"})
		close(done)
	}()
	request := "GET http://127.0.0.1:8080/path HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nProxy-Authorization: Basic dXNlcjpwYXNz\r\nConnection: close\r\n\r\n"
	writeProxyBytes(t, client, []byte(request))
	response := readProxyBytes(t, client, len("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	if string(response) != "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok" {
		t.Fatalf("HTTP response = %q", response)
	}
	_ = client.Close()
	waitProxyHandler(t, done)
	waitProxyHandler(t, remoteDone)
}

func TestHandleHTTPConnectRelaysData(t *testing.T) {
	a := newProxyTestAgent(t)
	remote, peer := net.Pipe()
	defer peer.Close()
	a.outbound = &proxyTestOutbound{streamFn: func(context.Context, proxy.Target, proxy.Path) (io.ReadWriteCloser, error) {
		return remote, nil
	}}
	remoteDone := make(chan struct{})
	go func() {
		defer close(remoteDone)
		buf := make([]byte, 4)
		if _, err := io.ReadFull(peer, buf); err == nil {
			_, _ = peer.Write([]byte("pong"))
		}
		_ = peer.Close()
	}()
	server, client := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	done := make(chan struct{})
	go func() {
		a.handleHTTP(server, config.Inbound{Tag: "connect"})
		close(done)
	}()
	writeProxyBytes(t, client, []byte("CONNECT 127.0.0.1:8443 HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n"))
	response := make([]byte, len("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != "HTTP/1.1 200 Connection Established\r\n\r\n" {
		t.Fatalf("CONNECT response = %q", response)
	}
	writeProxyBytes(t, client, []byte("ping"))
	if got := string(readProxyBytes(t, client, 4)); got != "pong" {
		t.Fatalf("CONNECT relay response = %q", got)
	}
	_ = client.Close()
	waitProxyHandler(t, done)
	waitProxyHandler(t, remoteDone)
}

func TestHandleHTTPRejectsAuthAndInvalidUpstream(t *testing.T) {
	t.Run("authentication", func(t *testing.T) {
		a := newProxyTestAgent(t)
		server, client := net.Pipe()
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		done := make(chan struct{})
		go func() {
			a.handleHTTP(server, config.Inbound{Tag: "auth", User: "user", Password: "pass"})
			close(done)
		}()
		writeProxyBytes(t, client, []byte("GET http://127.0.0.1:8080/ HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nProxy-Authorization: Basic d3Jvbmc6cGFzcw==\r\n\r\n"))
		line, err := bufio.NewReader(client).ReadString('\n')
		if err != nil || !strings.Contains(line, "407") {
			t.Fatalf("authentication response = %q, err=%v", line, err)
		}
		waitProxyHandler(t, done)
	})

	for _, tc := range []struct {
		name string
		req  string
		fn   func(context.Context, proxy.Target, proxy.Path) (io.ReadWriteCloser, error)
	}{
		{
			name: "invalid port",
			req:  "GET http://127.0.0.1:not-a-port/ HTTP/1.1\r\nHost: 127.0.0.1:not-a-port\r\n\r\n",
		},
		{
			name: "open failure",
			req:  "GET http://127.0.0.1:8080/ HTTP/1.1\r\nHost: 127.0.0.1:8080\r\n\r\n",
			fn: func(context.Context, proxy.Target, proxy.Path) (io.ReadWriteCloser, error) {
				return nil, errors.New("upstream unavailable")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newProxyTestAgent(t)
			a.outbound = &proxyTestOutbound{streamFn: tc.fn}
			server, client := net.Pipe()
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(2 * time.Second))
			done := make(chan struct{})
			go func() {
				a.handleHTTP(server, config.Inbound{Tag: "http"})
				close(done)
			}()
			writeProxyBytes(t, client, []byte(tc.req))
			waitProxyHandler(t, done)
		})
	}
}

type errorWriteCloser struct{}

func (errorWriteCloser) Read([]byte) (int, error)  { return 0, io.EOF }
func (errorWriteCloser) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func (errorWriteCloser) Close() error              { return nil }

func TestHandleHTTPUpstreamWriteFailure(t *testing.T) {
	a := newProxyTestAgent(t)
	a.outbound = &proxyTestOutbound{streamFn: func(context.Context, proxy.Target, proxy.Path) (io.ReadWriteCloser, error) {
		return errorWriteCloser{}, nil
	}}
	server, client := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	done := make(chan struct{})
	go func() {
		a.handleHTTP(server, config.Inbound{Tag: "http"})
		close(done)
	}()
	writeProxyBytes(t, client, []byte("GET http://127.0.0.1:8080/ HTTP/1.1\r\nHost: 127.0.0.1:8080\r\n\r\n"))
	waitProxyHandler(t, done)
}

func TestHandleSOCKSUDPDirectDatagram(t *testing.T) {
	a := newProxyTestAgent(t)
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
	a.outbound = &proxyTestOutbound{datagramFn: func(_ context.Context, target proxy.Target, path proxy.Path) (io.ReadWriteCloser, error) {
		if target.Network != "udp" || path != proxy.PathDirect {
			t.Fatalf("unexpected UDP target: %#v path=%q", target, path)
		}
		return net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(target.Host), Port: int(target.Port)})
	}}

	server, control := net.Pipe()
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan struct{})
	go func() {
		a.handleSOCKS(server, config.Inbound{Tag: "udp"})
		close(done)
	}()
	writeProxyBytes(t, control, []byte{5, 1, 0})
	_ = readProxyBytes(t, control, 2)
	writeProxyBytes(t, control, []byte{5, 3, 0, 1, 127, 0, 0, 1, 0, 53})
	reply := readProxyBytes(t, control, 10)
	if reply[1] != 0 {
		t.Fatalf("UDP association reply = %x", reply)
	}
	udpPort := int(reply[8])<<8 | int(reply[9])
	clientUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer clientUDP.Close()
	serverUDP := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udpPort}
	if _, err := clientUDP.WriteToUDP(socksDatagram(target.LocalAddr().String(), []byte("ping")), serverUDP); err != nil {
		t.Fatal(err)
	}
	_ = clientUDP.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _, err := clientUDP.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _, payload, ok := parseSocksDatagram(buf[:n])
	if !ok || string(payload) != "ping" {
		t.Fatalf("UDP proxy response = %x ok=%v", buf[:n], ok)
	}
	a.cancel()
	waitProxyHandler(t, done)
}

func TestUDPPathRemoteFramesAndCleanup(t *testing.T) {
	a := newProxyTestAgent(t)
	a.router.Default = config.RouteGateway
	remote, peer := net.Pipe()
	defer peer.Close()
	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	source := client.LocalAddr().(*net.UDPAddr)
	a.outbound = &proxyTestOutbound{datagramFn: func(context.Context, proxy.Target, proxy.Path) (io.ReadWriteCloser, error) {
		return remote, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := a.newUDPPath(ctx, "udp", "127.0.0.1", 53, client, source)
	if p == nil || !p.remote {
		t.Fatal("gateway UDP path was not created")
	}
	frameCh := make(chan struct {
		frame transport.Frame
		err   error
	}, 1)
	go func() {
		frame, err := transport.ReadFrame(peer, 4096)
		frameCh <- struct {
			frame transport.Frame
			err   error
		}{frame: frame, err: err}
	}()
	if err := p.write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	result := <-frameCh
	frame, err := result.frame, result.err
	if err != nil || frame.Type != transport.TypeData {
		t.Fatalf("remote UDP frame = %#v, err=%v", frame, err)
	}
	data, err := transport.DecodeData(frame, 4096, 16)
	if err != nil || string(data.Payload) != "ping" {
		t.Fatalf("remote UDP payload = %q, err=%v", data.Payload, err)
	}
	readDone := make(chan struct{})
	go func() {
		p.readResponses()
		close(readDone)
	}()
	response, _ := transport.MessageFrame(transport.TypeData, 0, transport.NewData([]byte("pong"), config.ProfileStandard, 16))
	if err := transport.WriteFrame(peer, response, 4096); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _, payload, ok := parseSocksDatagram(buf[:n])
	if !ok || string(payload) != "pong" {
		t.Fatalf("remote UDP response = %x ok=%v", buf[:n], ok)
	}
	_ = peer.Close()
	waitProxyHandler(t, readDone)
	if p.dead.Load() == false {
		t.Fatal("remote UDP path was not marked dead")
	}
}
