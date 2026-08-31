package dataplane

// This file contains the local HTTP and SOCKS5 proxy boundaries for an Agent
// node. They deliberately accept a dial callback rather than reaching into
// the Controller or a configuration package: direct egress, AFDP streams and
// policy/routing implementations can all be supplied by the node runtime.

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
	"strconv"
	"strings"
	"time"

	"asterferry/internal/domain"
	"asterferry/internal/duplex"
)

const (
	proxyHandshakeLimit = 64 << 10
	proxyTargetLimit    = 2048
	proxyIdleTimeout    = 2 * time.Minute
)

// ProxyDialFunc is the policy/egress seam used by both proxy protocols. The
// route is the node-snapshot route selected for the configured proxy entrance.
// Implementations must return an already connected stream or an error.
type ProxyDialFunc func(context.Context, string, string) (net.Conn, error)

// ServeProxy serves one snapshot-defined proxy entrance until ctx is
// cancelled or the listener fails. The supplied ProxySpec is copied and
// validated before any client connection is accepted.
func ServeProxy(ctx context.Context, engine *Engine, listener net.Listener, proxy domain.ProxySpec, dial ProxyDialFunc) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if engine == nil || listener == nil || dial == nil {
		return errors.New("proxy requires engine, listener and dialer")
	}
	if err := validateProxyForEngine(engine, proxy); err != nil {
		return err
	}
	switch proxy.Protocol {
	case "http":
		return ServeHTTPProxy(ctx, engine, listener, proxy, dial)
	case "socks5":
		return ServeSOCKS5(ctx, engine, listener, proxy, dial)
	default:
		return errors.New("proxy protocol must be http or socks5")
	}
}

func validateProxyForEngine(engine *Engine, proxy domain.ProxySpec) error {
	if err := domain.ValidateID(proxy.ID, "proxy.id"); err != nil {
		return err
	}
	if proxy.Protocol != "http" && proxy.Protocol != "socks5" {
		return errors.New("proxy protocol must be http or socks5")
	}
	if err := validateProxyBind(proxy.Bind); err != nil {
		return err
	}
	spec, ok := engine.AgentSpec()
	if !ok {
		return errors.New("agent proxy spec is not applied")
	}
	for _, configured := range spec.Proxies {
		if configured.ID == proxy.ID {
			if configured.Protocol != proxy.Protocol || configured.Bind != proxy.Bind {
				return errors.New("proxy does not match the applied agent spec")
			}
			if !configured.Enabled {
				return errors.New("proxy is disabled")
			}
			return nil
		}
	}
	return errors.New("proxy is not present in the applied agent spec")
}

func validateProxyBind(value string) error {
	host, portText, err := net.SplitHostPort(value)
	if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, " \t\r\n") {
		return errors.New("proxy bind must be host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strings.ContainsAny(portText, " \t\r\n") {
		return errors.New("proxy bind port must be between 1 and 65535")
	}
	return nil
}

// ServeHTTPProxy implements forward HTTP and CONNECT proxying. Proxy
// credentials, if a caller wants to add them, are handled before this seam;
// they are never forwarded to the destination.
func ServeHTTPProxy(ctx context.Context, engine *Engine, listener net.Listener, proxy domain.ProxySpec, dial ProxyDialFunc) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if engine == nil || listener == nil || dial == nil {
		return errors.New("HTTP proxy requires engine, listener and dialer")
	}
	if err := validateProxyForEngine(engine, proxy); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-done:
		}
	}()
	defer close(done)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		lease, err := engine.ReserveLocalOpen()
		if err != nil {
			_ = conn.Close()
			continue
		}
		// Pass the accepted socket into the goroutine.  A plain `for` loop
		// reuses its iteration variable; capturing `conn` directly can otherwise
		// make concurrent clients serve the wrong socket.
		go func(conn net.Conn, lease *OpenLease) {
			defer lease.Release()
			defer conn.Close()
			_ = handleHTTPProxyWithBuffer(ctx, conn, proxy, dial, engine.MaxBufferBytes())
		}(conn, lease)
	}
}

func handleHTTPProxyWithBuffer(ctx context.Context, conn net.Conn, proxy domain.ProxySpec, dial ProxyDialFunc, maxBuffer int) error {
	_ = conn.SetDeadline(time.Now().Add(proxyIdleTimeout))
	reader := bufio.NewReaderSize(io.LimitReader(conn, proxyHandshakeLimit), 4096)
	request, err := http.ReadRequest(reader)
	if err != nil {
		writeHTTPProxyError(conn, http.StatusBadRequest)
		return err
	}
	if request.Method == http.MethodConnect {
		target, err := normalizeHTTPConnectTarget(request.Host)
		if err != nil {
			writeHTTPProxyError(conn, http.StatusBadRequest)
			return err
		}
		upstream, err := dial(ctx, target, proxy.Route)
		if err != nil {
			writeHTTPProxyError(conn, http.StatusBadGateway)
			return err
		}
		defer upstream.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		return copyProxyDuplexReaderLimited(conn, io.MultiReader(reader, conn), upstream, maxBuffer)
	}
	if request.URL == nil || request.URL.Scheme == "" || request.URL.Host == "" {
		writeHTTPProxyError(conn, http.StatusBadRequest)
		return errors.New("HTTP proxy requests must use an absolute URL")
	}
	target, err := normalizeHTTPURLTarget(request.URL)
	if err != nil {
		writeHTTPProxyError(conn, http.StatusBadRequest)
		return err
	}
	upstream, err := dial(ctx, target, proxy.Route)
	if err != nil {
		writeHTTPProxyError(conn, http.StatusBadGateway)
		return err
	}
	defer upstream.Close()
	request.RequestURI = request.URL.RequestURI()
	request.URL.Scheme = ""
	request.URL.Host = ""
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Proxy-Connection")
	if err := request.Write(upstream); err != nil {
		return err
	}
	// http.ReadRequest may have read bytes belonging to the next request into
	// its buffered reader. Preserve those bytes before reading directly from
	// the socket so a client that pipelines payload immediately after headers
	// does not lose data at the protocol boundary.
	return copyProxyDuplexReaderLimited(conn, io.MultiReader(reader, conn), upstream, maxBuffer)
}

func normalizeHTTPConnectTarget(value string) (string, error) {
	if len(value) == 0 || len(value) > proxyTargetLimit || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("HTTP CONNECT target is invalid")
	}
	return normalizeHostPort(value)
}

func normalizeHTTPURLTarget(value *url.URL) (string, error) {
	if value == nil || len(value.Host) == 0 || len(value.Host) > proxyTargetLimit || strings.ContainsAny(value.Host, "\x00\r\n") {
		return "", errors.New("HTTP target is invalid")
	}
	scheme := strings.ToLower(strings.TrimSpace(value.Scheme))
	if scheme != "http" && scheme != "https" {
		return "", errors.New("HTTP scheme is unsupported")
	}
	port := value.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return normalizeHostPort(net.JoinHostPort(value.Hostname(), port))
}

func normalizeHostPort(value string) (string, error) {
	if strings.TrimSpace(value) != value || len(value) == 0 || len(value) > proxyTargetLimit || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("target must be host:port")
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil || strings.TrimSpace(host) == "" || host != strings.TrimSpace(host) || strings.ContainsAny(host, " \t") || strings.ContainsAny(portText, " \t") {
		return "", errors.New("target must be host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("target port is invalid")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func writeHTTPProxyError(conn net.Conn, code int) {
	message := http.StatusText(code)
	if message == "" {
		message = "Proxy Error"
	}
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", code, message)
}

// ServeSOCKS5 implements the no-auth SOCKS5 CONNECT path. Username/password
// authentication belongs at the local listener boundary and is intentionally
// absent from the node snapshot model; unsupported methods fail closed.
func ServeSOCKS5(ctx context.Context, engine *Engine, listener net.Listener, proxy domain.ProxySpec, dial ProxyDialFunc) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if engine == nil || listener == nil || dial == nil {
		return errors.New("SOCKS5 proxy requires engine, listener and dialer")
	}
	if err := validateProxyForEngine(engine, proxy); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-done:
		}
	}()
	defer close(done)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		lease, err := engine.ReserveLocalOpen()
		if err != nil {
			_ = conn.Close()
			continue
		}
		// See the HTTP listener above: pass the accepted connection explicitly
		// so each goroutine owns exactly one client socket.
		go func(conn net.Conn, lease *OpenLease) {
			defer lease.Release()
			defer conn.Close()
			_ = handleSOCKS5WithBuffer(ctx, conn, proxy, dial, engine.MaxBufferBytes())
		}(conn, lease)
	}
}

func handleSOCKS5WithBuffer(ctx context.Context, conn net.Conn, proxy domain.ProxySpec, dial ProxyDialFunc, maxBuffer int) error {
	_ = conn.SetDeadline(time.Now().Add(proxyIdleTimeout))
	reader := bufio.NewReaderSize(io.LimitReader(conn, proxyHandshakeLimit), 4096)
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return err
	}
	if header[0] != 5 || header[1] == 0 {
		return errors.New("invalid SOCKS5 greeting")
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return err
	}
	noAuth := false
	for _, method := range methods {
		if method == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		_, _ = conn.Write([]byte{5, 0xff})
		return errors.New("SOCKS5 client does not offer no-auth")
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return err
	}
	requestHeader := make([]byte, 4)
	if _, err := io.ReadFull(reader, requestHeader); err != nil {
		return err
	}
	if requestHeader[0] != 5 {
		return errors.New("invalid SOCKS5 request")
	}
	if requestHeader[1] != 1 {
		writeSOCKS5Reply(conn, 7)
		return errors.New("SOCKS5 command is unsupported")
	}
	if requestHeader[2] != 0 {
		writeSOCKS5Reply(conn, 1)
		return errors.New("SOCKS5 reserved byte is invalid")
	}
	host, err := readSOCKS5Address(reader, requestHeader[3])
	if err != nil {
		writeSOCKS5Reply(conn, 8)
		return err
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(reader, portBytes[:]); err != nil {
		return err
	}
	port := binary.BigEndian.Uint16(portBytes[:])
	if port == 0 {
		writeSOCKS5Reply(conn, 8)
		return errors.New("SOCKS5 target port is zero")
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	upstream, err := dial(ctx, target, proxy.Route)
	if err != nil {
		writeSOCKS5Reply(conn, 5)
		return err
	}
	defer upstream.Close()
	if err := writeSOCKS5Reply(conn, 0); err != nil {
		return err
	}
	return copyProxyDuplexReaderLimited(conn, io.MultiReader(reader, conn), upstream, maxBuffer)
}

func readSOCKS5Address(reader *bufio.Reader, atyp byte) (string, error) {
	switch atyp {
	case 1:
		value := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", err
		}
		return net.IP(value).String(), nil
	case 4:
		value := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", err
		}
		return net.IP(value).String(), nil
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(reader, length[:]); err != nil {
			return "", err
		}
		if length[0] == 0 || int(length[0]) > 253 {
			return "", errors.New("SOCKS5 domain is invalid")
		}
		value := make([]byte, int(length[0]))
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", err
		}
		if strings.ContainsAny(string(value), "\x00\r\n \t") {
			return "", errors.New("SOCKS5 domain is invalid")
		}
		return string(value), nil
	default:
		return "", errors.New("SOCKS5 address type is unsupported")
	}
}

func writeSOCKS5Reply(conn net.Conn, code byte) error {
	// A zero BND.ADDR with IPv4 is valid for a CONNECT response and avoids
	// disclosing the upstream socket address to the local client.
	_, err := conn.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

func copyProxyDuplexReaderLimited(client net.Conn, clientReader io.Reader, upstream net.Conn, maxBuffer int) error {
	return duplex.CopyDuplexWithReader(client, clientReader, upstream, maxBuffer)
}
