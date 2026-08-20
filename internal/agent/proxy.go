package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/quic"

	"asterferry/internal/config"
	"asterferry/internal/relay"
	"asterferry/internal/transport"
)

func (a *Agent) startInbound(in config.Inbound) (net.Listener, error) {
	l, err := net.Listen("tcp", in.Listen)
	if err != nil {
		return nil, err
	}
	if in.Protocol == "socks5" {
		go a.runSOCKS(l, in)
	} else {
		go a.runHTTP(l, in)
	}
	return l, nil
}

func (a *Agent) runSOCKS(l net.Listener, in config.Inbound) {
	defer l.Close()
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go a.handleSOCKS(conn, in)
	}
}

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
	if string(user) != in.User || string(pass) != in.Password {
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
	if addr == nil || addr.To4() == nil {
		addr = net.IPv4zero
	}
	b := []byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(b[8:], port)
	_, _ = conn.Write(b)
}

func (a *Agent) handleSOCKSConnect(conn net.Conn, br *bufio.Reader, tag, host string, port uint16) {
	_ = br // br has no buffered bytes after the request in normal SOCKS clients.
	if a.route(tag, host) == config.RouteDirect {
		dialer := &net.Dialer{Timeout: time.Duration(a.cfg.Limits.DialTimeoutSec) * time.Second}
		remote, err := dialer.DialContext(a.ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
		if err != nil {
			socksReply(conn, 5, nil, 0)
			return
		}
		defer remote.Close()
		socksReply(conn, 0, remote.LocalAddr().(*net.TCPAddr).IP, uint16(remote.LocalAddr().(*net.TCPAddr).Port))
		relay.Bidirectional(conn, remote, relay.Counters{In: func(n uint64) { a.metrics.BytesIn.Add(n) }, Out: func(n uint64) { a.metrics.BytesOut.Add(n) }})
		return
	}
	raw, err := a.OpenProxy(a.ctx, "tcp", host, port)
	if err != nil {
		socksReply(conn, 5, nil, 0)
		return
	}
	stream := relay.NewConn(raw, a.relayProfile(a.cfg.Obfuscation.ProxyProfile))
	defer stream.Close()
	socksReply(conn, 0, net.IPv4zero, 0)
	relay.Bidirectional(conn, stream, relay.Counters{In: func(n uint64) { a.metrics.BytesIn.Add(n) }, Out: func(n uint64) { a.metrics.BytesOut.Add(n) }})
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
			if len(paths) >= int(a.cfg.Limits.MaxStreamsPerAgent) {
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
	stream     *quic.Stream
	conn       *net.UDPConn
	addr       *net.UDPAddr
	client     *net.UDPConn
	clientAddr *net.UDPAddr
	target     string
	remote     bool
	mu         sync.Mutex
	last       atomic.Int64
	dead       atomic.Bool
	ctx        context.Context
	cancel     context.CancelFunc
}

func (p *udpPath) touch() {
	if p != nil {
		p.last.Store(time.Now().UnixNano())
	}
}

func (a *Agent) newUDPPath(ctx context.Context, tag, host string, port uint16, client *net.UDPConn, source *net.UDPAddr) *udpPath {
	p := &udpPath{agent: a, addr: source, client: client, clientAddr: source, target: net.JoinHostPort(host, strconv.Itoa(int(port))), ctx: ctx}
	p.touch()
	if a.route(tag, host) == config.RouteDirect {
		remoteAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(int(port))))
		if err != nil {
			return nil
		}
		remote, err := net.DialUDP("udp", nil, remoteAddr)
		if err != nil {
			return nil
		}
		p.conn = remote
		return p
	}
	stream, err := a.OpenProxy(ctx, "udp", host, port)
	if err != nil {
		return nil
	}
	p.stream = stream
	p.remote = true
	return p
}

func (p *udpPath) write(payload []byte) error {
	p.touch()
	if p.remote {
		f, _ := transport.JSONFrame(transport.TypeData, 0, transport.NewData(payload, p.agent.profile(p.agent.cfg.Obfuscation.ProxyProfile), p.agent.cfg.Obfuscation.MaxPaddingBytes))
		return writeFrame(p.stream, f, p.agent.cfg.Limits.MaxFrameBytes)
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
			f, err := transport.ReadFrame(p.stream, p.agent.cfg.Limits.MaxFrameBytes)
			if err != nil {
				return
			}
			if f.Type != transport.TypeData {
				return
			}
			d, err := transport.DecodeData(f, p.agent.cfg.Limits.MaxUDPBytes, p.agent.cfg.Obfuscation.MaxPaddingBytes)
			if err != nil {
				return
			}
			p.touch()
			p.client.WriteToUDP(socksDatagram(p.target, d.Payload), p.clientAddr)
			p.agent.metrics.BytesOut.Add(uint64(len(d.Payload)))
		}
	}
	buf := make([]byte, p.agent.cfg.Limits.MaxUDPBytes)
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

func (a *Agent) runHTTP(l net.Listener, in config.Inbound) {
	defer l.Close()
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go a.handleHTTP(conn, in)
	}
}

func (a *Agent) handleHTTP(conn net.Conn, in config.Inbound) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		log.Printf("http request parse: %v", err)
		return
	}
	if in.User != "" {
		u, p, ok := req.BasicAuth()
		if !ok || u != in.User || p != in.Password {
			_, _ = conn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic\r\n\r\n"))
			return
		}
	}
	// Proxy credentials are only for this local hop and must never cross the
	// encrypted tunnel or reach the destination server.
	req.Header.Del("Proxy-Authorization")
	log.Printf("http proxy %s %s %s", in.Tag, req.Method, req.Host)
	host, portText, err := net.SplitHostPort(req.Host)
	if err != nil {
		host = req.Host
		portText = "80"
	}
	portNum, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || portNum == 0 {
		return
	}
	if req.Method == http.MethodConnect {
		if a.route(in.Tag, host) == config.RouteDirect {
			dialer := &net.Dialer{Timeout: time.Duration(a.cfg.Limits.DialTimeoutSec) * time.Second}
			remote, err := dialer.DialContext(a.ctx, "tcp", net.JoinHostPort(host, portText))
			if err != nil {
				return
			}
			defer remote.Close()
			_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			relay.Bidirectional(conn, remote, relay.Counters{In: func(n uint64) { a.metrics.BytesIn.Add(n) }, Out: func(n uint64) { a.metrics.BytesOut.Add(n) }})
			return
		}
		raw, err := a.OpenProxy(a.ctx, "tcp", host, uint16(portNum))
		if err != nil {
			return
		}
		stream := relay.NewConn(raw, a.relayProfile(a.cfg.Obfuscation.ProxyProfile))
		defer stream.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		relay.Bidirectional(conn, stream, relay.Counters{In: func(n uint64) { a.metrics.BytesIn.Add(n) }, Out: func(n uint64) { a.metrics.BytesOut.Add(n) }})
		return
	}
	var request bytes.Buffer
	if err := req.Write(&request); err != nil {
		return
	}
	var remote io.ReadWriteCloser
	if a.route(in.Tag, host) == config.RouteDirect {
		dialer := &net.Dialer{Timeout: time.Duration(a.cfg.Limits.DialTimeoutSec) * time.Second}
		c, err := dialer.DialContext(a.ctx, "tcp", net.JoinHostPort(host, portText))
		if err != nil {
			return
		}
		remote = c
	} else {
		log.Printf("http proxy tunnel %s:%d", host, portNum)
		raw, err := a.OpenProxy(a.ctx, "tcp", host, uint16(portNum))
		if err != nil {
			log.Printf("http proxy open: %v", err)
			return
		}
		remote = relay.NewConn(raw, a.relayProfile(a.cfg.Obfuscation.ProxyProfile))
	}
	defer remote.Close()
	if _, err := remote.Write(request.Bytes()); err != nil {
		log.Printf("http proxy write request: %v", err)
		return
	}
	if flusher, ok := remote.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	relay.Bidirectional(&bufferedConn{Conn: conn, Reader: br}, remote, relay.Counters{In: func(n uint64) { a.metrics.BytesIn.Add(n) }, Out: func(n uint64) { a.metrics.BytesOut.Add(n) }})
	log.Printf("http proxy finished %s", req.Host)
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
