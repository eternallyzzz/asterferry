package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/lifecycle"
	"asterferry/internal/observability"
	"asterferry/internal/proxy"
	"asterferry/internal/relay"
	"asterferry/internal/routing"
	"asterferry/internal/transport"
)

type Agent struct {
	cfg      *config.AgentOptions
	router   *routing.Router
	ctx      context.Context
	cancel   context.CancelFunc
	metrics  *observability.Metrics
	mgmt     *observability.Server
	logger   *slog.Logger
	outbound proxy.Outbound
	proxy    *ProxyEngine

	sessions  *sessionManager
	mappings  map[string]config.Tunnel
	closeOnce sync.Once
	closeErr  error
}

type Session struct {
	agent     *Agent
	conn      transport.Session
	control   transport.Stream
	ctx       context.Context
	cancel    context.CancelFunc
	writeMu   sync.Mutex
	closeOnce sync.Once
	streamSem chan struct{}
}

type status struct {
	Mode                        string `json:"mode"`
	Ready                       bool   `json:"ready"`
	Connected                   bool   `json:"connected"`
	AgentID                     string `json:"agent_id"`
	Reconnects                  int64  `json:"reconnects"`
	TransportObfuscationMode    string `json:"transport_obfuscation_mode"`
	TransportObfuscationKeyHash string `json:"transport_obfuscation_key_fingerprint"`
}

func New(cfg *config.AgentOptions, loggerOpt ...*slog.Logger) (*Agent, error) {
	if cfg == nil {
		return nil, errors.New("agent requires agent configuration")
	}
	if err := transport.ValidateAgentCredentials(cfg); err != nil {
		return nil, err
	}
	r, err := routing.NewOptions(cfg.Agent.Proxy)
	if err != nil {
		return nil, fmt.Errorf("load routing database: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.Default()
	if len(loggerOpt) > 0 && loggerOpt[0] != nil {
		logger = loggerOpt[0]
	}
	a := &Agent{
		cfg:      cfg,
		router:   r,
		ctx:      ctx,
		cancel:   cancel,
		metrics:  &observability.Metrics{},
		logger:   logger,
		mappings: map[string]config.Tunnel{},
	}
	a.outbound = agentOutbound{agent: a}
	a.sessions = newSessionManager(ctx, a.connectOnce, logger)
	engine, err := NewProxyEngine(ProxyEngineOptions{Inbounds: cfg.Agent.Proxy.Inbounds, Handler: a.handleInbound})
	if err != nil {
		cancel()
		_ = r.Close()
		return nil, err
	}
	a.proxy = engine
	for _, tunnel := range cfg.Agent.Reverse {
		a.mappings[tunnel.Name] = tunnel
	}
	return a, nil
}

func (a *Agent) Start() error {
	if err := a.proxy.Start(a.ctx); err != nil {
		return err
	}
	mgmt, err := observability.Start(a.cfg.Management.Listen, a.metrics, a)
	if err != nil {
		_ = a.proxy.Close()
		return err
	}
	a.mgmt = mgmt
	a.sessions.Start()
	return nil
}

func (a *Agent) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		a.cancel()
		if a.proxy != nil {
			_ = a.proxy.Close()
		}
		if a.mgmt != nil {
			_ = a.mgmt.Close()
		}
		if a.sessions != nil {
			_ = a.sessions.Close()
		}
		if a.router != nil {
			a.closeErr = a.router.Close()
		}
	})
	return a.closeErr
}

func (a *Agent) handleInbound(conn net.Conn, in config.Inbound) {
	if in.Protocol == "socks5" {
		a.handleSOCKS(conn, in)
		return
	}
	a.handleHTTP(conn, in)
}

func (a *Agent) IsReady() bool {
	return a.sessions != nil && a.sessions.IsReady()
}

func (a *Agent) Status() any {
	connected := a.IsReady()
	var reconnects int64
	if a.sessions != nil {
		reconnects = a.sessions.Reconnects()
	}
	keyHash := ""
	if len(a.cfg.TransportObfuscation.CurrentKey) > 0 {
		keyHash = config.TokenFingerprint(a.cfg.TransportObfuscation.CurrentKey)
	}
	return status{
		Mode:                        config.RoleAgent,
		Ready:                       connected,
		Connected:                   connected,
		AgentID:                     a.cfg.Agent.ID,
		Reconnects:                  reconnects,
		TransportObfuscationMode:    a.cfg.TransportObfuscation.Mode,
		TransportObfuscationKeyHash: keyHash,
	}
}

func (a *Agent) connectOnce(ctx context.Context) (transport.Session, *Session, error) {
	conn, err := transport.Dial(ctx, a.cfg, a.metrics)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (transport.Session, *Session, error) {
		_ = transport.CloseSession(conn)
		return nil, nil, err
	}
	stream, err := conn.OpenStream(ctx)
	if err != nil {
		return fail(err)
	}
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, time.Duration(a.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	defer cancelHandshake()
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	defer stopStreamContext()
	max := a.cfg.Limits.MaxFrameBytes
	hello, _ := transport.JSONFrame(transport.TypeHello, 1, transport.Hello{AgentID: a.cfg.Agent.ID})
	if err = writeFrame(stream, hello, max); err != nil {
		return fail(err)
	}
	f, err := transport.ReadFrame(stream, max)
	if err != nil || f.Type != transport.TypeChallenge {
		return fail(errors.New("gateway did not issue challenge"))
	}
	var challenge transport.Challenge
	if err = transport.DecodeJSON(f, &challenge); err != nil {
		return fail(err)
	}
	auth, _ := transport.JSONFrame(transport.TypeAuth, 1, transport.Auth{MAC: transport.SignChallenge(a.cfg.Token, challenge.Nonce, a.cfg.Agent.ID)})
	if err = writeFrame(stream, auth, max); err != nil {
		return fail(err)
	}
	f, err = transport.ReadFrame(stream, max)
	if err != nil || f.Type != transport.TypeAuthOK {
		return fail(errors.New("gateway rejected authentication"))
	}
	var result transport.AuthResult
	if err = transport.DecodeJSON(f, &result); err != nil || result.Error != "" {
		if err == nil {
			err = errors.New(result.Error)
		}
		return fail(err)
	}
	regs := make([]transport.TunnelRegistration, 0, len(a.cfg.Agent.Reverse))
	for _, t := range a.cfg.Agent.Reverse {
		regs = append(regs, transport.TunnelRegistration{Name: t.Name, Protocol: t.Protocol, GatewayPort: t.GatewayPort, Profile: a.profile(a.cfg.Obfuscation.ReverseProfile)})
	}
	reg, _ := transport.JSONFrame(transport.TypeRegister, 2, transport.Register{Mappings: regs})
	if err = writeFrame(stream, reg, max); err != nil {
		return fail(err)
	}
	f, err = transport.ReadFrame(stream, max)
	if err != nil || f.Type != transport.TypeRegisterResult {
		return fail(errors.New("gateway did not acknowledge mappings"))
	}
	var registerResult transport.RegisterResult
	if err = transport.DecodeJSON(f, &registerResult); err != nil || registerResult.Error != "" {
		if err == nil {
			err = errors.New(registerResult.Error)
		}
		return fail(err)
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	cancelHandshake()
	stopStreamContext()
	stopStreamContext = transport.SetStreamContext(stream, sessionCtx)
	streamLimit := a.cfg.StreamLimit
	if streamLimit <= 0 {
		streamLimit = a.cfg.Limits.MaxStreamsPerAgent
	}
	sess := &Session{agent: a, conn: conn, control: stream, ctx: sessionCtx, cancel: cancel, streamSem: make(chan struct{}, streamLimit)}
	go sess.controlLoop()
	go sess.acceptReverseLoop()
	return conn, sess, nil
}

func (s *Session) Wait() error { return transport.WaitConn(s.ctx, s.conn) }

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.agent.logger.Info("session closing", "event", "agent.session.closing")
		s.cancel()
		_ = transport.CloseSession(s.conn)
	})
}

func (s *Session) controlLoop() {
	defer s.Close()
	for {
		f, err := transport.ReadFrame(s.control, s.agent.cfg.Limits.MaxFrameBytes)
		if err != nil {
			if s.agent.ctx.Err() == nil {
				s.agent.logger.Info("control loop ended", "event", "agent.control.ended", "error_kind", agentErrorKind(err))
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
				s.agent.logger.Info("reverse accept ended", "event", "agent.reverse.accept_ended", "error_kind", agentErrorKind(err))
			}
			return
		}
		release, ok := s.tryAcquireStream()
		if !ok {
			stream.Cancel()
			_ = stream.Close()
			continue
		}
		go s.handleReverse(stream, release)
	}
}

func (s *Session) handleReverse(stream transport.Stream, release func()) {
	defer stream.Close()
	defer release()
	handshakeCtx, cancelHandshake := context.WithTimeout(s.ctx, time.Duration(s.agent.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	f, err := transport.ReadFrame(stream, s.agent.cfg.Limits.MaxFrameBytes)
	cancelHandshake()
	stopStreamContext()
	stopStreamContext = transport.SetStreamContext(stream, s.ctx)
	defer stopStreamContext()
	if err != nil {
		s.agent.logger.Info("reverse open read failed", "event", "agent.reverse.open_read_failed", "error_kind", agentErrorKind(err))
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
		s.agent.logger.Warn("reverse mapping rejected", "event", "agent.reverse.rejected", "mapping", open.Name, "security_audit", true)
		sendOpenError(stream, f.RequestID, "unknown or invalid mapping", s.agent.cfg.Limits.MaxFrameBytes)
		return
	}
	if open.Protocol == "tcp" {
		s.reverseTCP(stream, tunnel, open.Profile)
		return
	}
	s.reverseUDP(stream, tunnel)
}

func (s *Session) reverseTCP(stream transport.Stream, tunnel config.Tunnel, profile string) {
	dialer := &net.Dialer{Timeout: time.Duration(s.agent.cfg.Limits.DialTimeoutSec) * time.Second}
	conn, err := dialer.DialContext(s.ctx, "tcp", tunnel.Local)
	if err != nil {
		s.agent.logger.Info("reverse local dial failed", "event", "agent.reverse.local_dial_failed", "mapping", tunnel.Name, "error_kind", agentErrorKind(err))
		sendOpenError(stream, 0, err.Error(), s.agent.cfg.Limits.MaxFrameBytes)
		return
	}
	defer conn.Close()
	ok, _ := transport.JSONFrame(transport.TypeOpenOK, 0, transport.OpenResult{})
	if err := writeFrame(stream, ok, s.agent.cfg.Limits.MaxFrameBytes); err != nil {
		s.agent.logger.Info("reverse status write failed", "event", "agent.reverse.status_write_failed", "mapping", tunnel.Name, "error_kind", agentErrorKind(err))
		return
	}
	remote := relay.NewConn(stream, s.agent.relayProfile(profile))
	s.agent.metrics.ActiveStreams.Add(1)
	defer s.agent.metrics.ActiveStreams.Add(-1)
	relay.Bidirectional(conn, remote, relay.Counters{In: func(n uint64) { s.agent.metrics.BytesIn.Add(n) }, Out: func(n uint64) { s.agent.metrics.BytesOut.Add(n) }})
}

func (s *Session) reverseUDP(stream transport.Stream, tunnel config.Tunnel) {
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

func (a *Agent) OpenProxy(ctx context.Context, network, address string, port uint16) (transport.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sess, err := a.sessions.wait(ctx)
	if err != nil {
		return nil, err
	}
	release, ok := sess.acquireStream(ctx)
	if !ok {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("agent stream limit exceeded")
	}
	stream, err := sess.conn.OpenStream(ctx)
	if err != nil {
		release()
		return nil, err
	}
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, time.Duration(a.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	defer cancelHandshake()
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	defer stopStreamContext()
	open, _ := transport.JSONFrame(transport.TypeOpenProxy, 0, transport.OpenProxy{Network: network, Address: address, Port: port, Profile: a.profile(a.cfg.Obfuscation.ProxyProfile)})
	if err := writeFrame(stream, open, a.cfg.Limits.MaxFrameBytes); err != nil {
		_ = stream.Close()
		release()
		return nil, err
	}
	f, err := transport.ReadFrame(stream, a.cfg.Limits.MaxFrameBytes)
	if err != nil {
		_ = stream.Close()
		release()
		return nil, err
	}
	if f.Type != transport.TypeOpenOK {
		var failure transport.OpenResult
		_ = transport.DecodeJSON(f, &failure)
		_ = stream.Close()
		release()
		if failure.Error == "" {
			failure.Error = "remote open failed"
		}
		return nil, errors.New(failure.Error)
	}
	cancelHandshake()
	stopStreamContext()
	stopStreamContext = transport.SetStreamContext(stream, ctx)
	return newLeasedStream(stream, release), nil
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

func writeFrame(stream io.Writer, f transport.Frame, max int64) error {
	if err := transport.WriteFrame(stream, f, max); err != nil {
		return err
	}
	return nil
}

func sendOpenError(stream io.Writer, id uint64, message string, max int64) {
	f, _ := transport.JSONFrame(transport.TypeOpenError, id, transport.OpenResult{Error: message})
	_ = writeFrame(stream, f, max)
}

func agentErrorKind(err error) string {
	return lifecycle.ErrorKind(err)
}
