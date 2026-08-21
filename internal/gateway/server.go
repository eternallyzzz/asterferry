package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"asterferry/internal/config"
	"asterferry/internal/lifecycle"
	"asterferry/internal/observability"
	"asterferry/internal/relay"
	"asterferry/internal/security"
	"asterferry/internal/transport"
)

type Gateway struct {
	cfg     *config.GatewayOptions
	ctx     context.Context
	cancel  context.CancelFunc
	ep      transport.Listener
	logger  *slog.Logger
	metrics *observability.Metrics
	mgmt    *observability.Server

	sessions  *sessionRegistry
	mappings  *mappingManager
	egress    *egressProxy
	acl       map[string]*credential
	closeOnce sync.Once
	closeErr  error
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
	ctx       context.Context
	cancel    context.CancelFunc
	writeMu   sync.Mutex
	closeOnce sync.Once
	streamSem chan struct{}
	connSem   chan struct{}
}

type status struct {
	Mode                        string `json:"mode"`
	Ready                       bool   `json:"ready"`
	Agents                      int    `json:"agents"`
	Mappings                    int    `json:"mappings"`
	Listening                   string `json:"listening"`
	TransportObfuscationMode    string `json:"transport_obfuscation_mode"`
	TransportObfuscationKeyHash string `json:"transport_obfuscation_key_fingerprint"`
}

func New(cfg *config.GatewayOptions, loggerOpt ...*slog.Logger) (*Gateway, error) {
	if cfg == nil {
		return nil, errors.New("gateway requires gateway configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.Default()
	if len(loggerOpt) > 0 && loggerOpt[0] != nil {
		logger = loggerOpt[0]
	}
	s := &Gateway{cfg: cfg, ctx: ctx, cancel: cancel, logger: logger, metrics: &observability.Metrics{}, sessions: newSessionRegistry(), acl: map[string]*credential{}}
	s.mappings = newMappingManager(s)
	s.egress = newEgressProxy(s)
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
	mgmt, err := observability.Start(s.cfg.Management.Listen, s.metrics, s)
	if err != nil {
		_ = ep.Close()
		return err
	}
	s.mgmt = mgmt
	go s.acceptLoop()
	return nil
}

func (s *Gateway) IsReady() bool {
	return s != nil && s.ep != nil && s.ctx.Err() == nil
}

func (s *Gateway) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.cancel()
		if s.mgmt != nil {
			_ = s.mgmt.Close()
		}
		if s.mappings != nil {
			s.mappings.CloseAll()
		}
		if s.sessions != nil {
			s.sessions.CloseAll()
		}
		if s.ep != nil {
			s.closeErr = s.ep.Close()
		}
	})
	return s.closeErr
}

func (s *Gateway) Status() any {
	keyHash := ""
	if len(s.cfg.TransportObfuscation.CurrentKey) > 0 {
		keyHash = config.TokenFingerprint(s.cfg.TransportObfuscation.CurrentKey)
	}
	return status{
		Mode:                        config.RoleGateway,
		Ready:                       s.IsReady(),
		Agents:                      s.sessions.Count(),
		Mappings:                    s.mappings.Count(),
		Listening:                   s.cfg.Gateway.Listen,
		TransportObfuscationMode:    s.cfg.TransportObfuscation.Mode,
		TransportObfuscationKeyHash: keyHash,
	}
}

func (s *Gateway) acceptLoop() {
	for {
		conn, err := s.ep.Accept(s.ctx)
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *Gateway) handleConn(conn transport.Session) {
	s.metrics.Connections.Add(1)
	defer s.metrics.Connections.Add(-1)
	stream, err := conn.AcceptStream(s.ctx)
	if err != nil {
		s.logger.Error("accept control stream failed", "event", "gateway.control_stream.accept_failed", "error_kind", errorKind(err))
		_ = transport.CloseSession(conn)
		return
	}
	handshakeCtx, cancelHandshake := context.WithTimeout(s.ctx, time.Duration(s.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	sess, err := s.authenticate(conn, stream)
	cancelHandshake()
	stopStreamContext()
	if err != nil {
		s.logger.Warn("connection rejected", "event", "gateway.auth.rejected", "error_kind", errorKind(err), "security_audit", true)
		s.metrics.AuthFailures.Add(1)
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
		old.Close()
	}

	controlDone := make(chan struct{})
	go func() { s.controlLoop(sess, stream); close(controlDone); sess.Close() }()
	for {
		incoming, err := conn.AcceptStream(sess.ctx)
		if err != nil {
			if s.ctx.Err() == nil {
				s.logger.Info("session closed", "event", "gateway.session.closed", "agent_id", sess.agentID, "error_kind", errorKind(err))
			}
			<-controlDone
			s.sessions.Remove(sess)
			s.mappings.RemoveSession(sess)
			return
		}
		go s.handleAgentStream(sess, incoming)
	}
}

func (s *Gateway) authenticate(conn transport.Session, stream transport.Stream) (*Session, error) {
	max := s.cfg.Limits.MaxFrameBytes
	f, err := transport.ReadFrame(stream, max)
	if err != nil || f.Type != transport.TypeHello {
		return nil, errors.New("invalid hello")
	}
	var hello transport.Hello
	if err := transport.DecodeJSON(f, &hello); err != nil {
		return nil, err
	}
	cred := s.acl[hello.AgentID]
	if cred == nil {
		return nil, errors.New("unknown agent")
	}
	nonce, err := transport.NewNonce()
	if err != nil {
		return nil, err
	}
	challenge, _ := transport.JSONFrame(transport.TypeChallenge, f.RequestID, transport.Challenge{Nonce: nonce})
	if err := writeFrame(stream, challenge, max); err != nil {
		return nil, err
	}
	f, err = transport.ReadFrame(stream, max)
	if err != nil || f.Type != transport.TypeAuth {
		return nil, errors.New("invalid auth response")
	}
	var auth transport.Auth
	if err := transport.DecodeJSON(f, &auth); err != nil {
		return nil, err
	}
	if !transport.VerifyChallenge(cred.token, nonce, auth.MAC, hello.AgentID) {
		return nil, errors.New("authentication failed")
	}
	ok, _ := transport.JSONFrame(transport.TypeAuthOK, f.RequestID, transport.AuthResult{})
	if err := writeFrame(stream, ok, max); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	return &Session{
		gateway:   s,
		agentID:   hello.AgentID,
		conn:      conn,
		ctx:       ctx,
		cancel:    cancel,
		streamSem: make(chan struct{}, s.cfg.StreamLimit),
		connSem:   make(chan struct{}, s.cfg.Limits.MaxConnectionsPerAgent),
	}, nil
}

func (s *Gateway) controlLoop(sess *Session, stream transport.Stream) {
	defer stream.Close()
	for {
		f, err := transport.ReadFrame(stream, s.cfg.Limits.MaxFrameBytes)
		if err != nil {
			if s.ctx.Err() == nil {
				s.logger.Info("control loop ended", "event", "gateway.control.ended", "agent_id", sess.agentID, "error_kind", errorKind(err))
			}
			return
		}
		switch f.Type {
		case transport.TypeRegister:
			var reg transport.Register
			if err := transport.DecodeJSON(f, &reg); err != nil {
				return
			}
			result := s.mappings.Register(sess, reg.Mappings)
			out, _ := transport.JSONFrame(transport.TypeRegisterResult, f.RequestID, result)
			if writeErr := sess.writeControl(stream, out); writeErr != nil {
				return
			}
		case transport.TypePing:
			pong, _ := transport.JSONFrame(transport.TypePong, f.RequestID, nil)
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
		sendOpenError(stream, 0, "agent stream limit exceeded", s.cfg.Limits.MaxFrameBytes)
		_ = stream.Close()
		return
	}
	defer sess.releaseStream()
	defer stream.Close()
	handshakeCtx, cancelHandshake := context.WithTimeout(sess.ctx, time.Duration(s.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	f, err := transport.ReadFrame(stream, s.cfg.Limits.MaxFrameBytes)
	cancelHandshake()
	stopStreamContext()
	stopStreamContext = transport.SetStreamContext(stream, sess.ctx)
	defer stopStreamContext()
	if err != nil || f.Type != transport.TypeOpenProxy {
		return
	}
	var open transport.OpenProxy
	if err := transport.DecodeJSON(f, &open); err != nil {
		return
	}
	if (open.Network != "tcp" && open.Network != "udp") || open.Address == "" || open.Port == 0 {
		sendOpenError(stream, f.RequestID, "invalid destination", s.cfg.Limits.MaxFrameBytes)
		return
	}
	cred := s.acl[sess.agentID]
	if cred == nil {
		sendOpenError(stream, f.RequestID, "agent credentials unavailable", s.cfg.Limits.MaxFrameBytes)
		return
	}
	dialCtx, cancelDial := context.WithTimeout(sess.ctx, time.Duration(s.cfg.Limits.DialTimeoutSec)*time.Second)
	address, err := cred.egress.Allow(dialCtx, open.Network, open.Address, open.Port)
	cancelDial()
	if err != nil {
		sendOpenError(stream, f.RequestID, err.Error(), s.cfg.Limits.MaxFrameBytes)
		return
	}
	release, ok := cred.egress.Acquire()
	if !ok {
		sendOpenError(stream, f.RequestID, "agent egress connection limit exceeded", s.cfg.Limits.MaxFrameBytes)
		return
	}
	defer release()
	profile, err := relay.NewProfile(s.profile(open.Profile), s.cfg.Limits.MaxRecordBytes, s.cfg.Obfuscation.MaxPaddingBytes)
	if err != nil {
		sendOpenError(stream, f.RequestID, "invalid relay profile", s.cfg.Limits.MaxFrameBytes)
		return
	}
	if open.Network == "tcp" {
		s.egress.TCP(sess, stream, address, f.RequestID, profile)
		return
	}
	s.egress.UDP(sess, stream, address, f.RequestID, s.profile(open.Profile))
}

func (s *Gateway) proxyTCP(sess *Session, stream transport.Stream, address string, requestID uint64, profile relay.Profile) {
	dialer := &net.Dialer{Timeout: time.Duration(s.cfg.Limits.DialTimeoutSec) * time.Second}
	conn, err := dialer.DialContext(sess.ctx, "tcp", address)
	if err != nil {
		sendOpenError(stream, requestID, "destination unavailable", s.cfg.Limits.MaxFrameBytes)
		return
	}
	defer conn.Close()
	ok, _ := transport.JSONFrame(transport.TypeOpenOK, requestID, transport.OpenResult{})
	if err := writeFrame(stream, ok, s.cfg.Limits.MaxFrameBytes); err != nil {
		return
	}
	s.metrics.ActiveStreams.Add(1)
	defer s.metrics.ActiveStreams.Add(-1)
	remote := relay.NewConn(stream, profile)
	relay.Bidirectional(remote, conn, relay.Counters{In: func(n uint64) { s.metrics.BytesIn.Add(n) }, Out: func(n uint64) { s.metrics.BytesOut.Add(n) }})
}

func (s *Gateway) proxyUDP(sess *Session, stream transport.Stream, address string, requestID uint64, profile string) {
	remote, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		sendOpenError(stream, requestID, "destination unavailable", s.cfg.Limits.MaxFrameBytes)
		return
	}
	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		sendOpenError(stream, requestID, "destination unavailable", s.cfg.Limits.MaxFrameBytes)
		return
	}
	defer conn.Close()
	ok, _ := transport.JSONFrame(transport.TypeOpenOK, requestID, transport.OpenResult{})
	if err := writeFrame(stream, ok, s.cfg.Limits.MaxFrameBytes); err != nil {
		return
	}
	s.metrics.ActiveStreams.Add(1)
	defer s.metrics.ActiveStreams.Add(-1)
	ctx, cancel := context.WithCancel(sess.ctx)
	defer cancel()
	touch, stopIdle := relay.StartIdleWatch(ctx, time.Duration(s.cfg.Limits.UDPIdleTimeoutSec)*time.Second, func() {
		_ = stream.Close()
		_ = conn.Close()
	})
	defer stopIdle()
	errCh := make(chan error, 2)
	go func() {
		for {
			f, err := transport.ReadFrame(stream, s.cfg.Limits.MaxFrameBytes)
			if err != nil {
				errCh <- err
				return
			}
			if f.Type != transport.TypeData {
				errCh <- errors.New("unexpected UDP frame")
				return
			}
			data, err := transport.DecodeData(f, s.cfg.Limits.MaxUDPBytes, s.cfg.Obfuscation.MaxPaddingBytes)
			if err != nil {
				errCh <- err
				return
			}
			if _, err := conn.Write(data.Payload); err != nil {
				errCh <- err
				return
			}
			touch()
			s.metrics.BytesIn.Add(uint64(len(data.Payload)))
		}
	}()
	go func() {
		buf := make([]byte, s.cfg.Limits.MaxUDPBytes)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			n, err := conn.Read(buf)
			if n > 0 {
				touch()
				f, _ := transport.JSONFrame(transport.TypeData, 0, transport.NewData(buf[:n], profile, s.cfg.Obfuscation.MaxPaddingBytes))
				if err := writeFrame(stream, f, s.cfg.Limits.MaxFrameBytes); err != nil {
					errCh <- err
					return
				}
				s.metrics.BytesOut.Add(uint64(n))
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

func mappingKey(protocol string, port uint16) string { return protocol + ":" + strconv.Itoa(int(port)) }

func (s *Session) acquireStream() bool {
	select {
	case s.streamSem <- struct{}{}:
		return true
	default:
		return false
	}
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
	return writeFrame(stream, f, s.gateway.cfg.Limits.MaxFrameBytes)
}

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.gateway.logger.Info("session closing", "event", "gateway.session.closing", "agent_id", s.agentID)
		s.cancel()
		_ = transport.CloseSession(s.conn)
		s.gateway.sessions.Remove(s)
		s.gateway.mappings.RemoveSession(s)
	})
}

func (s *Gateway) profile(name string) string {
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

func errorKind(err error) string {
	return lifecycle.ErrorKind(err)
}
