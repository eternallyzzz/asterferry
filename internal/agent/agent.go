package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"asterferry/internal/cluster"
	"asterferry/internal/config"
	"asterferry/internal/dashboard"
	"asterferry/internal/lifecycle"
	"asterferry/internal/observability"
	"asterferry/internal/proxy"
	"asterferry/internal/relay"
	"asterferry/internal/routing"
	"asterferry/internal/transport"
)

type Agent struct {
	cfg             *config.AgentOptions
	nodeID          string
	router          *routing.Router
	ctx             context.Context
	cancel          context.CancelFunc
	metrics         *observability.Metrics
	mgmt            *observability.Server
	logger          *slog.Logger
	outbound        proxy.Outbound
	proxy           *ProxyEngine
	life            *lifecycle.Gate
	events          *observability.EventHub
	shutdownTrigger *lifecycle.ShutdownTrigger

	sessions  *sessionManager
	mappings  map[string]config.Tunnel
	closeOnce sync.Once
	closeErr  error
}

type Session struct {
	agent     *Agent
	conn      transport.Session
	sessionID string
	control   transport.Stream
	caps      []transport.Capability
	limits    transport.Limits
	ctx       context.Context
	cancel    context.CancelFunc
	writeMu   sync.Mutex
	closeOnce sync.Once
	streamSem chan struct{}
}

type status struct {
	Mode                        string `json:"mode"`
	State                       string `json:"state"`
	Ready                       bool   `json:"ready"`
	Connected                   bool   `json:"connected"`
	NodeID                      string `json:"node_id"`
	SessionID                   string `json:"session_id,omitempty"`
	AgentID                     string `json:"agent_id"`
	Reconnects                  int64  `json:"reconnects"`
	TransportObfuscationMode    string `json:"transport_obfuscation_mode"`
	TransportObfuscationKeyHash string `json:"transport_obfuscation_key_fingerprint"`
}

type RuntimeOptions struct {
	Logger          *slog.Logger
	Events          *observability.EventHub
	ShutdownTrigger *lifecycle.ShutdownTrigger
}

func New(cfg *config.AgentOptions, loggerOpt ...*slog.Logger) (*Agent, error) {
	var runtime RuntimeOptions
	if len(loggerOpt) > 0 {
		runtime.Logger = loggerOpt[0]
	}
	return NewWithOptions(cfg, runtime)
}

func NewWithOptions(cfg *config.AgentOptions, runtime RuntimeOptions) (*Agent, error) {
	if cfg == nil {
		return nil, errors.New("agent requires agent configuration")
	}
	if err := transport.ValidateAgentCredentials(cfg); err != nil {
		return nil, err
	}
	nodeID, err := cluster.ResolveNodeID(cfg.Cluster.NodeID)
	if err != nil {
		return nil, err
	}
	r, err := routing.NewOptions(cfg.Agent.Proxy)
	if err != nil {
		return nil, fmt.Errorf("load routing database: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger := runtime.Logger
	if logger == nil {
		logger = slog.Default()
	}
	a := &Agent{
		cfg:             cfg,
		nodeID:          nodeID,
		router:          r,
		ctx:             ctx,
		cancel:          cancel,
		metrics:         &observability.Metrics{},
		logger:          logger,
		life:            lifecycle.NewGate(),
		events:          runtime.Events,
		shutdownTrigger: runtime.ShutdownTrigger,
		mappings:        map[string]config.Tunnel{},
	}
	a.outbound = agentOutbound{agent: a}
	a.sessions = newSessionManager(ctx, a.connectOnce, logger)
	engine, err := NewProxyEngine(ProxyEngineOptions{Inbounds: cfg.Agent.Proxy.Inbounds, Handler: a.handleInbound, Gate: a.life, MaxConnections: cfg.Limits.MaxInboundConnections})
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
	mgmt, err := observability.Start(a.cfg.Management.Listen, a.metrics, a, a.cfg.Management.AuthToken, observability.ServerOptions{
		Events:    a.events,
		Actions:   a,
		Dashboard: dashboard.Handler(),
		Logger:    a.logger,
	})
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
		a.life.Stop()
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

// Shutdown enters draining, stops new local work and reconnect attempts, and
// waits for admitted handlers until ctx expires. The final Close is always
// performed so callers never leave a half-shutdown Agent behind.
func (a *Agent) Shutdown(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	started := a.life.BeginDrain()
	if !started && a.life.State() == lifecycle.StateStopped {
		return nil
	}
	if started {
		a.metrics.BeginDrain()
		a.logger.Warn("agent draining", "event", "runtime.draining", "role", config.RoleAgent, "node_id", a.nodeID, "grace_period", a.cfg.Shutdown.GracePeriod.String())
		if a.proxy != nil {
			_ = a.proxy.BeginDrain()
		}
		if a.sessions != nil {
			a.sessions.BeginDrain()
		}
	}
	err := a.life.Wait(ctx)
	forced := err != nil
	if forced {
		a.logger.Warn("agent drain timed out", "event", "runtime.drain_timeout", "role", config.RoleAgent, "node_id", a.nodeID, "active_work", a.life.Active(), "error_kind", lifecycle.ErrorKind(err))
	} else if started {
		a.logger.Info("agent drain complete", "event", "runtime.drained", "role", config.RoleAgent, "node_id", a.nodeID)
	}
	if started {
		a.metrics.RecordShutdown(forced)
	}
	if a.shutdownTrigger != nil && a.shutdownTrigger.Requested() {
		a.logger.Info("dashboard shutdown completed", "event", "management.action.completed", "action", "shutdown", "security_audit", true)
	}
	closeErr := a.Close()
	if closeErr != nil {
		return closeErr
	}
	return err
}

func (a *Agent) handleInbound(conn net.Conn, in config.Inbound) {
	if in.Protocol == "socks5" {
		a.handleSOCKS(conn, in)
		return
	}
	a.handleHTTP(conn, in)
}

func (a *Agent) IsReady() bool {
	return a.life != nil && a.life.IsRunning() && a.sessions != nil && a.sessions.IsReady()
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
		State:                       a.life.State().String(),
		Ready:                       connected,
		Connected:                   connected,
		NodeID:                      a.nodeID,
		SessionID:                   a.currentSessionID(),
		AgentID:                     a.cfg.Agent.ID,
		Reconnects:                  reconnects,
		TransportObfuscationMode:    a.cfg.TransportObfuscation.Mode,
		TransportObfuscationKeyHash: keyHash,
	}
}

func (a *Agent) Dashboard() observability.DashboardSnapshot {
	if a == nil || a.cfg == nil {
		return observability.DashboardSnapshot{SchemaVersion: observability.DashboardSchemaVersion, Role: config.RoleAgent}
	}
	state := "stopped"
	ready := false
	if a.life != nil {
		state = a.life.State().String()
		ready = a.IsReady()
	}
	inbounds := make([]observability.AgentInboundSnapshot, 0, len(a.cfg.Agent.Proxy.Inbounds))
	for _, inbound := range a.cfg.Agent.Proxy.Inbounds {
		inbounds = append(inbounds, observability.AgentInboundSnapshot{Tag: inbound.Tag, Protocol: inbound.Protocol, Listen: inbound.Listen})
	}
	reverse := make([]observability.AgentReverseSnapshot, 0, len(a.cfg.Agent.Reverse))
	for _, tunnel := range a.cfg.Agent.Reverse {
		reverse = append(reverse, observability.AgentReverseSnapshot{Name: tunnel.Name, Protocol: tunnel.Protocol, GatewayPort: tunnel.GatewayPort, Local: tunnel.Local})
	}
	keyFingerprint := ""
	if len(a.cfg.TransportObfuscation.CurrentKey) > 0 {
		keyFingerprint = config.TokenFingerprint(a.cfg.TransportObfuscation.CurrentKey)
	}
	return observability.DashboardSnapshot{
		SchemaVersion: observability.DashboardSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Role:          config.RoleAgent,
		State:         state,
		Ready:         ready,
		NodeID:        a.nodeID,
		Transport: observability.DashboardTransportSnapshot{
			Protocol:        config.ConfigVersion,
			ObfuscationMode: a.cfg.TransportObfuscation.Mode,
			KeyFingerprint:  keyFingerprint,
		},
		Agent: &observability.AgentDashboardSnapshot{
			AgentID:         a.cfg.Agent.ID,
			Connected:       a.IsReady(),
			SessionID:       a.currentSessionID(),
			Reconnects:      a.sessions.Reconnects(),
			Inbounds:        inbounds,
			ReverseMappings: reverse,
		},
	}
}

func (a *Agent) RequestShutdown() error {
	if err := a.CanShutdown(); err != nil {
		return err
	}
	a.TriggerShutdown()
	return nil
}

func (a *Agent) CanShutdown() error {
	if a == nil || a.life == nil || !a.life.IsRunning() || a.shutdownTrigger == nil {
		return observability.ErrActionUnavailable
	}
	if a.shutdownTrigger.Requested() {
		return observability.ErrActionBusy
	}
	return nil
}

func (a *Agent) TriggerShutdown() {
	if a == nil || a.shutdownTrigger == nil {
		return
	}
	a.logger.Warn("dashboard requested shutdown", "event", "management.action.requested", "action", "shutdown", "security_audit", true)
	_ = a.shutdownTrigger.Request()
}

func (a *Agent) RequestReconnect() error {
	if a == nil || a.life == nil || !a.life.IsRunning() || a.sessions == nil {
		return observability.ErrActionUnavailable
	}
	if err := a.sessions.RequestReconnect(); err != nil {
		return err
	}
	a.logger.Warn("dashboard requested reconnect", "event", "management.action.requested", "action", "reconnect", "agent_id", a.cfg.Agent.ID, "security_audit", true)
	return nil
}

func (a *Agent) currentSessionID() string {
	if a == nil || a.sessions == nil {
		return ""
	}
	a.sessions.mu.RLock()
	sess := a.sessions.session
	a.sessions.mu.RUnlock()
	if sess == nil {
		return ""
	}
	return sess.sessionID
}

func (a *Agent) connectOnce(ctx context.Context) (transport.Session, *Session, error) {
	attemptCtx, cancelAttempt := context.WithTimeout(ctx, time.Duration(a.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	defer cancelAttempt()
	conn, err := transport.Dial(attemptCtx, a.cfg, a.metrics)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (transport.Session, *Session, error) {
		_ = transport.CloseSession(conn)
		return nil, nil, err
	}
	stream, err := conn.OpenStream(attemptCtx)
	if err != nil {
		return fail(err)
	}
	handshakeCtx, cancelHandshake := context.WithTimeout(attemptCtx, time.Duration(a.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	defer cancelHandshake()
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	defer stopStreamContext()
	localCaps := transport.AgentCapabilities(a.cfg)
	localLimits := transport.LimitsFromConfig(a.cfg.Limits, a.cfg.StreamLimit)
	max := int64(transport.HandshakeMaxFrame)
	hello, _ := transport.MessageFrame(transport.TypeHello, 1, transport.Hello{AgentID: a.cfg.Agent.ID, Capabilities: localCaps, Limits: localLimits})
	if err = writeFrame(stream, hello, max); err != nil {
		return fail(err)
	}
	f, err := transport.ReadFrame(stream, max)
	if err == nil && f.Type == transport.TypeError {
		var protocolErr transport.ProtocolError
		if decodeErr := transport.DecodeMessage(f, &protocolErr); decodeErr == nil {
			return fail(&protocolErr)
		}
	}
	if err != nil || f.Type != transport.TypeChallenge || f.RequestID != hello.RequestID {
		return fail(errors.New("gateway did not issue challenge"))
	}
	var challenge transport.Challenge
	if err = transport.DecodeMessage(f, &challenge); err != nil {
		return fail(err)
	}
	if err = transport.ValidateChallenge(challenge); err != nil {
		return fail(err)
	}
	if err = transport.ValidateNegotiation(localCaps, challenge.Capabilities, localLimits, challenge.Limits); err != nil {
		return fail(err)
	}
	auth, _ := transport.MessageFrame(transport.TypeAuth, 1, transport.Auth{MAC: transport.SignChallenge(a.cfg.Token, challenge.Nonce, a.cfg.Agent.ID, challenge.Capabilities, challenge.Limits)})
	if err = writeFrame(stream, auth, max); err != nil {
		return fail(err)
	}
	f, err = transport.ReadFrame(stream, max)
	if err == nil && f.Type == transport.TypeError {
		var protocolErr transport.ProtocolError
		if decodeErr := transport.DecodeMessage(f, &protocolErr); decodeErr == nil {
			return fail(&protocolErr)
		}
	}
	if err != nil || f.Type != transport.TypeAuthOK || f.RequestID != auth.RequestID {
		return fail(errors.New("gateway rejected authentication"))
	}
	var result transport.AuthResult
	if err = transport.DecodeMessage(f, &result); err != nil || result.Error != nil {
		if err == nil {
			err = result.Error
		}
		return fail(err)
	}
	regs := make([]transport.TunnelRegistration, 0, len(a.cfg.Agent.Reverse))
	for _, t := range a.cfg.Agent.Reverse {
		regs = append(regs, transport.TunnelRegistration{Name: t.Name, Protocol: t.Protocol, GatewayPort: t.GatewayPort, Profile: a.profile(a.cfg.Obfuscation.ReverseProfile)})
	}
	reg, _ := transport.MessageFrame(transport.TypeRegister, 2, transport.Register{Mappings: regs})
	if err = transport.ValidateRegister(transport.Register{Mappings: regs}); err != nil {
		return fail(err)
	}
	if err = writeFrame(stream, reg, max); err != nil {
		return fail(err)
	}
	f, err = transport.ReadFrame(stream, max)
	if err == nil && f.Type == transport.TypeError {
		var protocolErr transport.ProtocolError
		if decodeErr := transport.DecodeMessage(f, &protocolErr); decodeErr == nil {
			return fail(&protocolErr)
		}
	}
	if err != nil || f.Type != transport.TypeRegisterResult || f.RequestID != reg.RequestID {
		return fail(errors.New("gateway did not acknowledge mappings"))
	}
	var registerResult transport.RegisterResult
	if err = transport.DecodeMessage(f, &registerResult); err != nil || registerResult.Error != nil {
		if err == nil {
			err = registerResult.Error
		}
		return fail(err)
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	cancelHandshake()
	stopStreamContext()
	_ = transport.SetStreamContext(stream, sessionCtx)
	streamLimit := challenge.Limits.MaxStreams
	sessionID, err := cluster.NewSessionID()
	if err != nil {
		cancel()
		return fail(transport.NewProtocolError(transport.ErrorInternal, "session identity unavailable", true))
	}
	sess := &Session{agent: a, conn: conn, sessionID: sessionID, control: stream, caps: append([]transport.Capability(nil), challenge.Capabilities...), limits: challenge.Limits, ctx: sessionCtx, cancel: cancel, streamSem: make(chan struct{}, streamLimit)}
	go sess.controlLoop()
	go sess.acceptReverseLoop()
	go sess.statsLoop()
	return conn, sess, nil
}

func (s *Session) Wait() error { return transport.WaitConn(s.ctx, s.conn) }

func (s *Session) statsLoop() {
	if s == nil || s.agent == nil || s.agent.metrics == nil {
		return
	}
	observe := func() {
		if stats, ok := transport.SessionStats(s.conn); ok {
			s.agent.metrics.ObserveQUIC(stats)
		}
	}
	observe()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			observe()
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Session) maxFrame() int64 {
	if s != nil && s.limits.MaxFrameBytes > 0 {
		return s.limits.MaxFrameBytes
	}
	if s != nil && s.agent != nil {
		return s.agent.cfg.Limits.MaxFrameBytes
	}
	return transport.DefaultMaxFrame
}

func (s *Session) maxRecord() int64 {
	if s != nil && s.limits.MaxRecordBytes > 0 {
		return s.limits.MaxRecordBytes
	}
	if s != nil && s.agent != nil {
		return s.agent.cfg.Limits.MaxRecordBytes
	}
	return 0
}

func (s *Session) maxUDP() int64 {
	if s != nil && s.limits.MaxUDPBytes > 0 {
		return s.limits.MaxUDPBytes
	}
	if s != nil && s.agent != nil {
		return s.agent.cfg.Limits.MaxUDPBytes
	}
	return 0
}

func (s *Session) maxPadding() int64 {
	if s == nil || s.agent == nil {
		return 0
	}
	padding := s.agent.cfg.Obfuscation.MaxPaddingBytes
	if max := s.maxRecord() - 12; max >= 0 && padding > max {
		padding = max
	}
	return padding
}

func (s *Session) relayProfile(name string) relay.Profile {
	if s == nil || s.agent == nil {
		return relay.Profile{}
	}
	return s.agent.relayProfileWithLimits(name, s.limits)
}

func (s *Session) hasCapability(capability transport.Capability) bool {
	return s != nil && transport.SupportsCapability(s.caps, capability)
}

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
		f, err := transport.ReadFrame(s.control, transport.HandshakeMaxFrame)
		if err != nil {
			if s.agent.ctx.Err() == nil {
				s.agent.logger.Info("control loop ended", "event", "agent.control.ended", "error_kind", agentErrorKind(err))
			}
			return
		}
		switch f.Type {
		case transport.TypePing:
			if f.RequestID == 0 {
				return
			}
			pong, _ := transport.MessageFrame(transport.TypePong, f.RequestID, nil)
			_ = s.write(pong)
		default:
			return
		}
	}
}

func (s *Session) write(f transport.Frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeFrame(s.control, f, s.maxFrame())
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
		if s.agent.life != nil && !s.agent.life.TryAdd() {
			stream.Cancel()
			_ = stream.Close()
			release()
			continue
		}
		go func() {
			if s.agent.life != nil {
				defer s.agent.life.Done()
			}
			s.handleReverse(stream, release)
		}()
	}
}

func (s *Session) handleReverse(stream transport.Stream, release func()) {
	defer stream.Close()
	defer release()
	handshakeCtx, cancelHandshake := context.WithTimeout(s.ctx, time.Duration(s.agent.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	f, err := transport.ReadFrame(stream, transport.HandshakeMaxFrame)
	cancelHandshake()
	stopStreamContext()
	stopStreamContext = transport.SetStreamContext(stream, s.ctx)
	defer stopStreamContext()
	if err != nil {
		s.agent.logger.Info("reverse open read failed", "event", "agent.reverse.open_read_failed", "error_kind", agentErrorKind(err))
		return
	}
	if f.Type != transport.TypeOpenReverse || f.RequestID == 0 {
		return
	}
	var open transport.OpenReverse
	if err := transport.DecodeMessage(f, &open); err != nil {
		return
	}
	if err := transport.ValidateOpenReverse(open); err != nil {
		return
	}
	tunnel, ok := s.agent.mappings[open.Name]
	if (open.Protocol == "tcp" && !s.hasCapability(transport.CapabilityReverseTCP)) || (open.Protocol == "udp" && !s.hasCapability(transport.CapabilityReverseUDP)) {
		sendOpenError(stream, f.RequestID, transport.ErrorCapabilityMismatch, "reverse protocol capability was not negotiated", false, s.maxFrame())
		return
	}
	if !ok || tunnel.Protocol != open.Protocol || open.Profile != s.agent.profile(s.agent.cfg.Obfuscation.ReverseProfile) {
		s.agent.logger.Warn("reverse mapping rejected", "event", "agent.reverse.rejected", "mapping", open.Name, "security_audit", true)
		sendOpenError(stream, f.RequestID, transport.ErrorMappingRejected, "unknown or invalid mapping", false, s.maxFrame())
		return
	}
	if open.Protocol == "tcp" {
		s.reverseTCP(stream, tunnel, open.Profile, f.RequestID)
		return
	}
	s.reverseUDP(stream, tunnel, f.RequestID)
}

func (s *Session) reverseTCP(stream transport.Stream, tunnel config.Tunnel, profile string, requestID uint64) {
	dialer := &net.Dialer{Timeout: time.Duration(s.agent.cfg.Limits.DialTimeoutSec) * time.Second}
	conn, err := dialer.DialContext(s.ctx, "tcp", tunnel.Local)
	if err != nil {
		s.agent.logger.Info("reverse local dial failed", "event", "agent.reverse.local_dial_failed", "mapping", tunnel.Name, "error_kind", agentErrorKind(err))
		sendOpenError(stream, requestID, transport.ErrorResourceExhausted, "local destination unavailable", true, s.maxFrame())
		return
	}
	defer conn.Close()
	ok, _ := transport.MessageFrame(transport.TypeOpenOK, requestID, transport.OpenResult{})
	if err := writeFrame(stream, ok, s.maxFrame()); err != nil {
		s.agent.logger.Info("reverse status write failed", "event", "agent.reverse.status_write_failed", "mapping", tunnel.Name, "error_kind", agentErrorKind(err))
		return
	}
	remote := relay.NewConn(stream, s.relayProfile(profile))
	s.agent.metrics.ActiveStreams.Add(1)
	defer s.agent.metrics.ActiveStreams.Add(-1)
	relay.BidirectionalWithIdle(s.ctx, conn, remote, time.Duration(s.agent.cfg.Limits.RelayIdleTimeoutSec)*time.Second, relay.Counters{In: func(n uint64) { s.agent.metrics.BytesIn.Add(n) }, Out: func(n uint64) { s.agent.metrics.BytesOut.Add(n) }})
}

func (s *Session) reverseUDP(stream transport.Stream, tunnel config.Tunnel, requestID uint64) {
	local, err := net.ResolveUDPAddr("udp", tunnel.Local)
	if err != nil {
		sendOpenError(stream, requestID, transport.ErrorMappingRejected, "invalid local UDP destination", false, s.maxFrame())
		return
	}
	conn, err := net.DialUDP("udp", nil, local)
	if err != nil {
		sendOpenError(stream, requestID, transport.ErrorResourceExhausted, "local destination unavailable", true, s.maxFrame())
		return
	}
	defer conn.Close()
	ok, _ := transport.MessageFrame(transport.TypeOpenOK, requestID, transport.OpenResult{})
	if err := writeFrame(stream, ok, s.maxFrame()); err != nil {
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
			f, err := transport.ReadFrame(stream, s.maxFrame())
			if err != nil {
				errCh <- err
				return
			}
			if f.Type != transport.TypeData {
				errCh <- errors.New("unexpected reverse UDP frame")
				return
			}
			d, err := transport.DecodeData(f, s.maxUDP(), s.maxPadding())
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
		buf := make([]byte, s.maxUDP())
		for {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			n, err := conn.Read(buf)
			if n > 0 {
				touch()
				f, _ := transport.MessageFrame(transport.TypeData, 0, transport.NewData(buf[:n], s.agent.profile(s.agent.cfg.Obfuscation.ReverseProfile), s.maxPadding()))
				if err := writeFrame(stream, f, s.maxFrame()); err != nil {
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
	if err := transport.ValidateOpenProxy(transport.OpenProxy{Network: network, Address: address, Port: port, Profile: a.profile(a.cfg.Obfuscation.ProxyProfile)}); err != nil {
		return nil, err
	}
	sess, err := a.sessions.wait(ctx)
	if err != nil {
		return nil, err
	}
	if !sess.hasCapability(transport.CapabilityEgressProxy) {
		return nil, transport.NewProtocolError(transport.ErrorCapabilityMismatch, "egress proxy capability was not negotiated", false)
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
	requestID := uint64(1)
	open, _ := transport.MessageFrame(transport.TypeOpenProxy, requestID, transport.OpenProxy{Network: network, Address: address, Port: port, Profile: a.profile(a.cfg.Obfuscation.ProxyProfile)})
	if err := writeFrame(stream, open, sess.maxFrame()); err != nil {
		_ = stream.Close()
		release()
		return nil, err
	}
	f, err := transport.ReadFrame(stream, transport.HandshakeMaxFrame)
	if err != nil {
		_ = stream.Close()
		release()
		return nil, err
	}
	if f.Type != transport.TypeOpenOK || f.RequestID != requestID {
		if f.Type == transport.TypeError {
			var protocolErr transport.ProtocolError
			if decodeErr := transport.DecodeMessage(f, &protocolErr); decodeErr == nil {
				_ = stream.Close()
				release()
				return nil, &protocolErr
			}
		}
		var failure transport.OpenResult
		_ = transport.DecodeMessage(f, &failure)
		_ = stream.Close()
		release()
		if failure.Error == nil {
			failure.Error = transport.NewProtocolError(transport.ErrorInternal, "remote open failed", false)
		}
		return nil, failure.Error
	}
	cancelHandshake()
	stopStreamContext()
	_ = transport.SetStreamContext(stream, ctx)
	return newLeasedStream(stream, release, sess.limits), nil
}

func (a *Agent) route(inbound, host string) string {
	route, _ := a.routeTarget(inbound, host)
	return route
}

func (a *Agent) routeTarget(inbound, host string) (string, netip.Addr) {
	ctx, cancel := context.WithTimeout(a.ctx, time.Duration(a.cfg.Limits.DialTimeoutSec)*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return config.RouteGateway, netip.Addr{}
	}
	addr, ok := netip.AddrFromSlice(ips[0])
	if !ok {
		return config.RouteGateway, netip.Addr{}
	}
	return a.router.Choose(inbound, host, ips[0]), addr.Unmap()
}

func (a *Agent) relayProfileWithLimits(name string, limits transport.Limits) relay.Profile {
	maxRecord := limits.MaxRecordBytes
	if maxRecord <= 0 {
		maxRecord = a.cfg.Limits.MaxRecordBytes
	}
	maxPadding := a.cfg.Obfuscation.MaxPaddingBytes
	if max := maxRecord - 12; max >= 0 && maxPadding > max {
		maxPadding = max
	}
	maxBatch := limits.MaxWriteBatchBytes
	if maxBatch <= 0 {
		maxBatch = a.cfg.Limits.MaxWriteBatchBytes
	}
	p, err := relay.NewProfileWithBatch(a.profile(name), maxRecord, maxPadding, maxBatch)
	if err != nil {
		p, _ = relay.NewProfileWithBatch(config.ProfileStandard, maxRecord, 0, maxBatch)
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

func sendOpenError(stream io.Writer, id uint64, code transport.ErrorCode, message string, retryable bool, max int64) {
	f, _ := transport.MessageFrame(transport.TypeOpenError, id, transport.OpenResult{Error: transport.NewProtocolError(code, message, retryable)})
	_ = writeFrame(stream, f, max)
}

func agentErrorKind(err error) string {
	return lifecycle.ErrorKind(err)
}
