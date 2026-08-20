package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/quic"

	"asterferry/internal/config"
	"asterferry/internal/observability"
	"asterferry/internal/relay"
	"asterferry/internal/security"
	"asterferry/internal/transport"
)

type Gateway struct {
	cfg     *config.Config
	ctx     context.Context
	cancel  context.CancelFunc
	ep      *quic.Endpoint
	metrics *observability.Metrics
	mgmt    *observability.Server

	mu       sync.RWMutex
	sessions map[string]*Session
	mappings map[string]*Mapping
	acl      map[string]*credential
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
	conn      *quic.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	writeMu   sync.Mutex
	closeOnce sync.Once
	streamSem chan struct{}
	connSem   chan struct{}
}

type status struct {
	Mode      string `json:"mode"`
	Ready     bool   `json:"ready"`
	Agents    int    `json:"agents"`
	Mappings  int    `json:"mappings"`
	Listening string `json:"listening"`
}

func New(cfg *config.Config) (*Gateway, error) {
	if cfg == nil || cfg.Role != config.RoleGateway || cfg.Gateway == nil {
		return nil, errors.New("gateway requires gateway configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Gateway{cfg: cfg, ctx: ctx, cancel: cancel, metrics: &observability.Metrics{}, sessions: map[string]*Session{}, mappings: map[string]*Mapping{}, acl: map[string]*credential{}}
	for _, agent := range cfg.Gateway.Agents {
		token, err := config.ReadToken(agent.TokenFile)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("agent %q: %w", agent.ID, err)
		}
		tcp, _ := config.ParsePortRanges(agent.Reverse.TCPPorts)
		udp, _ := config.ParsePortRanges(agent.Reverse.UDPPorts)
		egress, err := security.NewEgressPolicy(agent.Egress)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("agent %q egress: %w", agent.ID, err)
		}
		s.acl[agent.ID] = &credential{token: token, tcp: tcp, udp: udp, egress: egress}
	}
	return s, nil
}

func (s *Gateway) Start() error {
	ep, err := transport.Listen(s.cfg)
	if err != nil {
		return err
	}
	s.ep = ep
	mgmt, err := observability.Start(s.cfg.Management.Listen, s.metrics, s.Status, s.IsReady)
	if err != nil {
		_ = ep.Close(context.Background())
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
	s.cancel()
	if s.mgmt != nil {
		_ = s.mgmt.Close()
	}
	s.mu.Lock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.Close()
	}
	if s.ep != nil {
		return s.ep.Close(context.Background())
	}
	return nil
}

func (s *Gateway) Status() any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return status{Mode: config.RoleGateway, Ready: s.IsReady(), Agents: len(s.sessions), Mappings: len(s.mappings), Listening: s.cfg.Gateway.Listen}
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

func (s *Gateway) handleConn(conn *quic.Conn) {
	s.metrics.Connections.Add(1)
	defer s.metrics.Connections.Add(-1)
	stream, err := conn.AcceptStream(s.ctx)
	if err != nil {
		log.Printf("gateway accept control stream failed: %v", err)
		_ = conn.Close()
		return
	}
	handshakeCtx, cancelHandshake := context.WithTimeout(s.ctx, time.Duration(s.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stream.SetReadContext(handshakeCtx)
	stream.SetWriteContext(handshakeCtx)
	sess, err := s.authenticate(conn, stream)
	cancelHandshake()
	if err != nil {
		log.Printf("gateway connection rejected: %v", err)
		s.metrics.AuthFailures.Add(1)
		_ = conn.Close()
		return
	}
	stream.SetReadContext(sess.ctx)
	stream.SetWriteContext(sess.ctx)
	s.mu.Lock()
	old := s.sessions[sess.agentID]
	if old == nil && int64(len(s.sessions)) >= s.cfg.Limits.MaxAgents {
		s.mu.Unlock()
		s.metrics.AuthFailures.Add(1)
		_ = conn.Close()
		return
	}
	s.sessions[sess.agentID] = sess
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}

	controlDone := make(chan struct{})
	go func() { s.controlLoop(sess, stream); close(controlDone); sess.Close() }()
	for {
		incoming, err := conn.AcceptStream(sess.ctx)
		if err != nil {
			if s.ctx.Err() == nil {
				log.Printf("gateway session %s closed: %v", sess.agentID, err)
			}
			<-controlDone
			s.removeSession(sess)
			return
		}
		go s.handleAgentStream(sess, incoming)
	}
}

func (s *Gateway) authenticate(conn *quic.Conn, stream *quic.Stream) (*Session, error) {
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
		streamSem: make(chan struct{}, s.cfg.Limits.MaxStreamsPerAgent),
		connSem:   make(chan struct{}, s.cfg.Limits.MaxConnectionsPerAgent),
	}, nil
}

func (s *Gateway) controlLoop(sess *Session, stream *quic.Stream) {
	defer stream.Close()
	for {
		f, err := transport.ReadFrame(stream, s.cfg.Limits.MaxFrameBytes)
		if err != nil {
			if s.ctx.Err() == nil {
				log.Printf("gateway control loop %s ended: %v", sess.agentID, err)
			}
			return
		}
		switch f.Type {
		case transport.TypeRegister:
			var reg transport.Register
			if err := transport.DecodeJSON(f, &reg); err != nil {
				return
			}
			result := s.register(sess, reg.Mappings)
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

func (s *Gateway) register(sess *Session, specs []transport.TunnelRegistration) transport.RegisterResult {
	cred := s.acl[sess.agentID]
	if cred == nil {
		return transport.RegisterResult{Error: "agent credentials disappeared"}
	}
	if len(specs) > int(s.cfg.Limits.MaxStreamsPerAgent) {
		return transport.RegisterResult{Error: "mapping count exceeds agent limit"}
	}
	seen := map[string]bool{}
	seenPorts := map[string]bool{}
	for _, spec := range specs {
		if spec.Name == "" || seen[spec.Name] {
			return transport.RegisterResult{Error: "duplicate or empty mapping name"}
		}
		seen[spec.Name] = true
		if spec.Protocol != "tcp" && spec.Protocol != "udp" {
			return transport.RegisterResult{Error: "unsupported mapping protocol"}
		}
		if _, err := relay.NewProfile(s.profile(spec.Profile), s.cfg.Limits.MaxRecordBytes, s.cfg.Obfuscation.MaxPaddingBytes); err != nil {
			return transport.RegisterResult{Error: "unsupported mapping profile"}
		}
		if !allowedPort(spec.Protocol, spec.GatewayPort, cred) {
			return transport.RegisterResult{Error: fmt.Sprintf("port %d is not allowed", spec.GatewayPort)}
		}
		key := mappingKey(spec.Protocol, spec.GatewayPort)
		if seenPorts[key] {
			return transport.RegisterResult{Error: fmt.Sprintf("port %d is registered more than once", spec.GatewayPort)}
		}
		seenPorts[key] = true
		s.mu.RLock()
		existing := s.mappings[key]
		s.mu.RUnlock()
		if existing != nil && existing.session != sess {
			return transport.RegisterResult{Error: fmt.Sprintf("port %d is already in use", spec.GatewayPort)}
		}
	}

	s.mu.Lock()
	oldMappings := make([]*Mapping, 0)
	for key, m := range s.mappings {
		if m.session == sess {
			delete(s.mappings, key)
			oldMappings = append(oldMappings, m)
		}
	}
	s.mu.Unlock()
	for _, m := range oldMappings {
		_ = m.Close()
	}
	created := make([]*Mapping, 0, len(specs))
	for _, spec := range specs {
		m, err := newMapping(s, sess, spec)
		if err != nil {
			for _, old := range created {
				_ = old.Close()
			}
			s.metrics.MappingFailures.Add(1)
			return transport.RegisterResult{Error: fmt.Sprintf("bind %s/%d: %v", spec.Protocol, spec.GatewayPort, err)}
		}
		created = append(created, m)
	}
	s.mu.Lock()
	for _, m := range created {
		s.mappings[m.key] = m
	}
	s.mu.Unlock()
	for _, m := range created {
		go m.Run()
	}
	return transport.RegisterResult{Mappings: specs}
}

func (s *Gateway) removeSession(sess *Session) {
	s.mu.Lock()
	if s.sessions[sess.agentID] == sess {
		delete(s.sessions, sess.agentID)
	}
	for key, m := range s.mappings {
		if m.session == sess {
			delete(s.mappings, key)
			go m.Close()
		}
	}
	s.mu.Unlock()
}

func (s *Gateway) handleAgentStream(sess *Session, stream *quic.Stream) {
	if !sess.acquireStream() {
		sendOpenError(stream, 0, "agent stream limit exceeded", s.cfg.Limits.MaxFrameBytes)
		_ = stream.Close()
		return
	}
	defer sess.releaseStream()
	defer stream.Close()
	handshakeCtx, cancelHandshake := context.WithTimeout(sess.ctx, time.Duration(s.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stream.SetReadContext(handshakeCtx)
	f, err := transport.ReadFrame(stream, s.cfg.Limits.MaxFrameBytes)
	cancelHandshake()
	stream.SetReadContext(sess.ctx)
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
		s.proxyTCP(sess, stream, address, f.RequestID, profile)
		return
	}
	s.proxyUDP(sess, stream, address, f.RequestID, s.profile(open.Profile))
}

func (s *Gateway) proxyTCP(sess *Session, stream *quic.Stream, address string, requestID uint64, profile relay.Profile) {
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

func (s *Gateway) proxyUDP(sess *Session, stream *quic.Stream, address string, requestID uint64, profile string) {
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

func (s *Session) writeControl(stream *quic.Stream, f transport.Frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeFrame(stream, f, s.gateway.cfg.Limits.MaxFrameBytes)
}

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		log.Printf("gateway session %s closing", s.agentID)
		s.cancel()
		_ = s.conn.Close()
		s.gateway.removeSession(s)
	})
}

func (s *Gateway) profile(name string) string {
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
