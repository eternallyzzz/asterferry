package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"asterferry/internal/agent"
	"asterferry/internal/config"
	"asterferry/internal/gateway"
	"asterferry/internal/relay"
)

func BenchmarkAsterFerryProxy(b *testing.B) {
	for _, mode := range []string{config.TransportObfuscationStandard, config.TransportObfuscationCamouflage} {
		for _, profile := range []string{config.ProfileStandard, config.ProfileBalanced} {
			for _, payloadSize := range []int{16 << 10, 64 << 10} {
				for _, streams := range []int{1, 8, 32, 64} {
					mode, profile, payloadSize, streams := mode, profile, payloadSize, streams
					b.Run(fmt.Sprintf("mode=%s/profile=%s/payload=%d/streams=%d", mode, profile, payloadSize, streams), func(b *testing.B) {
						payload := bytes.Repeat([]byte("p"), payloadSize)
						_, a, echoPort := startBenchmarkPair(b, profile, mode)
						profileConfig, err := relay.NewProfile(profile, 16<<10, 2048)
						if err != nil {
							b.Fatal(err)
						}
						connections := make([]*relay.Conn, 0, streams)
						for i := 0; i < streams; i++ {
							stream, err := a.OpenProxy(context.Background(), "tcp", "127.0.0.1", uint16(echoPort))
							if err != nil {
								b.Fatal(err)
							}
							connections = append(connections, relay.NewConn(stream, profileConfig))
						}
						defer func() {
							for _, conn := range connections {
								_ = conn.Close()
							}
						}()

						b.SetBytes(int64(len(payload) * streams))
						b.ReportAllocs()
						b.ResetTimer()
						errs := make(chan error, streams)
						var wg sync.WaitGroup
						for _, conn := range connections {
							conn := conn
							wg.Add(1)
							go func() {
								defer wg.Done()
								result := make([]byte, len(payload))
								for i := 0; i < b.N; i++ {
									if _, err := conn.Write(payload); err != nil {
										errs <- err
										return
									}
									if _, err := io.ReadFull(conn, result); err != nil {
										errs <- err
										return
									}
								}
							}()
						}
						wg.Wait()
						b.StopTimer()
						select {
						case err := <-errs:
							b.Fatal(err)
						default:
						}
					})
				}
			}
		}
	}
}

func startBenchmarkPair(t testing.TB, profile, mode string) (*gateway.Gateway, *agent.Agent, int) {
	t.Helper()
	dir := t.TempDir()
	certs := makeCertificates(t, dir, "bench")
	tokenPath := filepath.Join(dir, "bench.token")
	if err := os.WriteFile(tokenPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	managementTokenPath := filepath.Join(dir, "management.token")
	if err := os.WriteFile(managementTokenPath, []byte("management-token-0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	transportObfuscation := config.TransportObfuscationConfig{Mode: mode}
	if mode == config.TransportObfuscationCamouflage {
		obfsKeyPath := filepath.Join(dir, "bench.obfs.key")
		if err := os.WriteFile(obfsKeyPath, []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"), 0o600); err != nil {
			t.Fatal(err)
		}
		transportObfuscation.KeyFile = obfsKeyPath
	}
	quicPort := freeUDPPort(t)
	managementGateway := freeTCPPort(t)
	managementAgent := freeTCPPort(t)
	echoListener, echoPort := startEcho(t)
	proxyPort := freeTCPPort(t)
	publicPort := freeTCPPort(t)
	alpn := "af-benchmark-1234"

	gatewayCfg := &config.Config{
		Version: config.ConfigVersion,
		Role:    config.RoleGateway,
		Transport: config.TransportConfig{
			ALPN: alpn, MaxBidiRemoteStreams: 128,
			InitialStreamReceiveWindow: 1 << 20, InitialConnectionReceiveWindow: 8 << 20,
			MaxStreamReadBuffer: 16 << 20, MaxConnReadBuffer: 64 << 20,
		},
		Management:  config.ManagementConfig{Listen: fmt.Sprintf("127.0.0.1:%d", managementGateway), AuthTokenFile: managementTokenPath},
		Limits:      config.Limits{MaxAgents: 4, MaxConnectionsPerAgent: 128, MaxStreamsPerAgent: 64, MaxFrameBytes: 2 << 20, MaxRecordBytes: 16 << 10, UDPIdleTimeoutSec: 5},
		Obfuscation: config.ObfuscationConfig{ProxyProfile: profile, ReverseProfile: profile, MaxPaddingBytes: 2048, Transport: transportObfuscation},
		Gateway: &config.GatewayConfig{
			Listen: fmt.Sprintf("127.0.0.1:%d", quicPort),
			TLS:    config.GatewayTLS{CertFile: certs.serverCert, KeyFile: certs.serverKey, ClientCAFile: certs.caCert},
			Agents: []config.GatewayAgent{{ID: "bench", TokenFile: tokenPath, Reverse: config.ReverseACL{TCPPorts: []string{fmt.Sprint(publicPort)}}, Egress: config.EgressPolicy{Enabled: true, TCPPorts: []string{fmt.Sprint(echoPort)}, AllowCIDRs: []string{"127.0.0.0/8"}, AllowSpecialCIDRs: []string{"127.0.0.0/8"}, MaxConnections: 128}}},
		},
	}
	if err := gatewayCfg.Validate(); err != nil {
		t.Fatal(err)
	}
	gatewayOpts, err := gatewayCfg.ResolveGateway()
	if err != nil {
		t.Fatal(err)
	}
	benchmarkLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g, err := gateway.New(gatewayOpts, benchmarkLogger)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}

	agentCfg := &config.Config{
		Version: config.ConfigVersion,
		Role:    config.RoleAgent,
		Transport: config.TransportConfig{
			ALPN: alpn, MaxBidiRemoteStreams: 128,
			InitialStreamReceiveWindow: 1 << 20, InitialConnectionReceiveWindow: 8 << 20,
			MaxStreamReadBuffer: 16 << 20, MaxConnReadBuffer: 64 << 20,
		},
		Management:  config.ManagementConfig{Listen: fmt.Sprintf("127.0.0.1:%d", managementAgent), AuthTokenFile: managementTokenPath},
		Limits:      config.Limits{MaxAgents: 4, MaxConnectionsPerAgent: 128, MaxStreamsPerAgent: 64, MaxFrameBytes: 2 << 20, MaxRecordBytes: 16 << 10, UDPIdleTimeoutSec: 5},
		Obfuscation: config.ObfuscationConfig{ProxyProfile: profile, ReverseProfile: profile, MaxPaddingBytes: 2048, Transport: transportObfuscation},
		Agent: &config.AgentConfig{
			ID: "bench", Server: fmt.Sprintf("127.0.0.1:%d", quicPort), TokenFile: tokenPath,
			TLS:     config.AgentTLS{CAFile: certs.caCert, CertFile: certs.clientCert, KeyFile: certs.clientKey, ServerName: "localhost"},
			Proxy:   config.ProxyConfig{Inbounds: []config.Inbound{{Tag: "bench", Protocol: "http", Listen: fmt.Sprintf("127.0.0.1:%d", proxyPort)}}, DefaultRoute: config.RouteGateway},
			Reverse: []config.Tunnel{{Name: "bench", Protocol: "tcp", Local: fmt.Sprintf("127.0.0.1:%d", echoPort), GatewayPort: uint16(publicPort)}},
		},
	}
	if err := agentCfg.Validate(); err != nil {
		t.Fatal(err)
	}
	agentOpts, err := agentCfg.ResolveAgent()
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agentOpts, benchmarkLogger)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		_ = g.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = a.Close()
		_ = g.Close()
		_ = echoListener.Close()
	})
	deadline := time.Now().Add(10 * time.Second)
	for !a.IsReady() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !a.IsReady() {
		t.Fatal("benchmark agent did not become ready")
	}
	return g, a, echoPort
}

func TestGatewayAgentCamouflageProxy(t *testing.T) {
	_, a, echoPort := startBenchmarkPair(t, config.ProfileBalanced, config.TransportObfuscationCamouflage)
	stream, err := a.OpenProxy(context.Background(), "tcp", "127.0.0.1", uint16(echoPort))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	profile, err := relay.NewProfile(config.ProfileBalanced, 16<<10, 2048)
	if err != nil {
		t.Fatal(err)
	}
	conn := relay.NewConn(stream, profile)
	defer conn.Close()
	if _, err := conn.Write([]byte("camouflage")); err != nil {
		t.Fatal(err)
	}
	if err := stream.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("camouflage"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "camouflage" {
		t.Fatalf("camouflage echo mismatch: %q", got)
	}
}

func TestGatewayAgentReverseTCP(t *testing.T) {
	dir := t.TempDir()
	certs := makeCertificates(t, dir, "edge-a")
	tokenPath := filepath.Join(dir, "edge-a.token")
	if err := os.WriteFile(tokenPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	managementTokenPath := filepath.Join(dir, "management.token")
	if err := os.WriteFile(managementTokenPath, []byte("management-token-0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	quicPort := freeUDPPort(t)
	managementGateway := freeTCPPort(t)
	managementAgent := freeTCPPort(t)
	echoListener, echoPort := startEcho(t)
	defer echoListener.Close()
	udpEcho, udpEchoPort := startUDPEcho(t)
	defer udpEcho.Close()
	publicPort := freeTCPPort(t)
	publicUDPPort := freeUDPPort(t)
	proxyPort := freeTCPPort(t)
	alpn := "af-integration-1234"

	gatewayCfg := &config.Config{
		Version:     config.ConfigVersion,
		Role:        config.RoleGateway,
		Transport:   config.TransportConfig{ALPN: alpn, MaxBidiRemoteStreams: 64},
		Management:  config.ManagementConfig{Listen: fmt.Sprintf("127.0.0.1:%d", managementGateway), AuthTokenFile: managementTokenPath},
		Limits:      config.Limits{MaxAgents: 4, MaxConnectionsPerAgent: 8, MaxStreamsPerAgent: 16, MaxFrameBytes: 1 << 20, MaxRecordBytes: 4096, UDPIdleTimeoutSec: 5},
		Obfuscation: config.ObfuscationConfig{ProxyProfile: config.ProfileBalanced, ReverseProfile: config.ProfileBalanced, MaxPaddingBytes: 256, Transport: config.TransportObfuscationConfig{Mode: config.TransportObfuscationStandard}},
		Gateway: &config.GatewayConfig{
			Listen: fmt.Sprintf("127.0.0.1:%d", quicPort),
			TLS:    config.GatewayTLS{CertFile: certs.serverCert, KeyFile: certs.serverKey, ClientCAFile: certs.caCert},
			Agents: []config.GatewayAgent{{ID: "edge-a", TokenFile: tokenPath, Reverse: config.ReverseACL{TCPPorts: []string{fmt.Sprint(publicPort)}, UDPPorts: []string{fmt.Sprint(publicUDPPort)}}, Egress: config.EgressPolicy{Enabled: true, TCPPorts: []string{fmt.Sprint(echoPort)}, AllowCIDRs: []string{"127.0.0.0/8"}, AllowSpecialCIDRs: []string{"127.0.0.0/8"}, MaxConnections: 4}}},
		},
	}
	if err := gatewayCfg.Validate(); err != nil {
		t.Fatal(err)
	}
	agentCfg := &config.Config{
		Version:     config.ConfigVersion,
		Role:        config.RoleAgent,
		Transport:   config.TransportConfig{ALPN: alpn, MaxBidiRemoteStreams: 64},
		Management:  config.ManagementConfig{Listen: fmt.Sprintf("127.0.0.1:%d", managementAgent), AuthTokenFile: managementTokenPath},
		Limits:      config.Limits{MaxAgents: 4, MaxConnectionsPerAgent: 8, MaxStreamsPerAgent: 16, MaxFrameBytes: 1 << 20, MaxRecordBytes: 4096, UDPIdleTimeoutSec: 5},
		Obfuscation: config.ObfuscationConfig{ProxyProfile: config.ProfileBalanced, ReverseProfile: config.ProfileBalanced, MaxPaddingBytes: 256, Transport: config.TransportObfuscationConfig{Mode: config.TransportObfuscationStandard}},
		Agent: &config.AgentConfig{
			ID: "edge-a", Server: fmt.Sprintf("127.0.0.1:%d", quicPort), TokenFile: tokenPath,
			TLS:     config.AgentTLS{CAFile: certs.caCert, CertFile: certs.clientCert, KeyFile: certs.clientKey, ServerName: "localhost"},
			Proxy:   config.ProxyConfig{Inbounds: []config.Inbound{{Tag: "http", Protocol: "http", Listen: fmt.Sprintf("127.0.0.1:%d", proxyPort)}}, DefaultRoute: config.RouteGateway},
			Reverse: []config.Tunnel{{Name: "echo", Protocol: "tcp", Local: fmt.Sprintf("127.0.0.1:%d", echoPort), GatewayPort: uint16(publicPort)}, {Name: "udp-echo", Protocol: "udp", Local: fmt.Sprintf("127.0.0.1:%d", udpEchoPort), GatewayPort: uint16(publicUDPPort)}},
		},
	}
	if err := agentCfg.Validate(); err != nil {
		t.Fatal(err)
	}

	gatewayOpts, err := gatewayCfg.ResolveGateway()
	if err != nil {
		t.Fatal(err)
	}
	g, err := gateway.New(gatewayOpts)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	agentOpts, err := agentCfg.ResolveAgent()
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agentOpts)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	deadline := time.Now().Add(8 * time.Second)
	var conn net.Conn
	for time.Now().Before(deadline) {
		conn, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 250*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("reverse mapping did not become available: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("asterferry")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len("asterferry"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "asterferry" {
		t.Fatalf("echo mismatch: %q", got)
	}

	proxy, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	if _, err := fmt.Fprintf(proxy, "CONNECT 127.0.0.1:%d HTTP/1.1\r\nHost: 127.0.0.1:%d\r\n\r\n", echoPort, echoPort); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 128)
	_ = proxy.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := proxy.Read(response)
	if err != nil {
		t.Fatal(err)
	}
	if !containsHeader(response[:n], "200 Connection Established") {
		t.Fatalf("proxy did not connect: %q", response[:n])
	}
	if _, err := proxy.Write([]byte("via-gateway")); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, len("via-gateway"))
	if _, err := io.ReadFull(proxy, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "via-gateway" {
		t.Fatalf("proxy echo mismatch: %q", got)
	}

	udpClient, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: publicUDPPort})
	if err != nil {
		t.Fatal(err)
	}
	defer udpClient.Close()
	if _, err := udpClient.Write([]byte("udp-through-reverse")); err != nil {
		t.Fatal(err)
	}
	_ = udpClient.SetReadDeadline(time.Now().Add(3 * time.Second))
	udpGot := make([]byte, 128)
	n, err = udpClient.Read(udpGot)
	if err != nil {
		t.Fatal(err)
	}
	if string(udpGot[:n]) != "udp-through-reverse" {
		t.Fatalf("udp echo mismatch: %q", udpGot[:n])
	}
}

type certificateFiles struct {
	caCert, serverCert, serverKey string
	clientCert, clientKey         string
}

func makeCertificates(t testing.TB, dir, agentID string) certificateFiles {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "AsterFerry Test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert := filepath.Join(dir, "ca.crt")
	writePEM(t, caCert, "CERTIFICATE", caDER)

	makeLeaf := func(name string, client bool) (string, string) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
		tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		if client {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
			identity, parseErr := url.Parse("urn:asterferry:agent:" + agentID)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			tmpl.URIs = []*url.URL{identity}
		} else {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tmpl.DNSNames = []string{"localhost"}
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caTemplate, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		certPath := filepath.Join(dir, name+".crt")
		keyPath := filepath.Join(dir, name+".key")
		writePEM(t, certPath, "CERTIFICATE", der)
		keyDER := x509.MarshalPKCS1PrivateKey(key)
		writePEM(t, keyPath, "RSA PRIVATE KEY", keyDER)
		return certPath, keyPath
	}
	serverCert, serverKey := makeLeaf("server", false)
	clientCert, clientKey := makeLeaf("edge-a", true)
	return certificateFiles{caCert: caCert, serverCert: serverCert, serverKey: serverKey, clientCert: clientCert, clientKey: clientKey}
}

func writePEM(t testing.TB, path, kind string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func freeUDPPort(t testing.TB) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func freeTCPPort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startEcho(t testing.TB) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return l, l.Addr().(*net.TCPAddr).Port
}

func startUDPEcho(t testing.TB) (*net.UDPConn, int) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP(buf[:n], addr)
		}
	}()
	return conn, conn.LocalAddr().(*net.UDPAddr).Port
}

func containsHeader(data []byte, value string) bool {
	return bytes.Contains(data, []byte(value))
}
