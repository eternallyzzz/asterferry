//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"asterferry/internal/controller"
	"asterferry/internal/domain"
	"asterferry/internal/node"
)

func TestControllerGatewayAgentQUICEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	configResult, err := controller.Init(ctx, controller.InitOptions{Dir: filepath.Join(root, "controller"), HTTPListen: "127.0.0.1:0", GRPCListen: "127.0.0.1:0", GRPCAdvertise: "127.0.0.1:9443", Username: "admin", Password: "integration-password"})
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := controller.LoadOrCreateMasterKey(configResult.Config.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := controller.OpenStore(configResult.Config.DatabasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	grpcContext, grpcCancel := context.WithCancel(ctx)
	defer grpcCancel()
	grpcListener, grpcServer, serveErr, err := controller.StartGRPCWithErrors(grpcContext, configResult.Config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer grpcServer.Stop()
	defer grpcListener.Close()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("control server stopped: %v", err)
		}
	default:
	}

	tcpEcho := startTCPEcho(t)
	udpEcho := startUDPEcho(t)
	httpTarget := startHTTPEcho(t)
	quicPort := freeUDPPort(t)
	tcpPort := freeTCPPort(t)
	udpPort := freeUDPPort(t)
	httpProxyPort := freeTCPPort(t)
	socksProxyPort := freeTCPPort(t)

	if err := store.CreateNode(ctx, domain.Node{ID: "gateway-e2e", Name: "gateway", Enabled: true}, controller.WriteOptions{Actor: "integration"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateNode(ctx, domain.Node{ID: "agent-e2e", Name: "agent", Enabled: true}, controller.WriteOptions{Actor: "integration"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gateway-e2e", PublicEndpoints: []string{net.JoinHostPort("127.0.0.1", fmt.Sprint(quicPort))}, PortPool: domain.PortPool{TCP: []domain.PortRange{{Min: uint16(tcpPort), Max: uint16(tcpPort)}}, UDP: []domain.PortRange{{Min: uint16(udpPort), Max: uint16(udpPort)}}}}, controller.WriteOptions{Actor: "integration"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent-e2e", Proxies: []domain.ProxySpec{{ID: "http-proxy", Protocol: "http", Bind: net.JoinHostPort("127.0.0.1", fmt.Sprint(httpProxyPort)), Enabled: true}, {ID: "socks-proxy", Protocol: "socks5", Bind: net.JoinHostPort("127.0.0.1", fmt.Sprint(socksProxyPort)), Enabled: true}}, Egress: domain.EgressPolicy{Enabled: false}}, controller.WriteOptions{Actor: "integration"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "tcp-service", AgentID: "agent-e2e", Protocol: domain.ProtocolTCP, LocalTarget: tcpEcho, PublicBind: "127.0.0.1", Enabled: true}, controller.WriteOptions{Actor: "integration"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "udp-service", AgentID: "agent-e2e", Protocol: domain.ProtocolUDP, LocalTarget: udpEcho, PublicBind: "127.0.0.1", Enabled: true}, controller.WriteOptions{Actor: "integration"}); err != nil {
		t.Fatal(err)
	}

	caPath := configResult.Config.CACertPath
	gatewayToken, _, err := store.CreateEnrollmentToken(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	agentToken, _, err := store.CreateEnrollmentToken(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gatewayBootstrap, err := node.Enroll(ctx, node.EnrollOptions{ControllerAddress: grpcListener.Addr().String(), Token: gatewayToken, NodeID: "gateway-e2e", CAPath: caPath, CachePath: filepath.Join(root, "gateway", "snapshot.cache")})
	if err != nil {
		t.Fatal(err)
	}
	agentBootstrap, err := node.Enroll(ctx, node.EnrollOptions{ControllerAddress: grpcListener.Addr().String(), Token: agentToken, NodeID: "agent-e2e", CAPath: caPath, CachePath: filepath.Join(root, "agent", "snapshot.cache")})
	if err != nil {
		t.Fatal(err)
	}

	assignments, err := store.ScheduleAgent(ctx, "agent-e2e", controller.WriteOptions{Actor: "integration"})
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected one assignment, got %d", len(assignments))
	}
	assignment := assignments[0]
	gatewayRuntime, err := node.NewRuntime(gatewayBootstrap, node.RuntimeOptions{CachePath: filepath.Join(root, "gateway", "snapshot.cache"), CacheKeyPath: filepath.Join(root, "gateway", "snapshot.key")})
	if err != nil {
		t.Fatal(err)
	}
	agentRuntime, err := node.NewRuntime(agentBootstrap, node.RuntimeOptions{CachePath: filepath.Join(root, "agent", "snapshot.cache"), CacheKeyPath: filepath.Join(root, "agent", "snapshot.key")})
	if err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 2)
	go func() { runErr <- gatewayRuntime.Run(ctx) }()
	go func() { runErr <- agentRuntime.Run(ctx) }()

	waitFor(t, ctx, func() bool {
		current, lookupErr := store.GetAssignment(ctx, assignment.ID)
		return lookupErr == nil && current.State == domain.AssignmentApplied
	})

	current, err := store.GetAssignment(ctx, assignment.ID)
	if err != nil {
		t.Fatal(err)
	}
	bindings := make(map[string]domain.Binding, len(current.Bindings))
	for _, binding := range current.Bindings {
		bindings[binding.ServiceID] = binding
	}
	if err := retryTCP(ctx, net.JoinHostPort("127.0.0.1", fmt.Sprint(bindings["tcp-service"].Port)), []byte("tcp-e2e")); err != nil {
		t.Fatal(err)
	}
	if err := retryUDP(ctx, net.JoinHostPort("127.0.0.1", fmt.Sprint(bindings["udp-service"].Port)), []byte("udp-e2e")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, func() bool {
		conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(httpProxyPort)), time.Second)
		if dialErr != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
	proxyURL, _ := url.Parse("http://" + net.JoinHostPort("127.0.0.1", fmt.Sprint(httpProxyPort)))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
	response, err := client.Get("http://" + httpTarget + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "asterferry-e2e" {
		t.Fatalf("HTTP proxy response: status=%d body=%q", response.StatusCode, body)
	}
	if err := socks5HTTP(ctx, net.JoinHostPort("127.0.0.1", fmt.Sprint(socksProxyPort)), httpTarget); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runErr:
		if err != nil && ctx.Err() == nil {
			t.Fatalf("node runtime stopped: %v", err)
		}
	default:
	}
}

func waitFor(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	port := listener.LocalAddr().(*net.UDPAddr).Port
	_ = listener.Close()
	return port
}

func startTCPEcho(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func startUDPEcho(t *testing.T) string {
	socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	go func() {
		buffer := make([]byte, 64<<10)
		for {
			n, remote, readErr := socket.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			_, _ = socket.WriteToUDP(buffer[:n], remote)
		}
	}()
	return socket.LocalAddr().String()
}

func startHTTPEcho(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "asterferry-e2e") })}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	return listener.Addr().String()
}

func retryTCP(ctx context.Context, address string, payload []byte) error {
	var last error
	for ctx.Err() == nil {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err = conn.Write(payload); err == nil {
				response := make([]byte, len(payload))
				_, err = io.ReadFull(conn, response)
				if err == nil && string(response) == string(payload) {
					_ = conn.Close()
					return nil
				}
			}
			_ = conn.Close()
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	if last == nil {
		last = ctx.Err()
	}
	return last
}

func retryUDP(ctx context.Context, address string, payload []byte) error {
	var last error
	for ctx.Err() == nil {
		remote, err := net.ResolveUDPAddr("udp", address)
		if err == nil {
			conn, dialErr := net.DialUDP("udp", nil, remote)
			if dialErr == nil {
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err = conn.Write(payload); err == nil {
					response := make([]byte, len(payload))
					_, err = io.ReadFull(conn, response)
					if err == nil && string(response) == string(payload) {
						_ = conn.Close()
						return nil
					}
				}
				_ = conn.Close()
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	if last == nil {
		last = ctx.Err()
	}
	return last
}

func socks5HTTP(ctx context.Context, proxyAddress, target string) error {
	conn, err := net.DialTimeout("tcp", proxyAddress, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return err
	}
	response := make([]byte, 2)
	if _, err := io.ReadFull(conn, response); err != nil || response[0] != 5 || response[1] != 0 {
		return fmt.Errorf("SOCKS5 method negotiation failed: %v", err)
	}
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	port := 0
	if _, err := fmt.Sscan(portText, &port); err != nil {
		return err
	}
	hostBytes := []byte(host)
	request := append([]byte{5, 1, 0, 3, byte(len(hostBytes))}, hostBytes...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	if _, err := conn.Write(request); err != nil {
		return err
	}
	response = make([]byte, 4)
	if _, err := io.ReadFull(conn, response); err != nil {
		return err
	}
	if response[1] != 0 {
		return fmt.Errorf("SOCKS5 CONNECT failed with code %d", response[1])
	}
	addressLength := 0
	switch response[3] {
	case 1:
		addressLength = 4
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return err
		}
		addressLength = int(length[0])
	case 4:
		addressLength = 16
	default:
		return errors.New("SOCKS5 reply address type is invalid")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(addressLength+2)); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n"); err != nil {
		return err
	}
	data, err := io.ReadAll(bufio.NewReader(conn))
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "asterferry-e2e") {
		return fmt.Errorf("SOCKS5 response did not contain target body")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
