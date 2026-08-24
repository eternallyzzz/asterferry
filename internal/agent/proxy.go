package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"asterferry/internal/logging"

	"asterferry/internal/config"
	"asterferry/internal/proxy"
	"asterferry/internal/relay"
	"asterferry/internal/transport"
)

func (a *Agent) handleSOCKS(conn net.Conn, in config.Inbound) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(conn)
	version, err := br.ReadByte()
	if err != nil || version != 5 {
		return
	}
	nmethods, err := br.ReadByte()
	if err != nil || nmethods == 0 {
		return
	}
	methods := make([]byte, int(nmethods))
	if _, err = io.ReadFull(br, methods); err != nil {
		return
	}
	method := byte(0xff)
	if in.User == "" {
		for _, m := range methods {
			if m == 0 {
				method = 0
				break
			}
		}
	} else {
		for _, m := range methods {
			if m == 2 {
				method = 2
				break
			}
		}
	}
	_, _ = conn.Write([]byte{5, method})
	if method == 0xff {
		return
	}
	if method == 2 {
		if err := socksAuth(br, conn, in); err != nil {
			return
		}
	}
	_ = conn.SetDeadline(time.Time{})
	header := make([]byte, 4)
	if _, err := io.ReadFull(br, header); err != nil || header[0] != 5 || header[2] != 0 {
		return
	}
	host, port, err := readSocksAddress(br, header[3])
	if err != nil {
		socksReply(conn, 8, nil, 0)
		return
	}
	switch header[1] {
	case 1:
		a.handleSOCKSConnect(conn, br, in.Tag, host, port)
	case 3:
		a.handleSOCKSUDP(conn, in.Tag)
	default:
		socksReply(conn, 7, nil, 0)
	}
}

func socksAuth(br *bufio.Reader, conn net.Conn, in config.Inbound) error {
	ver, err := br.ReadByte()
	if err != nil || ver != 1 {
		return errors.New("invalid auth version")
	}
	ln, err := br.ReadByte()
	if err != nil {
		return err
	}
	user := make([]byte, ln)
	if _, err = io.ReadFull(br, user); err != nil {
		return err
	}
	ln, err = br.ReadByte()
	if err != nil {
		return err
	}
	pass := make([]byte, ln)
	if _, err = io.ReadFull(br, pass); err != nil {
		return err
	}
	if !constantTimeStringEqual(string(user), in.User) || !constantTimeStringEqual(string(pass), in.Password) {
		_, _ = conn.Write([]byte{1, 1})
		return errors.New("invalid credentials")
	}
	_, err = conn.Write([]byte{1, 0})
	return err
}

func readSocksAddress(r io.ByteReader, initial ...byte) (string, uint16, error) {
	var typ byte
	var err error
	if len(initial) > 0 {
		typ = initial[0]
	} else {
		typ, err = r.ReadByte()
		if err != nil {
			return "", 0, err
		}
	}
	var host string
	switch typ {
	case 1:
		b := make([]byte, 4)
		for i := range b {
			b[i], err = r.ReadByte()
			if err != nil {
				return "", 0, err
			}
		}
		host = net.IP(b).String()
	case 3:
		ln, e := r.ReadByte()
		if e != nil || ln == 0 {
			return "", 0, errors.New("invalid domain")
		}
		b := make([]byte, ln)
		for i := range b {
			b[i], err = r.ReadByte()
			if err != nil {
				return "", 0, err
			}
		}
		host = string(b)
	case 4:
		b := make([]byte, 16)
		for i := range b {
			b[i], err = r.ReadByte()
			if err != nil {
				return "", 0, err
			}
		}
		host = net.IP(b).String()
	default:
		return "", 0, errors.New("unsupported address type")
	}
	var p [2]byte
	p[0], err = r.ReadByte()
	if err != nil {
		return "", 0, err
	}
	p[1], err = r.ReadByte()
	if err != nil {
		return "", 0, err
	}
	return host, binary.BigEndian.Uint16(p[:]), nil
}

func socksReply(conn net.Conn, code byte, addr net.IP, port uint16) {
	address := addr.To4()
	if address == nil {
		address = net.IPv4zero
	}
	b := []byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}
	copy(b[4:8], address)
	binary.BigEndian.PutUint16(b[8:], port)
	_, _ = conn.Write(b)
}

func (a *Agent) handleSOCKSConnect(conn net.Conn, br *bufio.Reader, tag, host string, port uint16) {
	route, resolvedIP := a.routeTarget(tag, host)
	remote, err := a.outbound.OpenStream(a.ctx, proxy.Target{Network: "tcp", Host: host, Port: port, ResolvedIP: resolvedIP}, proxy.Path(route))
	if err != nil {
		a.logProxyEvent(tag, "socks5", host, port, route, "open_failed", route, agentErrorKind(err))
		socksReply(conn, 5, nil, 0)
		return
	}
	defer remote.Close()
	replyIP := net.IPv4zero
	var replyPort uint16
	if local, ok := remote.(net.Conn); ok {
		if address, ok := local.LocalAddr().(*net.TCPAddr); ok {
			replyIP = address.IP
			replyPort = uint16(address.Port)
		}
	}
	socksReply(conn, 0, replyIP, replyPort)
	client := a.sniffClient(conn, br, tag, "socks5", port)
	a.logProxyEvent(tag, "socks5", host, port, route, "connected", route, "")
	relay.BidirectionalWithIdle(a.ctx, client, remote, time.Duration(a.cfg.Limits.RelayIdleTimeoutSec)*time.Second, relay.Counters{In: func(n uint64) { a.metrics.BytesIn.Add(n) }, Out: func(n uint64) { a.metrics.BytesOut.Add(n) }})
}

func (a *Agent) handleSOCKSUDP(control net.Conn, tag string) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		socksReply(control, 1, nil, 0)
		return
	}
	defer udp.Close()
	addr := udp.LocalAddr().(*net.UDPAddr)
	socksReply(control, 0, addr.IP, uint16(addr.Port))
	clientIP, _, _ := net.SplitHostPort(control.RemoteAddr().String())
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	paths := map[string]*udpPath{}
	var mu sync.Mutex
	defer func() {
		mu.Lock()
		for _, p := range paths {
			p.close()
		}
		mu.Unlock()
	}()
	buf := make([]byte, a.cfg.Limits.MaxUDPBytes)
	for {
		_ = udp.SetReadDeadline(time.Now().Add(time.Second))
		n, source, err := udp.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				a.cleanupUDPPaths(&paths, &mu)
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			return
		}
		if clientIP != "" && source.IP.String() != clientIP {
			continue
		}
		host, port, payload, ok := parseSocksDatagram(buf[:n])
		if !ok {
			continue
		}
		key := net.JoinHostPort(host, strconv.Itoa(int(port)))
		mu.Lock()
		p := paths[key]
		if p != nil && p.dead.Load() {
			delete(paths, key)
			go p.close()
			p = nil
		}
		if p == nil {
			streamLimit := a.cfg.StreamLimit
			if streamLimit <= 0 {
				streamLimit = a.cfg.Limits.MaxStreamsPerAgent
			}
			if len(paths) >= int(streamLimit) {
				mu.Unlock()
				continue
			}
			p = a.newUDPPath(ctx, tag, host, port, udp, source)
			if p != nil {
				paths[key] = p
				go p.readResponses()
			}
		}
		var failedPath *udpPath
		if p != nil {
			p.touch()
			if err := p.write(payload); err != nil {
				delete(paths, key)
				failedPath = p
			}
		}
		mu.Unlock()
		if failedPath != nil {
			failedPath.dead.Store(true)
			failedPath.close()
		}
	}
}

func (a *Agent) cleanupUDPPaths(paths *map[string]*udpPath, mu *sync.Mutex) {
	cutoff := time.Now().Add(-time.Duration(a.cfg.Limits.UDPIdleTimeoutSec) * time.Second)
	mu.Lock()
	stale := make([]*udpPath, 0)
	for key, p := range *paths {
		if time.Unix(0, p.last.Load()).Before(cutoff) {
			delete(*paths, key)
			stale = append(stale, p)
		}
	}
	mu.Unlock()
	for _, p := range stale {
		p.close()
	}
}

func parseSocksDatagram(b []byte) (host string, port uint16, payload []byte, ok bool) {
	if len(b) < 4 || b[0] != 0 || b[1] != 0 || b[2] != 0 {
		return "", 0, nil, false
	}
	idx := 3
	if idx >= len(b) {
		return "", 0, nil, false
	}
	switch b[idx] {
	case 1:
		if len(b) < idx+1+4+2 {
			return "", 0, nil, false
		}
		host = net.IP(b[idx+1 : idx+5]).String()
		idx += 5
	case 3:
		if len(b) < idx+2 {
			return "", 0, nil, false
		}
		ln := int(b[idx+1])
		if len(b) < idx+2+ln+2 {
			return "", 0, nil, false
		}
		host = string(b[idx+2 : idx+2+ln])
		idx += 2 + ln
	case 4:
		if len(b) < idx+1+16+2 {
			return "", 0, nil, false
		}
		host = net.IP(b[idx+1 : idx+17]).String()
		idx += 17
	default:
		return "", 0, nil, false
	}
	port = binary.BigEndian.Uint16(b[idx : idx+2])
	idx += 2
	return host, port, b[idx:], true
}

type udpPath struct {
	agent      *Agent
	stream     io.ReadWriteCloser
	conn       net.Conn
	addr       *net.UDPAddr
	client     *net.UDPConn
	clientAddr *net.UDPAddr
	target     string
	remote     bool
	last       atomic.Int64
	dead       atomic.Bool
	limits     transport.Limits
	ctx        context.Context
	cancel     context.CancelFunc
}

func (p *udpPath) touch() {
	if p != nil {
		p.last.Store(time.Now().UnixNano())
	}
}

func (a *Agent) newUDPPath(ctx context.Context, tag, host string, port uint16, client *net.UDPConn, source *net.UDPAddr) *udpPath {
	limits := transport.LimitsFromConfig(a.cfg.Limits, a.cfg.StreamLimit)
	p := &udpPath{agent: a, addr: source, client: client, clientAddr: source, target: net.JoinHostPort(host, strconv.Itoa(int(port))), ctx: ctx, limits: limits}
	p.touch()
	routeName, resolvedIP := a.routeTarget(tag, host)
	route := proxy.Path(routeName)
	remote, err := a.outbound.OpenDatagram(ctx, proxy.Target{Network: "udp", Host: host, Port: port, ResolvedIP: resolvedIP}, route)
	if err != nil {
		return nil
	}
	if route == proxy.PathDirect {
		udpConn, ok := remote.(net.Conn)
		if !ok {
			_ = remote.Close()
			return nil
		}
		p.conn = udpConn
		return p
	}
	p.stream = remote
	p.remote = true
	if negotiated, ok := remote.(interface{ sessionLimits() transport.Limits }); ok {
		p.limits = negotiated.sessionLimits()
	}
	return p
}

func (p *udpPath) maxFrame() int64 {
	if p != nil && p.limits.MaxFrameBytes > 0 {
		return p.limits.MaxFrameBytes
	}
	if p != nil && p.agent != nil {
		return p.agent.cfg.Limits.MaxFrameBytes
	}
	return transport.DefaultMaxFrame
}

func (p *udpPath) maxUDP() int64 {
	if p != nil && p.limits.MaxUDPBytes > 0 {
		return p.limits.MaxUDPBytes
	}
	if p != nil && p.agent != nil {
		return p.agent.cfg.Limits.MaxUDPBytes
	}
	return 0
}

func (p *udpPath) maxPadding() int64 {
	if p == nil || p.agent == nil {
		return 0
	}
	padding := p.agent.cfg.Obfuscation.MaxPaddingBytes
	if max := p.limits.MaxRecordBytes - 8; max >= 0 && padding > max {
		padding = max
	}
	return padding
}

func (p *udpPath) write(payload []byte) error {
	p.touch()
	if p.remote {
		f, err := transport.MessageFrame(transport.TypeData, 0, transport.NewData(payload, p.agent.profile(p.agent.cfg.Obfuscation.ProxyProfile), p.maxPadding()))
		if err != nil {
			return err
		}
		return writeFrame(p.stream, f, p.maxFrame())
	}
	_, err := p.conn.Write(payload)
	return err
}

func (p *udpPath) readResponses() {
	defer func() {
		p.dead.Store(true)
		p.close()
	}()
	if p.remote {
		for {
			f, err := transport.ReadFrame(p.stream, p.maxFrame())
			if err != nil {
				return
			}
			if f.Type != transport.TypeData {
				return
			}
			d, err := transport.DecodeData(f, p.maxUDP(), p.maxPadding())
			if err != nil {
				return
			}
			p.touch()
			p.client.WriteToUDP(socksDatagram(p.target, d.Payload), p.clientAddr)
			p.agent.metrics.BytesOut.Add(uint64(len(d.Payload)))
		}
	}
	buf := make([]byte, p.maxUDP())
	for {
		_ = p.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := p.conn.Read(buf)
		if n > 0 {
			p.touch()
			p.client.WriteToUDP(socksDatagram(p.target, buf[:n]), p.clientAddr)
			p.agent.metrics.BytesOut.Add(uint64(n))
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				select {
				case <-p.ctx.Done():
					return
				default:
					continue
				}
			}
			return
		}
	}
}

func (p *udpPath) close() {
	if p.cancel != nil {
		p.cancel()
	}
	if p.stream != nil {
		_ = p.stream.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
}

func socksDatagram(target string, payload []byte) []byte {
	host, portText, _ := net.SplitHostPort(target)
	port, _ := strconv.ParseUint(portText, 10, 16)
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		b := make([]byte, 10+len(payload))
		copy(b, []byte{0, 0, 0, 1})
		copy(b[4:8], ip4)
		binary.BigEndian.PutUint16(b[8:10], uint16(port))
		copy(b[10:], payload)
		return b
	}
	if ip == nil {
		if len(host) > 255 {
			return nil
		}
		b := make([]byte, 7+len(host)+len(payload))
		copy(b, []byte{0, 0, 0, 3, byte(len(host))})
		copy(b[5:], host)
		binary.BigEndian.PutUint16(b[5+len(host):7+len(host)], uint16(port))
		copy(b[7+len(host):], payload)
		return b
	}
	b := make([]byte, 22+len(payload))
	copy(b, []byte{0, 0, 0, 4})
	copy(b[4:20], ip.To16())
	binary.BigEndian.PutUint16(b[20:22], uint16(port))
	copy(b[22:], payload)
	return b
}

func (a *Agent) handleHTTP(conn net.Conn, in config.Inbound) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(&httpHeaderLimitReader{Reader: conn, Max: 64 << 10})
	req, err := http.ReadRequest(br)
	if err != nil {
		a.logger.Info("HTTP request parse failed", "event", "proxy.http.parse_failed", "inbound", in.Tag, "error_kind", agentErrorKind(err))
		return
	}
	if in.User != "" {
		u, p, ok := req.BasicAuth()
		if !ok || !constantTimeStringEqual(u, in.User) || !constantTimeStringEqual(p, in.Password) {
			_, _ = conn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic\r\n\r\n"))
			return
		}
	}
	// Proxy credentials are only for this local hop and must never cross the
	// encrypted tunnel or reach the destination server.
	req.Header.Del("Proxy-Authorization")
	stripHopByHopHeaders(req.Header)
	_ = conn.SetDeadline(time.Time{})
	if len(req.RequestURI) > transport.MaxEndpointBytes || len(req.Host) > transport.MaxEndpointBytes || strings.ContainsAny(req.Host, "\x00\r\n") {
		return
	}
	host, portText, err := net.SplitHostPort(req.Host)
	if err != nil {
		host = req.Host
		portText = "80"
		if req.Method == http.MethodConnect {
			portText = "443"
		}
	}
	portNum, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || portNum == 0 {
		a.logProxyEvent(in.Tag, "http", host, 0, "", "invalid_destination", "", "invalid_port")
		return
	}
	if req.Method == http.MethodConnect {
		route, resolvedIP := a.routeTarget(in.Tag, host)
		remote, err := a.outbound.OpenStream(a.ctx, proxy.Target{Network: "tcp", Host: host, Port: uint16(portNum), ResolvedIP: resolvedIP}, proxy.Path(route))
		if err != nil {
			a.logProxyEvent(in.Tag, "http_connect", host, uint16(portNum), route, "open_failed", route, agentErrorKind(err))
			return
		}
		defer remote.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		client := a.sniffClient(conn, br, in.Tag, "http_connect", uint16(portNum))
		a.logProxyEvent(in.Tag, "http_connect", host, uint16(portNum), route, "connected", route, "")
		relay.BidirectionalWithIdle(a.ctx, client, remote, time.Duration(a.cfg.Limits.RelayIdleTimeoutSec)*time.Second, relay.Counters{In: func(n uint64) { a.metrics.BytesIn.Add(n) }, Out: func(n uint64) { a.metrics.BytesOut.Add(n) }})
		return
	}
	var remote io.ReadWriteCloser
	route, resolvedIP := a.routeTarget(in.Tag, host)
	remote, err = a.outbound.OpenStream(a.ctx, proxy.Target{Network: "tcp", Host: host, Port: uint16(portNum), ResolvedIP: resolvedIP}, proxy.Path(route))
	if err != nil {
		a.logProxyEvent(in.Tag, "http", host, uint16(portNum), route, "open_failed", route, agentErrorKind(err))
		return
	}
	defer remote.Close()
	if err := req.Write(remote); err != nil {
		a.logProxyEvent(in.Tag, "http", host, uint16(portNum), route, "write_failed", route, agentErrorKind(err))
		return
	}
	if flusher, ok := remote.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	relay.BidirectionalWithIdle(a.ctx, &bufferedConn{Conn: conn, Reader: br}, remote, time.Duration(a.cfg.Limits.RelayIdleTimeoutSec)*time.Second, relay.Counters{In: func(n uint64) { a.metrics.BytesIn.Add(n) }, Out: func(n uint64) { a.metrics.BytesOut.Add(n) }})
	a.logProxyEvent(in.Tag, "http", host, uint16(portNum), route, "finished", route, "")
}

const errHTTPHeadersTooLarge = "http request headers exceed 64KiB"

type httpHeaderLimitReader struct {
	io.Reader
	Max  int
	seen int
	tail [3]byte
	done bool
}

func (r *httpHeaderLimitReader) Read(p []byte) (int, error) {
	if r.done {
		return r.Reader.Read(p)
	}
	if r.Max <= 0 {
		r.Max = 64 << 10
	}
	if remaining := r.Max - r.seen; remaining <= 0 {
		return 0, errors.New(errHTTPHeadersTooLarge)
	} else if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := r.Reader.Read(p)
	if n == 0 {
		return n, err
	}
	if r.seen+n > r.Max {
		return n, errors.New(errHTTPHeadersTooLarge)
	}
	for _, value := range p[:n] {
		if r.tail[0] == '\r' && r.tail[1] == '\n' && r.tail[2] == '\r' && value == '\n' {
			r.done = true
			break
		}
		r.tail[0], r.tail[1], r.tail[2] = r.tail[1], r.tail[2], value
	}
	r.seen += n
	if !r.done && r.seen >= r.Max {
		return n, errors.New(errHTTPHeadersTooLarge)
	}
	return n, err
}

func stripHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				header.Del(name)
			}
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}

func constantTimeStringEqual(a, b string) bool {
	left := sha256.Sum256([]byte(a))
	right := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

type bufferedConn struct {
	net.Conn
	Reader io.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.Reader.Read(b) }
func (c *bufferedConn) CloseRead() error {
	if h, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return h.CloseRead()
	}
	return c.Conn.Close()
}
func (c *bufferedConn) CloseWrite() error {
	if h, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return h.CloseWrite()
	}
	return c.Conn.Close()
}

func (a *Agent) sniffClient(conn net.Conn, reader io.Reader, inbound, protocol string, port uint16) *bufferedConn {
	if a.cfg.Agent.Proxy.Sniff.Enabled {
		result, replay := sniffTLS(conn, reader, a.cfg.Agent.Proxy.Sniff.MaxBytes, time.Duration(a.cfg.Agent.Proxy.Sniff.TimeoutMillis)*time.Millisecond)
		if result.Domain != "" {
			attrs := []any{
				"event", "proxy.sniff.tls_sni",
				"inbound", inbound,
				"protocol", protocol,
				"target_port", port,
				"domain_hash", logging.DomainHash(result.Domain),
				"sniff_result", "observed",
			}
			if a.cfg.Logging.ExposeDomainAtDebug && a.logger.Enabled(context.Background(), slog.LevelDebug) {
				attrs = append(attrs, "domain", result.Domain)
				a.logger.Debug("TLS SNI observed", attrs...)
			} else {
				a.logger.Info("TLS SNI observed", attrs...)
			}
		}
		return &bufferedConn{Conn: conn, Reader: replay}
	}
	return &bufferedConn{Conn: conn, Reader: reader}
}

func (a *Agent) logProxyEvent(inbound, protocol, host string, port uint16, route, result, path, errKind string) {
	attrs := []any{
		"event", "proxy.request",
		"inbound", inbound,
		"protocol", protocol,
		"target_port", port,
		"route", route,
		"result", result,
		"path", path,
	}
	if protocol == "http" {
		attrs = append(attrs, "sniff_protocol", "http_host")
	}
	if host != "" {
		attrs = append(attrs, "target_hash", logging.DomainHash(host))
	}
	if errKind != "" {
		attrs = append(attrs, "error_kind", errKind)
	}
	a.logger.Info("proxy request", attrs...)
}
