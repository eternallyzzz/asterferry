package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"asterferry/internal/cluster"
	"asterferry/internal/config"
	"asterferry/internal/configstore"
	"asterferry/internal/dashboard"
	"asterferry/internal/dial"
	"asterferry/internal/lifecycle"
	"asterferry/internal/observability"
	"asterferry/internal/relay"
	"asterferry/internal/security"
	"asterferry/internal/transport"
)

type Gateway struct {
	cfg             *config.GatewayOptions
	nodeID          string
	ctx             context.Context
	cancel          context.CancelFunc
	ep              transport.Listener
	logger          *slog.Logger
	metrics         *observability.Metrics
	mgmt            *observability.Server
	life            *lifecycle.Gate
	events          *observability.EventHub
	shutdownTrigger *lifecycle.ShutdownTrigger
	configManager   *configstore.Manager

	sessions  sessionDirectory
	mappings  mappingDirectory
	owners    cluster.OwnerStore
	acl       map[string]*credential
	admission *handshakeAdmission
	closeOnce sync.Once
	closeErr  error
	accepting atomic.Bool
}

type credential struct {
	token  []byte
	tcp    []config.PortRange
	udp    []config.PortRange
	egress *security.EgressPolicy
}

type Session struct {
	gateway   *Gateway
	agentID   string
	conn      transport.Session
	sessionID string
	caps      []transport.Capability
	limits    transport.Limits
	ctx       context.Context
	cancel    context.CancelFunc
	writeMu   sync.Mutex
	closeOnce sync.Once
	streamSem chan struct{}
	connSem   chan struct{}
	closeErr  error
}

type status struct {
	Mode                        string `json:"mode"`
	State                       string `json:"state"`
	Ready                       bool   `json:"ready"`
	NodeID                      string `json:"node_id"`
	Agents                      int    `json:"agents"`
	Mappings                    int    `json:"mappings"`
	Listening                   string `json:"listening"`
	TransportObfuscationMode    string `json:"transport_obfuscation_mode"`
	TransportObfuscationKeyHash string `json:"transport_obfuscation_key_fingerprint"`
}

type RuntimeOptions struct {
	Logger          *slog.Logger
	Events          *observability.EventHub
	ShutdownTrigger *lifecycle.ShutdownTrigger
	Config          *configstore.Manager
}

func New(cfg *config.GatewayOptions, loggerOpt ...*slog.Logger) (*Gateway, error) {
	var runtime RuntimeOptions
	if len(loggerOpt) > 0 {
		runtime.Logger = loggerOpt[0]
	}
	return NewWithOptions(cfg, runtime)
}

func NewWithOptions(cfg *config.GatewayOptions, runtime RuntimeOptions) (*Gateway, error) {
	if cfg == nil {
		return nil, errors.New("gateway requires gateway configuration")
	}
	nodeID, err := cluster.ResolveNodeID(cfg.Cluster.NodeID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger := runtime.Logger
	if logger == nil {
		logger = slog.Default()
	}
	owners := cluster.NewLocalOwnerStore()
	s := &Gateway{cfg: cfg, nodeID: nodeID, ctx: ctx, cancel: cancel, logger: logger, metrics: &observability.Metrics{}, life: lifecycle.NewGate(), owners: owners, sessions: newSessionRegistry(nodeID, owners), acl: map[string]*credential{}, admission: newHandshakeAdmission(cfg.Limits.MaxPendingHandshakes), events: runtime.Events, shutdownTrigger: runtime.ShutdownTrigger, configManager: runtime.Config}
	s.mappings = newMappingManager(s)
	for _, agent := range cfg.Agents {
		tcp, _ := config.ParsePortRanges(agent.Reverse.TCPPorts)
		udp, _ := config.ParsePortRanges(agent.Reverse.UDPPorts)
		egress, err := security.NewEgressPolicy(agent.Egress)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("agent %q egress: %w", agent.ID, err)
		}
		s.acl[agent.ID] = &credential{token: append([]byte(nil), agent.Token...), tcp: tcp, udp: udp, egress: egress}
	}
	return s, nil
}

func (s *Gateway) Start() error {
	ep, err := transport.Listen(s.cfg, s.metrics)
	if err != nil {
		return err
	}
	s.ep = ep
	mgmt, err := observability.StartWithTokens(s.cfg.Management.Listen, s.metrics, s, observability.AuthTokens{Admin: s.cfg.Management.AdminToken, Viewer: s.cfg.Management.ViewerToken}, observability.ServerOptions{
		Events:  s.events,
		Actions: s,
		Dashboard: func() http.Handler {
			if s.cfg.Management.Web.Enabled != nil && !*s.cfg.Management.Web.Enabled {
				return nil
			}
			return dashboard.Handler()
		}(),
		TLS: func() *observability.TLSServerOptions {
			if s.cfg.Management.TLS.CertFile == "" {
				return nil
			}
			return &observability.TLSServerOptions{CertFile: s.cfg.Management.TLS.CertFile, KeyFile: s.cfg.Management.TLS.KeyFile}
		}(),
		Config: s.configManager,
		Restart: func() bool {
			return s.shutdownTrigger != nil && s.shutdownTrigger.RequestRestart()
		},
		Logger: s.logger,
	})
	if err != nil {
		_ = ep.Close()
		return err
	}
	s.mgmt = mgmt
	s.accepting.Store(true)
	go s.acceptLoop()
	return nil
}

func (s *Gateway) IsReady() bool {
	return s != nil && s.life != nil && s.life.IsRunning() && s.ep != nil && s.accepting.Load() && s.ctx.Err() == nil
}

func (s *Gateway) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		var errs []error
		s.accepting.Store(false)
		s.life.Stop()
		s.cancel()
		if s.mgmt != nil {
			errs = append(errs, s.mgmt.Close())
		}
		if s.mappings != nil {
			errs = append(errs, s.mappings.CloseAll())
		}
		if s.sessions != nil {
			errs = append(errs, s.sessions.CloseAll())
		}
		if s.ep != nil {
			errs = append(errs, s.ep.Close())
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// Shutdown stops new Gateway admissions, lets active relay work finish until
// ctx expires, and then performs the hard close path.
func (s *Gateway) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	started := s.life.BeginDrain()
	if !started && s.life.State() == lifecycle.StateStopped {
		return nil
	}
	if started {
		s.accepting.Store(false)
		s.metrics.BeginDrain()
		s.logger.Warn("gateway draining", "event", "runtime.draining", "role", config.RoleGateway, "node_id", s.nodeID, "grace_period", s.cfg.Shutdown.GracePeriod.String())
		if s.ep != nil {
			if err := s.ep.StopAccepting(); err != nil {
				s.logger.Warn("gateway listener drain failed", "event", "runtime.drain_listener_failed", "error_kind", lifecycle.ErrorKind(err))
			}
		}
		if s.mappings != nil {
			s.mappings.BeginDrain()
		}
	}
	err := s.life.Wait(ctx)
	forced := err != nil
	if forced {
		s.logger.Warn("gateway drain timed out", "event", "runtime.drain_timeout", "role", config.RoleGateway, "node_id", s.nodeID, "active_work", s.life.Active(), "error_kind", lifecycle.ErrorKind(err))
	} else if started {
		s.logger.Info("gateway drain complete", "event", "runtime.drained", "role", config.RoleGateway, "node_id", s.nodeID)
	}
	if started {
		s.metrics.RecordShutdown(forced)
	}
	if s.shutdownTrigger != nil && s.shutdownTrigger.Requested() {
		action := "shutdown"
		message := "management shutdown completed"
		if s.shutdownTrigger.RestartRequested() {
			action = "configuration_restart"
			message = "management configuration restart completed"
		}
		s.logger.Info(message, "event", "management.action.completed", "action", action, "security_audit", true)
	}
	closeErr := s.Close()
	if closeErr != nil {
		return closeErr
	}
	return err
}

func (s *Gateway) Status() any {
	keyHash := ""
	if len(s.cfg.TransportObfuscation.CurrentKey) > 0 {
		keyHash = config.TokenFingerprint(s.cfg.TransportObfuscation.CurrentKey)
	}
	return status{
		Mode:                        config.RoleGateway,
		State:                       s.life.State().String(),
		Ready:                       s.IsReady(),
		NodeID:                      s.nodeID,
		Agents:                      s.sessions.Count(),
		Mappings:                    s.mappings.Count(),
		Listening:                   s.cfg.Gateway.Listen,
		TransportObfuscationMode:    s.cfg.TransportObfuscation.Mode,
		TransportObfuscationKeyHash: keyHash,
	}
}

func (s *Gateway) Dashboard() observability.DashboardSnapshot {
	if s == nil || s.cfg == nil {
		return observability.DashboardSnapshot{SchemaVersion: observability.DashboardSchemaVersion, Role: config.RoleGateway}
	}
	state := "stopped"
	ready := false
	if s.life != nil {
		state = s.life.State().String()
		ready = s.IsReady()
	}
	mappings := []observability.GatewayMappingSnapshot(nil)
	if s.mappings != nil {
		mappings = s.mappings.Snapshot()
	}
	mappingCounts := make(map[string]int)
	for _, mapping := range mappings {
		mappingCounts[mapping.AgentID]++
	}
	agents := make([]observability.GatewayAgentSnapshot, 0)
	if s.sessions != nil {
		sessions := s.sessions.Snapshot()
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].agentID < sessions[j].agentID })
		for _, session := range sessions {
			if session == nil {
				continue
			}
			connected := session.ctx == nil || session.ctx.Err() == nil
			agents = append(agents, observability.GatewayAgentSnapshot{
				AgentID:      session.agentID,
				SessionID:    session.sessionID,
				NodeID:       s.nodeID,
				Connected:    connected,
				MappingCount: mappingCounts[session.agentID],
			})
		}
	}
	keyFingerprint := ""
	if len(s.cfg.TransportObfuscation.CurrentKey) > 0 {
		keyFingerprint = config.TokenFingerprint(s.cfg.TransportObfuscation.CurrentKey)
	}
	return observability.DashboardSnapshot{
		SchemaVersion: observability.DashboardSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Role:          config.RoleGateway,
		State:         state,
		Ready:         ready,
		NodeID:        s.nodeID,
		Transport: observability.DashboardTransportSnapshot{
			Protocol:        config.ConfigVersion,
			ObfuscationMode: s.cfg.TransportObfuscation.Mode,
			KeyFingerprint:  keyFingerprint,
		},
		Gateway: &observability.GatewayDashboardSnapshot{Agents: agents, Mappings: mappings},
	}
}

func (s *Gateway) RequestShutdown() error {
	if err := s.CanShutdown(); err != nil {
		return err
	}
	s.TriggerShutdown()
	return nil
}

func (s *Gateway) CanShutdown() error {
	if s == nil || s.life == nil || !s.life.IsRunning() || s.shutdownTrigger == nil {
		return observability.ErrActionUnavailable
	}
	if s.shutdownTrigger.Requested() {
		return observability.ErrActionBusy
	}
	return nil
}

func (s *Gateway) TriggerShutdown() {
	if s == nil || s.shutdownTrigger == nil {
		return
	}
	s.logger.Warn("dashboard requested shutdown", "event", "management.action.requested", "action", "shutdown", "security_audit", true)
	_ = s.shutdownTrigger.Request()
}

func (s *Gateway) RequestReconnect() error {
	return observability.ErrActionUnsupported
}

func (s *Gateway) acceptLoop() {
	defer s.accepting.Store(false)
	for {
		conn, err := s.ep.Accept(s.ctx)
		if err != nil {
			if s.ctx.Err() != nil || s.life == nil || s.life.State() != lifecycle.StateRunning {
				return
			}
			s.logger.Error("gateway accept loop stopped", "event", "gateway.accept_loop_failed", "error_kind", lifecycle.ErrorKind(err), "security_audit", true)
			if s.shutdownTrigger != nil {
				_ = s.shutdownTrigger.Request()
			} else {
				_ = s.Shutdown(context.Background())
			}
			return
		}
		go s.handleConn(conn)
	}
}

func (s *Gateway) handleConn(conn transport.Session) {
	releasePending, sourceKey, allowed := s.admission.begin(conn)
	if !allowed {
		s.metrics.AuthFailures.Add(1)
		s.logger.Warn("connection rejected before authentication", "event", "gateway.auth.admission_rejected", "source", sourceKey, "security_audit", true)
		_ = transport.CloseSession(conn)
		return
	}
	defer releasePending()
	admitted := s.life == nil || s.life.TryAdd()
	if !admitted {
		_ = transport.CloseSession(conn)
		return
	}
	if s.life != nil {
		defer func() {
			if admitted {
				s.life.Done()
			}
		}()
	}
	s.metrics.Connections.Add(1)
	defer s.metrics.Connections.Add(-1)
	handshakeCtx, cancelHandshake := context.WithTimeout(s.ctx, time.Duration(s.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stream, err := conn.AcceptStream(handshakeCtx)
	if err != nil {
		cancelHandshake()
		s.admission.failure(sourceKey)
		s.logger.Warn("accept control stream failed", "event", "gateway.control_stream.accept_failed", "error_kind", lifecycle.ErrorKind(err), "security_audit", true)
		_ = transport.CloseSession(conn)
		return
	}
	if s.life != nil && !s.life.IsRunning() {
		cancelHandshake()
		_ = transport.CloseSession(conn)
		return
	}
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	sess, err := s.authenticate(conn, stream)
	stopStreamContext()
	cancelHandshake()
	if err != nil {
		s.admission.failure(sourceKey)
		if protocolErr, ok := err.(*transport.ProtocolError); ok {
			_ = transport.WriteProtocolError(stream, 0, protocolErr.Code, protocolErr.Detail, protocolErr.Retryable, transport.HandshakeMaxFrame)
		}
		s.logger.Warn("connection rejected", "event", "gateway.auth.rejected", "error_kind", lifecycle.ErrorKind(err), "security_audit", true)
		s.metrics.AuthFailures.Add(1)
		_ = transport.CloseSession(conn)
		return
	}
	s.admission.success(sourceKey)
	// The bounded slot protects only the unauthenticated handshake; release it
	// explicitly after authentication. The deferred releasePending is an
	// idempotent fallback for every earlier return path.
	releasePending()
	if s.life != nil && !s.life.IsRunning() {
		_ = transport.CloseSession(conn)
		return
	}
	stopStreamContext = transport.SetStreamContext(stream, sess.ctx)
	defer stopStreamContext()
	old, accepted := s.sessions.Add(sess, s.cfg.Limits.MaxAgents)
	if !accepted {
		s.metrics.AuthFailures.Add(1)
		s.logger.Warn("agent limit reached", "event", "gateway.auth.limit_rejected", "agent_id", sess.agentID, "security_audit", true)
		_ = transport.CloseSession(conn)
		return
	}
	if old != nil {
		_ = old.Close()
	}
	if s.life != nil && !s.life.IsRunning() {
		s.sessions.Remove(sess)
		_ = transport.CloseSession(conn)
		return
	}
	if s.life != nil {
		s.life.Done()
		admitted = false
	}

	controlDone := make(chan struct{})
	go func() { s.controlLoop(sess, stream); close(controlDone); _ = sess.Close() }()
	go sess.statsLoop()
	for {
		incoming, err := conn.AcceptStream(sess.ctx)
		if err != nil {
			if s.ctx.Err() == nil {
				s.logger.Info("session closed", "event", "gateway.session.closed", "agent_id", sess.agentID, "session_id", sess.sessionID, "node_id", s.nodeID, "error_kind", lifecycle.ErrorKind(err))
			}
			<-controlDone
			s.sessions.Remove(sess)
			_ = s.mappings.RemoveSession(sess)
			return
		}
		if s.life != nil && !s.life.TryAdd() {
			_ = incoming.Close()
			continue
		}
		go func() {
			if s.life != nil {
				defer s.life.Done()
			}
			s.handleAgentStream(sess, incoming)
		}()
	}
}

func (s *Gateway) authenticate(conn transport.Session, stream transport.Stream) (*Session, error) {
	max := int64(transport.HandshakeMaxFrame)
	f, err := transport.ReadFrame(stream, max)
	if err != nil || f.Type != transport.TypeHello || f.RequestID == 0 {
		return nil, transport.NewProtocolError(transport.ErrorInvalidFrame, "invalid hello", false)
	}
	var hello transport.Hello
	if err := transport.DecodeMessage(f, &hello); err != nil {
		return nil, err
	}
	helloRequestID := f.RequestID
	if err := transport.ValidateHello(hello); err != nil {
		return nil, transport.NewProtocolError(transport.ErrorCapabilityMismatch, "invalid capabilities", false)
	}
	peerCertificates := conn.PeerCertificates()
	if len(peerCertificates) == 0 {
		return nil, transport.NewProtocolError(transport.ErrorAuthFailed, "authentication failed", false)
	}
	peerID, ok := transport.CertificateAgentID(peerCertificates[0])
	if !ok || peerID != hello.AgentID {
		return nil, transport.NewProtocolError(transport.ErrorAuthFailed, "authentication failed", false)
	}
	cred := s.acl[hello.AgentID]
	if cred == nil {
		return nil, transport.NewProtocolError(transport.ErrorAuthFailed, "authentication failed", false)
	}
	nonce, err := transport.NewNonce()
	if err != nil {
		return nil, transport.NewProtocolError(transport.ErrorInternal, "challenge unavailable", true)
	}
	selectedCaps, err := transport.NegotiateCapabilities(hello.Capabilities, transport.GatewayCapabilities(s.cfg))
	if err != nil {
		return nil, transport.NewProtocolError(transport.ErrorCapabilityMismatch, "capabilities cannot be negotiated", false)
	}
	selectedLimits, err := transport.NegotiateLimits(hello.Limits, transport.LimitsFromConfig(s.cfg.Limits, s.cfg.StreamLimit))
	if err != nil {
		return nil, transport.NewProtocolError(transport.ErrorLimitMismatch, "limits cannot be negotiated", false)
	}
	challenge := transport.MustMessageFrame(transport.TypeChallenge, f.RequestID, transport.Challenge{Nonce: nonce, Capabilities: selectedCaps, Limits: selectedLimits})
	if err := transport.WriteFrame(stream, challenge, max); err != nil {
		return nil, err
	}
	f, err = transport.ReadFrame(stream, max)
	if err != nil || f.Type != transport.TypeAuth || f.RequestID != helloRequestID {
		return nil, transport.NewProtocolError(transport.ErrorInvalidFrame, "invalid auth response", false)
	}
	var auth transport.Auth
	if err := transport.DecodeMessage(f, &auth); err != nil {
		return nil, transport.NewProtocolError(transport.ErrorInvalidFrame, "invalid auth payload", false)
	}
	if err := transport.ValidateAuth(auth); err != nil {
		return nil, transport.NewProtocolError(transport.ErrorInvalidFrame, "invalid auth payload", false)
	}
	if !transport.VerifyChallenge(cred.token, nonce, auth.MAC, hello.AgentID, selectedCaps, selectedLimits) {
		return nil, transport.NewProtocolError(transport.ErrorAuthFailed, "authentication failed", false)
	}
	authOK := transport.MustMessageFrame(transport.TypeAuthOK, f.RequestID, transport.AuthResult{})
	if err := transport.WriteFrame(stream, authOK, max); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	sessionID, err := cluster.NewSessionID()
	if err != nil {
		cancel()
		return nil, transport.NewProtocolError(transport.ErrorInternal, "session identity unavailable", true)
	}
	return &Session{
		gateway:   s,
		agentID:   hello.AgentID,
		conn:      conn,
		sessionID: sessionID,
		caps:      append([]transport.Capability(nil), selectedCaps...),
		limits:    selectedLimits,
		ctx:       ctx,
		cancel:    cancel,
		streamSem: make(chan struct{}, selectedLimits.MaxStreams),
		connSem:   make(chan struct{}, s.cfg.Limits.MaxConnectionsPerAgent),
	}, nil
}

func (s *Session) owner(nodeID string) cluster.Owner {
	if s == nil {
		return cluster.Owner{}
	}
	return cluster.Owner{AgentID: s.agentID, SessionID: s.sessionID, NodeID: nodeID}
}

func (s *Gateway) controlLoop(sess *Session, stream transport.Stream) {
	defer stream.Close()
	for {
		f, err := transport.ReadFrame(stream, transport.HandshakeMaxFrame)
		if err != nil {
			if s.ctx.Err() == nil {
				s.logger.Info("control loop ended", "event", "gateway.control.ended", "agent_id", sess.agentID, "session_id", sess.sessionID, "node_id", s.nodeID, "error_kind", lifecycle.ErrorKind(err))
			}
			return
		}
		switch f.Type {
		case transport.TypeRegister:
			if f.RequestID == 0 {
				return
			}
			var reg transport.Register
			if err := transport.DecodeMessage(f, &reg); err != nil {
				return
			}
			if err := transport.ValidateRegister(reg); err != nil {
				return
			}
			result := s.mappings.Register(sess, reg.Mappings)
			out := transport.MustMessageFrame(transport.TypeRegisterResult, f.RequestID, result)
			if writeErr := sess.writeControl(stream, out); writeErr != nil {
				return
			}
		case transport.TypePing:
			if f.RequestID == 0 {
				return
			}
			pong := transport.MustMessageFrame(transport.TypePong, f.RequestID, nil)
			if writeErr := sess.writeControl(stream, pong); writeErr != nil {
				return
			}
		default:
			return
		}
	}
}

func (s *Gateway) handleAgentStream(sess *Session, stream transport.Stream) {
	if !sess.acquireStream() {
		_ = transport.WriteOpenError(stream, 0, transport.ErrorResourceExhausted, "agent stream limit exceeded", true, sess.maxFrame())
		_ = stream.Close()
		return
	}
	defer sess.releaseStream()
	defer stream.Close()
	handshakeCtx, cancelHandshake := context.WithTimeout(sess.ctx, time.Duration(s.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	f, err := transport.ReadFrame(stream, transport.HandshakeMaxFrame)
	cancelHandshake()
	stopStreamContext()
	stopStreamContext = transport.SetStreamContext(stream, sess.ctx)
	defer stopStreamContext()
	if err != nil || f.Type != transport.TypeOpenProxy || f.RequestID == 0 {
		_ = transport.WriteProtocolError(stream, f.RequestID, transport.ErrorInvalidFrame, "invalid proxy open", false, sess.maxFrame())
		return
	}
	var open transport.OpenProxy
	if err := transport.DecodeMessage(f, &open); err != nil {
		_ = transport.WriteProtocolError(stream, f.RequestID, transport.ErrorInvalidFrame, "invalid proxy payload", false, sess.maxFrame())
		return
	}
	if err := transport.ValidateOpenProxy(open); err != nil {
		_ = transport.WriteProtocolError(stream, f.RequestID, transport.ErrorMappingRejected, "invalid proxy payload", false, sess.maxFrame())
		return
	}
	if !sess.hasCapability(transport.CapabilityEgressProxy) {
		_ = transport.WriteOpenError(stream, f.RequestID, transport.ErrorCapabilityMismatch, "egress proxy capability was not negotiated", false, sess.maxFrame())
		return
	}
	if open.Profile == config.ProfileBalanced && !sess.hasCapability(transport.CapabilityRelayBalanced) {
		_ = transport.WriteOpenError(stream, f.RequestID, transport.ErrorCapabilityMismatch, "balanced relay capability was not negotiated", false, sess.maxFrame())
		return
	}
	if (open.Network != "tcp" && open.Network != "udp") || open.Address == "" || open.Port == 0 {
		_ = transport.WriteOpenError(stream, f.RequestID, transport.ErrorMappingRejected, "invalid destination", false, sess.maxFrame())
		return
	}
	cred := s.acl[sess.agentID]
	if cred == nil {
		_ = transport.WriteOpenError(stream, f.RequestID, transport.ErrorAuthFailed, "agent credentials unavailable", false, sess.maxFrame())
		return
	}
	dialCtx, cancelDial := context.WithTimeout(sess.ctx, time.Duration(s.cfg.Limits.DialTimeoutSec)*time.Second)
	addresses, err := cred.egress.AllowCandidates(dialCtx, open.Network, open.Address, open.Port, open.Candidates)
	cancelDial()
	if err != nil {
		_ = transport.WriteOpenError(stream, f.RequestID, transport.ErrorPolicyDenied, "egress policy denied", false, sess.maxFrame())
		return
	}
	for _, address := range addresses {
		if cred.egress.IsSpecialException(address) {
			s.logger.Warn("special-use egress exception used", "event", "security.egress.special_exception", "agent_id", sess.agentID, "network", open.Network, "address", address, "security_audit", true)
		}
	}
	release, ok := cred.egress.Acquire()
	if !ok {
		_ = transport.WriteOpenError(stream, f.RequestID, transport.ErrorResourceExhausted, "agent egress connection limit exceeded", true, sess.maxFrame())
		return
	}
	defer release()
	profile, err := sess.relayProfile(open.Profile)
	if err != nil {
		_ = transport.WriteOpenError(stream, f.RequestID, transport.ErrorMappingRejected, "invalid relay profile", false, sess.maxFrame())
		return
	}
	if open.Network == "tcp" {
		s.proxyTCP(sess, stream, addresses, f.RequestID, profile)
		return
	}
	s.proxyUDP(sess, stream, addresses, f.RequestID, open.Profile)
}

func (s *Gateway) proxyTCP(sess *Session, stream transport.Stream, addresses []string, requestID uint64, profile relay.Profile) {
	conn, err := dial.TCP(sess.ctx, addresses, time.Duration(s.cfg.Limits.DialTimeoutSec)*time.Second)
	if err != nil {
		_ = transport.WriteOpenError(stream, requestID, transport.ErrorResourceExhausted, "destination unavailable", true, sess.maxFrame())
		return
	}
	defer conn.Close()
	ok := transport.MustMessageFrame(transport.TypeOpenOK, requestID, transport.OpenResult{})
	if err := transport.WriteFrame(stream, ok, sess.maxFrame()); err != nil {
		return
	}
	s.metrics.ActiveStreams.Add(1)
	defer s.metrics.ActiveStreams.Add(-1)
	remote := relay.NewConn(stream, profile)
	relay.BidirectionalWithIdle(sess.ctx, remote, conn, time.Duration(s.cfg.Limits.RelayIdleTimeoutSec)*time.Second, relay.Counters{In: func(n uint64) { s.metrics.BytesIn.Add(n) }, Out: func(n uint64) { s.metrics.BytesOut.Add(n) }})
}

func (s *Gateway) proxyUDP(sess *Session, stream transport.Stream, addresses []string, requestID uint64, profile string) {
	var conn *net.UDPConn
	var err error
	for _, address := range addresses {
		remote, resolveErr := net.ResolveUDPAddr("udp", address)
		if resolveErr != nil {
			err = resolveErr
			continue
		}
		conn, err = net.DialUDP("udp", nil, remote)
		if err == nil {
			break
		}
	}
	if err != nil || conn == nil {
		_ = transport.WriteOpenError(stream, requestID, transport.ErrorResourceExhausted, "destination unavailable", true, sess.maxFrame())
		return
	}
	defer conn.Close()
	ok := transport.MustMessageFrame(transport.TypeOpenOK, requestID, transport.OpenResult{})
	if err := transport.WriteFrame(stream, ok, sess.maxFrame()); err != nil {
		return
	}
	s.metrics.ActiveStreams.Add(1)
	defer s.metrics.ActiveStreams.Add(-1)
	_ = relay.BidirectionalUDP(sess.ctx, stream, conn, relay.UDPPumpOptions{
		MaxFrameBytes:   sess.maxFrame(),
		MaxUDPBytes:     sess.maxUDP(),
		MaxPaddingBytes: sess.maxPadding(),
		Profile:         profile,
		IdleTimeout:     time.Duration(s.cfg.Limits.UDPIdleTimeoutSec) * time.Second,
		Counters: relay.Counters{
			In:  func(n uint64) { s.metrics.BytesIn.Add(n) },
			Out: func(n uint64) { s.metrics.BytesOut.Add(n) },
		},
	})
}

func allowedPort(protocol string, port uint16, c *credential) bool {
	ranges := c.tcp
	if protocol == "udp" {
		ranges = c.udp
	}
	for _, r := range ranges {
		if r.Contains(port) {
			return true
		}
	}
	return false
}

func mappingKey(protocol, bind string, port uint16) string {
	return protocol + ":" + bind + ":" + strconv.Itoa(int(port))
}

func (s *Session) acquireStream() bool {
	select {
	case s.streamSem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Session) statsLoop() {
	if s == nil || s.gateway == nil || s.gateway.metrics == nil {
		return
	}
	observe := func() {
		if stats, ok := transport.SessionStats(s.conn); ok {
			s.gateway.metrics.ObserveQUIC(stats)
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
	if s == nil || s.gateway == nil {
		return transport.DefaultMaxFrame
	}
	limits := s.limits.WithFallback(transport.LimitsFromConfig(s.gateway.cfg.Limits, s.gateway.cfg.StreamLimit))
	if limits.MaxFrameBytes <= 0 {
		return transport.DefaultMaxFrame
	}
	return limits.MaxFrameBytes
}

func (s *Session) maxRecord() int64 {
	if s == nil || s.gateway == nil {
		return 0
	}
	return s.limits.WithFallback(transport.LimitsFromConfig(s.gateway.cfg.Limits, s.gateway.cfg.StreamLimit)).MaxRecordBytes
}

func (s *Session) maxUDP() int64 {
	if s == nil || s.gateway == nil {
		return 0
	}
	return s.limits.WithFallback(transport.LimitsFromConfig(s.gateway.cfg.Limits, s.gateway.cfg.StreamLimit)).MaxUDPBytes
}

func (s *Session) maxPadding() int64 {
	if s == nil || s.gateway == nil {
		return 0
	}
	limits := s.limits.WithFallback(transport.LimitsFromConfig(s.gateway.cfg.Limits, s.gateway.cfg.StreamLimit))
	return limits.EffectivePadding(s.gateway.cfg.Obfuscation.MaxPaddingBytes)
}

func (s *Session) relayProfile(name string) (relay.Profile, error) {
	if s == nil || s.gateway == nil {
		return relay.Profile{}, errors.New("session is closed")
	}
	limits := s.limits.WithFallback(transport.LimitsFromConfig(s.gateway.cfg.Limits, s.gateway.cfg.StreamLimit))
	return relay.NewProfileWithBatch(name, limits.MaxRecordBytes, limits.EffectivePadding(s.gateway.cfg.Obfuscation.MaxPaddingBytes), limits.MaxWriteBatchBytes)
}

func (s *Session) hasCapability(capability transport.Capability) bool {
	return s != nil && transport.SupportsCapability(s.caps, capability)
}

func (s *Session) releaseStream() { <-s.streamSem }

func (s *Session) acquireConnection() (func(), bool) {
	select {
	case s.connSem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.connSem }) }, true
	default:
		return func() {}, false
	}
}

func (s *Session) writeControl(stream transport.Stream, f transport.Frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return transport.WriteFrame(stream, f, s.maxFrame())
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.gateway.logger.Info("session closing", "event", "gateway.session.closing", "agent_id", s.agentID, "session_id", s.sessionID, "node_id", s.gateway.nodeID)
		s.cancel()
		s.closeErr = errors.Join(transport.CloseSession(s.conn), s.gateway.mappings.RemoveSession(s))
		s.gateway.sessions.Remove(s)
	})
	return s.closeErr
}
