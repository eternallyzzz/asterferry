package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/quic"

	"asterferry/internal/config"
	"asterferry/internal/observability"
	"asterferry/internal/relay"
	"asterferry/internal/routing"
	"asterferry/internal/transport"
)

type Agent struct {
	cfg     *config.Config
	router  *routing.Router
	ctx     context.Context
	cancel  context.CancelFunc
	metrics *observability.Metrics
	mgmt    *observability.Server

	mu         sync.RWMutex
	session    *Session
	ready      chan struct{}
	connects   atomic.Int64
	mappings   map[string]config.Tunnel
	listenerMu sync.Mutex
	listeners  []net.Listener
}

type Session struct {
	agent     *Agent
	conn      *quic.Conn
	control   *quic.Stream
	ctx       context.Context
	cancel    context.CancelFunc
	writeMu   sync.Mutex
	closeOnce sync.Once
}

type status struct {
	Mode       string `json:"mode"`
	Ready      bool   `json:"ready"`
	Connected  bool   `json:"connected"`
	AgentID    string `json:"agent_id"`
	Reconnects int64  `json:"reconnects"`
}

func New(cfg *config.Config) (*Agent, error) {
	if cfg == nil || cfg.Role != config.RoleAgent || cfg.Agent == nil {
		return nil, errors.New("agent requires agent configuration")
	}
	if err := transport.ValidateAgentCredentials(cfg); err != nil {
		return nil, err
	}
	r, err := routing.New(cfg.Agent.Proxy)
	if err != nil {
		return nil, fmt.Errorf("load routing database: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{
		cfg:      cfg,
		router:   r,
		ctx:      ctx,
		cancel:   cancel,
		metrics:  &observability.Metrics{},
		ready:    make(chan struct{}),
		mappings: map[string]config.Tunnel{},
	}
	for _, tunnel := range cfg.Agent.Reverse {
		a.mappings[tunnel.Name] = tunnel
	}
	return a, nil
}

func (a *Agent) Start() error {
	for _, in := range a.cfg.Agent.Proxy.Inbounds {
		listener, err := a.startInbound(in)
		if err != nil {
			a.closeListeners()
			return fmt.Errorf("start proxy %s: %w", in.Tag, err)
		}
		a.listenerMu.Lock()
		a.listeners = append(a.listeners, listener)
		a.listenerMu.Unlock()
	}
	mgmt, err := observability.Start(a.cfg.Management.Listen, a.metrics, a.Status, a.IsReady)
	if err != nil {
		a.closeListeners()
		return err
	}
	a.mgmt = mgmt
	go a.connectLoop()
	return nil
}

func (a *Agent) Close() error {
	if a == nil {
		return nil
	}
	a.cancel()
	a.closeListeners()
	if a.mgmt != nil {
		_ = a.mgmt.Close()
	}
	a.mu.Lock()
	sess := a.session
	a.session = nil
	a.ready = make(chan struct{})
	a.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
	return a.router.Close()
}

func (a *Agent) closeListeners() {
	a.listenerMu.Lock()
	listeners := append([]net.Listener(nil), a.listeners...)
	a.listeners = nil
	a.listenerMu.Unlock()
	for _, listener := range listeners {
		_ = listener.Close()
	}
}

func (a *Agent) IsReady() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.session != nil
}

func (a *Agent) Status() any {
	a.mu.RLock()
	connected := a.session != nil
	a.mu.RUnlock()
	return status{Mode: config.RoleAgent, Ready: connected, Connected: connected, AgentID: a.cfg.Agent.ID, Reconnects: a.connects.Load()}
}

func (a *Agent) connectLoop() {
	backoff := time.Second
	for {
		if a.ctx.Err() != nil {
			return
		}
		a.connects.Add(1)
		ep, conn, sess, err := a.connectOnce()
		if err == nil {
			if a.ctx.Err() != nil {
				sess.Close()
				_ = conn.Close()
				_ = ep.Close(context.Background())
				return
			}
			backoff = time.Second
			if !a.setSession(sess) {
				_ = conn.Close()
				_ = ep.Close(context.Background())
				return
			}
			if waitErr := sess.Wait(); waitErr != nil && a.ctx.Err() == nil {
				log.Printf("agent QUIC session ended: %v", waitErr)
			}
			a.clearSession(sess)
			_ = conn.Close()
			_ = ep.Close(context.Background())
			continue
		}
		if a.ctx.Err() != nil {
			return
		}
		log.Printf("agent gateway connection failed: %v", err)
		timer := time.NewTimer(backoff + time.Duration(a.connects.Load()%5)*100*time.Millisecond)
		select {
		case <-timer.C:
		case <-a.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (a *Agent) connectOnce() (*quic.Endpoint, *quic.Conn, *Session, error) {
	ep, conn, err := transport.Dial(a.ctx, a.cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	stream, err := conn.NewStream(a.ctx)
	if err != nil {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		return nil, nil, nil, err
	}
	handshakeCtx, cancelHandshake := context.WithTimeout(a.ctx, time.Duration(a.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	defer cancelHandshake()
	stream.SetReadContext(handshakeCtx)
	stream.SetWriteContext(handshakeCtx)
	max := a.cfg.Limits.MaxFrameBytes
	hello, _ := transport.JSONFrame(transport.TypeHello, 1, transport.Hello{AgentID: a.cfg.Agent.ID})
	if err = writeFrame(stream, hello, max); err != nil {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		return nil, nil, nil, err
	}
	f, err := transport.ReadFrame(stream, max)
	if err != nil || f.Type != transport.TypeChallenge {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		return nil, nil, nil, errors.New("gateway did not issue challenge")
	}
	var challenge transport.Challenge
	if err = transport.DecodeJSON(f, &challenge); err != nil {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		return nil, nil, nil, err
	}
	token, err := config.ReadToken(a.cfg.Agent.TokenFile)
	if err != nil {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		return nil, nil, nil, err
	}
	auth, _ := transport.JSONFrame(transport.TypeAuth, 1, transport.Auth{MAC: transport.SignChallenge(token, challenge.Nonce, a.cfg.Agent.ID)})
	if err = writeFrame(stream, auth, max); err != nil {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		return nil, nil, nil, err
	}
	f, err = transport.ReadFrame(stream, max)
	if err != nil || f.Type != transport.TypeAuthOK {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		return nil, nil, nil, errors.New("gateway rejected authentication")
	}
	var result transport.AuthResult
	if err = transport.DecodeJSON(f, &result); err != nil || result.Error != "" {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		if err == nil {
			err = errors.New(result.Error)
		}
		return nil, nil, nil, err
	}
	regs := make([]transport.TunnelRegistration, 0, len(a.cfg.Agent.Reverse))
	for _, t := range a.cfg.Agent.Reverse {
		regs = append(regs, transport.TunnelRegistration{Name: t.Name, Protocol: t.Protocol, GatewayPort: t.GatewayPort, Profile: a.profile(a.cfg.Obfuscation.ReverseProfile)})
	}
	reg, _ := transport.JSONFrame(transport.TypeRegister, 2, transport.Register{Mappings: regs})
	if err = writeFrame(stream, reg, max); err != nil {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		return nil, nil, nil, err
	}
	f, err = transport.ReadFrame(stream, max)
	if err != nil || f.Type != transport.TypeRegisterResult {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		return nil, nil, nil, errors.New("gateway did not acknowledge mappings")
	}
	var registerResult transport.RegisterResult
	if err = transport.DecodeJSON(f, &registerResult); err != nil || registerResult.Error != "" {
		_ = conn.Close()
		_ = ep.Close(context.Background())
		if err == nil {
			err = errors.New(registerResult.Error)
		}
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithCancel(a.ctx)
	cancelHandshake()
	stream.SetReadContext(ctx)
	stream.SetWriteContext(ctx)
	sess := &Session{agent: a, conn: conn, control: stream, ctx: ctx, cancel: cancel}
	go sess.controlLoop()
	go sess.acceptReverseLoop()
	return ep, conn, sess, nil
}

func (a *Agent) setSession(sess *Session) bool {
	a.mu.Lock()
	if a.ctx.Err() != nil {
		a.mu.Unlock()
		sess.Close()
		return false
	}
	if a.session != nil {
		a.session.Close()
	}
	a.session = sess
	close(a.ready)
	a.mu.Unlock()
	return true
}

func (a *Agent) clearSession(sess *Session) {
	a.mu.Lock()
	if a.session == sess {
		a.session = nil
		a.ready = make(chan struct{})
	}
	a.mu.Unlock()
}

func (a *Agent) waitSession(ctx context.Context) (*Session, error) {
	for {
		a.mu.RLock()
		sess, ready := a.session, a.ready
		a.mu.RUnlock()
		if sess != nil {
			return sess, nil
		}
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-a.ctx.Done():
			return nil, a.ctx.Err()
		}
	}
}

func (s *Session) Wait() error { return s.conn.Wait(s.ctx) }

func (s *Session) Close() {
	s.closeOnce.Do(func() { log.Printf("agent session closing"); s.cancel(); _ = s.conn.Close() })
}

func (s *Session) controlLoop() {
	defer s.Close()
	for {
		f, err := transport.ReadFrame(s.control, s.agent.cfg.Limits.MaxFrameBytes)
		if err != nil {
			if s.agent.ctx.Err() == nil {
				log.Printf("agent control loop ended: %v", err)
			}
			return
		}
		if f.Type == transport.TypePing {
			pong, _ := transport.JSONFrame(transport.TypePong, f.RequestID, nil)
			_ = s.write(pong)
		}
	}
}

func (s *Session) write(f transport.Frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeFrame(s.control, f, s.agent.cfg.Limits.MaxFrameBytes)
}

func (s *Session) acceptReverseLoop() {
	for {
		stream, err := s.conn.AcceptStream(s.ctx)
		if err != nil {
			if s.agent.ctx.Err() == nil {
				log.Printf("agent reverse accept ended: %v", err)
			}
			return
		}
		go s.handleReverse(stream)
	}
}

func (s *Session) handleReverse(stream *quic.Stream) {
	defer stream.Close()
	handshakeCtx, cancelHandshake := context.WithTimeout(s.ctx, time.Duration(s.agent.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stream.SetReadContext(handshakeCtx)
	f, err := transport.ReadFrame(stream, s.agent.cfg.Limits.MaxFrameBytes)
	cancelHandshake()
	stream.SetReadContext(s.ctx)
	if err != nil {
		log.Printf("agent reverse open read: %v", err)
		return
	}
	if f.Type != transport.TypeOpenReverse {
		return
	}
	var open transport.OpenReverse
	if err := transport.DecodeJSON(f, &open); err != nil {
		return
	}
	tunnel, ok := s.agent.mappings[open.Name]
	if !ok || tunnel.Protocol != open.Protocol || open.Profile != s.agent.profile(s.agent.cfg.Obfuscation.ReverseProfile) {
		log.Printf("agent rejected reverse mapping %s", open.Name)
		sendOpenError(stream, f.RequestID, "unknown or invalid mapping", s.agent.cfg.Limits.MaxFrameBytes)
		return
	}
	if open.Protocol == "tcp" {
		s.reverseTCP(stream, tunnel, open.Profile)
		return
	}
	s.reverseUDP(stream, tunnel)
}

func (s *Session) reverseTCP(stream *quic.Stream, tunnel config.Tunnel, profile string) {
	dialer := &net.Dialer{Timeout: time.Duration(s.agent.cfg.Limits.DialTimeoutSec) * time.Second}
	conn, err := dialer.DialContext(s.ctx, "tcp", tunnel.Local)
	if err != nil {
		log.Printf("agent reverse local dial %s: %v", tunnel.Local, err)
		sendOpenError(stream, 0, err.Error(), s.agent.cfg.Limits.MaxFrameBytes)
		return
	}
	defer conn.Close()
	ok, _ := transport.JSONFrame(transport.TypeOpenOK, 0, transport.OpenResult{})
	if err := writeFrame(stream, ok, s.agent.cfg.Limits.MaxFrameBytes); err != nil {
		log.Printf("agent reverse status write: %v", err)
		return
	}
	remote := relay.NewConn(stream, s.agent.relayProfile(profile))
	s.agent.metrics.ActiveStreams.Add(1)
	defer s.agent.metrics.ActiveStreams.Add(-1)
	relay.Bidirectional(conn, remote, relay.Counters{In: func(n uint64) { s.agent.metrics.BytesIn.Add(n) }, Out: func(n uint64) { s.agent.metrics.BytesOut.Add(n) }})
}

func (s *Session) reverseUDP(stream *quic.Stream, tunnel config.Tunnel) {
	local, err := net.ResolveUDPAddr("udp", tunnel.Local)
	if err != nil {
		sendOpenError(stream, 0, err.Error(), s.agent.cfg.Limits.MaxFrameBytes)
		return
	}
	conn, err := net.DialUDP("udp", nil, local)
	if err != nil {
		sendOpenError(stream, 0, err.Error(), s.agent.cfg.Limits.MaxFrameBytes)
		return
	}
	defer conn.Close()
	ok, _ := transport.JSONFrame(transport.TypeOpenOK, 0, transport.OpenResult{})
	if err := writeFrame(stream, ok, s.agent.cfg.Limits.MaxFrameBytes); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	touch, stopIdle := relay.StartIdleWatch(ctx, time.Duration(s.agent.cfg.Limits.UDPIdleTimeoutSec)*time.Second, func() {
		_ = stream.Close()
		_ = conn.Close()
	})
	defer stopIdle()
	errCh := make(chan error, 2)
	go func() {
		for {
			f, err := transport.ReadFrame(stream, s.agent.cfg.Limits.MaxFrameBytes)
			if err != nil {
				errCh <- err
				return
			}
			if f.Type != transport.TypeData {
				errCh <- errors.New("unexpected reverse UDP frame")
				return
			}
			d, err := transport.DecodeData(f, s.agent.cfg.Limits.MaxUDPBytes, s.agent.cfg.Obfuscation.MaxPaddingBytes)
			if err != nil {
				errCh <- err
				return
			}
			if _, err := conn.Write(d.Payload); err != nil {
				errCh <- err
				return
			}
			touch()
			s.agent.metrics.BytesIn.Add(uint64(len(d.Payload)))
		}
	}()
	go func() {
		buf := make([]byte, s.agent.cfg.Limits.MaxUDPBytes)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			n, err := conn.Read(buf)
			if n > 0 {
				touch()
				f, _ := transport.JSONFrame(transport.TypeData, 0, transport.NewData(buf[:n], s.agent.profile(s.agent.cfg.Obfuscation.ReverseProfile), s.agent.cfg.Obfuscation.MaxPaddingBytes))
				if err := writeFrame(stream, f, s.agent.cfg.Limits.MaxFrameBytes); err != nil {
					errCh <- err
					return
				}
				s.agent.metrics.BytesOut.Add(uint64(n))
			}
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					select {
					case <-ctx.Done():
						errCh <- ctx.Err()
						return
					default:
						continue
					}
				}
				errCh <- err
				return
			}
		}
	}()
	<-errCh
}

func (a *Agent) OpenProxy(ctx context.Context, network, address string, port uint16) (*quic.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sess, err := a.waitSession(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := sess.conn.NewStream(ctx)
	if err != nil {
		return nil, err
	}
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, time.Duration(a.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	defer cancelHandshake()
	stream.SetReadContext(handshakeCtx)
	stream.SetWriteContext(handshakeCtx)
	open, _ := transport.JSONFrame(transport.TypeOpenProxy, 0, transport.OpenProxy{Network: network, Address: address, Port: port, Profile: a.profile(a.cfg.Obfuscation.ProxyProfile)})
	if err := writeFrame(stream, open, a.cfg.Limits.MaxFrameBytes); err != nil {
		_ = stream.Close()
		return nil, err
	}
	f, err := transport.ReadFrame(stream, a.cfg.Limits.MaxFrameBytes)
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	if f.Type != transport.TypeOpenOK {
		var failure transport.OpenResult
		_ = transport.DecodeJSON(f, &failure)
		_ = stream.Close()
		if failure.Error == "" {
			failure.Error = "remote open failed"
		}
		return nil, errors.New(failure.Error)
	}
	cancelHandshake()
	stream.SetReadContext(ctx)
	stream.SetWriteContext(ctx)
	return stream, nil
}

func (a *Agent) route(inbound, host string) string {
	ctx, cancel := context.WithTimeout(a.ctx, time.Duration(a.cfg.Limits.DialTimeoutSec)*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return config.RouteGateway
	}
	return a.router.Choose(inbound, host, ips[0])
}

func (a *Agent) relayProfile(name string) relay.Profile {
	p, err := relay.NewProfile(a.profile(name), a.cfg.Limits.MaxRecordBytes, a.cfg.Obfuscation.MaxPaddingBytes)
	if err != nil {
		p, _ = relay.NewProfile(config.ProfileStandard, a.cfg.Limits.MaxRecordBytes, 0)
	}
	return p
}

func (a *Agent) profile(name string) string {
	return name
}

func writeFrame(stream *quic.Stream, f transport.Frame, max int64) error {
	if err := transport.WriteFrame(stream, f, max); err != nil {
		return err
	}
	stream.Flush()
	return nil
}

func sendOpenError(stream *quic.Stream, id uint64, message string, max int64) {
	f, _ := transport.JSONFrame(transport.TypeOpenError, id, transport.OpenResult{Error: message})
	_ = writeFrame(stream, f, max)
}
