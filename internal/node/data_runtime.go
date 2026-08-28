package node

// This file is the process-level bridge between an applied node snapshot and
// the Controller-independent AFDP/1 data-plane adapters.  It intentionally
// contains no REST, SQLite, YAML or Controller imports: the only inputs are a
// bootstrap identity, an Engine and a typed DesiredSnapshot.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"asterferry/internal/afdp"
	"asterferry/internal/dataplane"
	"asterferry/internal/domain"
	"github.com/quic-go/quic-go"
)

const (
	dataPlaneDatagramMTU = 1200
	dataPlaneFlowLimit   = 256
	dataPlaneByteLimit   = 8 << 20
	dataPlaneFlowTTL     = 30 * time.Second
)

// DataPlaneOptions controls the network adapters owned by a node runtime.
// ListenAddress is an optional local override for a Gateway's first public
// endpoint; it is useful when the advertised address is a DNS name or a NAT
// address that cannot be bound locally.
type DataPlaneOptions struct {
	Engine        *dataplane.Engine
	Bootstrap     Bootstrap
	Logger        *slog.Logger
	ListenAddress string
	QUICOptions   afdp.QUICOptions
}

// DataPlaneRuntime owns listeners and AFDP sessions for one node generation.
// ApplySnapshot drains the old socket set before opening the replacement (the
// operating system cannot bind two generations to the same public port), and
// restores the last generation on a failed build.
type DataPlaneRuntime struct {
	engine        *dataplane.Engine
	bootstrap     Bootstrap
	logger        *slog.Logger
	listenAddress string
	quicOptions   afdp.QUICOptions
	serverTLS     *tls.Config
	clientTLS     *tls.Config
	tlsMu         sync.RWMutex

	mu      sync.Mutex
	applyMu sync.Mutex
	started bool
	ctx     context.Context
	cancel  context.CancelFunc
	state   *dataGeneration
	pending *domain.DesiredSnapshot
}

type dataGeneration struct {
	ctx    context.Context
	cancel context.CancelFunc
	engine *dataplane.Engine
	snap   domain.DesiredSnapshot

	quicListeners []*quic.Listener
	tcpListeners  map[string]net.Listener
	udpListeners  map[string]*net.UDPConn
	proxies       map[string]net.Listener

	sessionMu       sync.RWMutex
	gatewaySessions map[string]*afdp.Session
	agentSessions   map[string]*afdp.Session

	udpMu    sync.Mutex
	udpFlows map[uint64]*dataUDPFlow
	udpByKey map[string]*dataUDPFlow
	nextFlow atomic.Uint64
	closed   atomic.Bool
}

type dataUDPFlow struct {
	id           uint64
	key          string
	assignmentID string
	serviceID    string
	socket       *net.UDPConn
	remote       *net.UDPAddr
	session      *afdp.Session
	stream       *quic.Stream
	lease        *dataplane.OpenLease
	sequence     atomic.Uint32
	lastUnixNano atomic.Int64
}

type agentUDPFlow struct {
	conn         *net.UDPConn
	stream       *quic.Stream
	lease        *dataplane.OpenLease
	release      func()
	sequence     atomic.Uint32
	lastUnixNano atomic.Int64
}

// NewDataPlaneRuntime validates the mTLS material once, before any network
// socket is opened. Node bootstrap certificates are used for both sides of an
// AFDP connection; the peer identity is checked again by the AFDP handshake.
func NewDataPlaneRuntime(options DataPlaneOptions) (*DataPlaneRuntime, error) {
	if options.Engine == nil {
		return nil, errors.New("data-plane engine is required")
	}
	if options.Bootstrap.NodeID == "" || options.Bootstrap.Role != options.Engine.Role() || options.Bootstrap.NodeID != options.Engine.NodeID() {
		return nil, errors.New("data-plane bootstrap identity does not match the engine")
	}
	certificate, pool, err := loadDataPlaneTLS(options.Bootstrap)
	if err != nil {
		return nil, err
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &DataPlaneRuntime{
		engine:        options.Engine,
		bootstrap:     options.Bootstrap,
		logger:        options.Logger,
		listenAddress: strings.TrimSpace(options.ListenAddress),
		quicOptions:   options.QUICOptions,
		serverTLS:     afdp.ServerTLSConfigFromPEM(certificate, pool),
		clientTLS:     afdp.ClientTLSConfigFromPEM(certificate, pool, ""),
	}, nil
}

func (d *DataPlaneRuntime) Engine() *dataplane.Engine { return d.engine }

// ObservedState returns a point-in-time summary of listeners, sessions and
// counters owned by the active generation. The boolean is false until a
// cached/desired snapshot has been activated.
func (d *DataPlaneRuntime) ObservedState() (domain.ObservedState, bool) {
	if d == nil || d.engine == nil {
		return domain.ObservedState{}, false
	}
	d.mu.Lock()
	state := d.state
	started := d.started
	d.mu.Unlock()
	if !started || state == nil || state.closed.Load() {
		return domain.ObservedState{}, false
	}
	observed := domain.ObservedState{
		SchemaVersion:     domain.SchemaVersion,
		NodeID:            d.engine.NodeID(),
		AppliedGeneration: state.snap.Generation,
		Healthy:           state.ctx.Err() == nil,
		ObservedAt:        time.Now().UTC(),
		Metrics: map[string]float64{
			"active_streams":  float64(d.engine.ActiveStreams()),
			"active_sessions": float64(d.engine.ActiveSessions()),
			"active_egress":   float64(d.engine.ActiveEgress()),
		},
	}
	state.sessionMu.RLock()
	for assignmentID, session := range state.gatewaySessions {
		if session == nil {
			continue
		}
		observed.Sessions = append(observed.Sessions, domain.SessionSummary{ID: "gateway-" + assignmentID, PeerID: session.Assignment().AgentID, StartedAt: time.Now().UTC(), Streams: int(session.ActiveStreams())})
	}
	for assignmentID, session := range state.agentSessions {
		if session == nil {
			continue
		}
		observed.Sessions = append(observed.Sessions, domain.SessionSummary{ID: "agent-" + assignmentID, PeerID: "gateway", StartedAt: time.Now().UTC(), Streams: int(session.ActiveStreams())})
	}
	state.sessionMu.RUnlock()
	for _, listener := range state.tcpListeners {
		if listener == nil {
			continue
		}
		if address, err := net.ResolveTCPAddr("tcp", listener.Addr().String()); err == nil {
			observed.Listeners = append(observed.Listeners, domain.ListenerState{Protocol: domain.ProtocolTCP, Bind: address.IP.String(), Port: uint16(address.Port), Ready: true})
		}
	}
	for _, socket := range state.udpListeners {
		if socket == nil {
			continue
		}
		if address, ok := socket.LocalAddr().(*net.UDPAddr); ok {
			observed.Listeners = append(observed.Listeners, domain.ListenerState{Protocol: domain.ProtocolUDP, Bind: address.IP.String(), Port: uint16(address.Port), Ready: true})
		}
	}
	return observed, true
}

// UpdateBootstrap replaces the certificate material used by future AFDP
// connections and rebuilds the active generation so a Gateway listener also
// presents the rotated certificate. Existing sessions are closed as part of
// the generation swap; callers should reconnect them through the normal
// assignment loop.
func (d *DataPlaneRuntime) UpdateBootstrap(bootstrap Bootstrap) error {
	if d == nil || d.engine == nil {
		return errors.New("data-plane runtime is not initialized")
	}
	if bootstrap.NodeID != d.engine.NodeID() || bootstrap.Role != d.engine.Role() {
		return errors.New("data-plane bootstrap identity does not match the engine")
	}
	certificate, pool, err := loadDataPlaneTLS(bootstrap)
	if err != nil {
		return fmt.Errorf("load rotated data-plane certificate: %w", err)
	}
	serverTLS := afdp.ServerTLSConfigFromPEM(certificate, pool)
	clientTLS := afdp.ClientTLSConfigFromPEM(certificate, pool, "")
	d.tlsMu.Lock()
	oldServerTLS, oldClientTLS, oldBootstrap := d.serverTLS, d.clientTLS, d.bootstrap
	d.serverTLS, d.clientTLS = serverTLS, clientTLS
	d.bootstrap = bootstrap
	d.tlsMu.Unlock()
	d.mu.Lock()
	started := d.started
	state := d.state
	var snapshot *domain.DesiredSnapshot
	if state != nil {
		copy := state.snap.Clone()
		snapshot = &copy
	}
	d.mu.Unlock()
	if !started || snapshot == nil {
		return nil
	}
	if err := d.applyStarted(*snapshot, nil); err != nil {
		d.tlsMu.Lock()
		d.serverTLS, d.clientTLS, d.bootstrap = oldServerTLS, oldClientTLS, oldBootstrap
		d.tlsMu.Unlock()
		return err
	}
	return nil
}

func loadDataPlaneTLS(bootstrap Bootstrap) (tls.Certificate, *x509.CertPool, error) {
	certificate, err := tls.X509KeyPair([]byte(bootstrap.CertificatePEM), []byte(bootstrap.PrivateKeyPEM))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load node data-plane certificate: %w", err)
	}
	if len(certificate.Certificate) == 0 {
		return tls.Certificate{}, nil, errors.New("node data-plane certificate is empty")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parse node data-plane certificate: %w", err)
	}
	if leaf.Subject.CommonName != bootstrap.NodeID {
		return tls.Certificate{}, nil, errors.New("node data-plane certificate identity does not match bootstrap")
	}
	now := time.Now().UTC()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return tls.Certificate{}, nil, errors.New("node data-plane certificate is expired or not yet valid")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(bootstrap.CAPEM)) {
		return tls.Certificate{}, nil, errors.New("node data-plane CA is invalid")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("node data-plane certificate is not signed by the configured CA: %w", err)
	}
	return certificate, pool, nil
}

// Start activates the current cached generation and makes subsequent
// ApplySnapshot calls live. Calling Start more than once is idempotent.
func (d *DataPlaneRuntime) Start(ctx context.Context) error {
	if d == nil || d.engine == nil {
		return errors.New("data-plane runtime is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	d.started, d.ctx, d.cancel = true, runCtx, cancel
	pending := d.pending
	d.pending = nil
	d.mu.Unlock()
	if pending != nil {
		if err := d.applyStarted(*pending, nil); err != nil {
			d.Close()
			return err
		}
	}
	return nil
}

// ApplySnapshot installs listeners/sessions for an already validated engine
// generation. Before Start it merely records the cached snapshot; this keeps
// NewRuntime safe to construct without opening sockets during initialization.
func (d *DataPlaneRuntime) ApplySnapshot(_ context.Context, snapshot domain.DesiredSnapshot, previous *domain.DesiredSnapshot) error {
	if d == nil || d.engine == nil {
		return errors.New("data-plane runtime is not initialized")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	d.mu.Lock()
	if !d.started {
		copy := snapshot.Clone()
		d.pending = &copy
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()
	return d.applyStarted(snapshot, previous)
}

func (d *DataPlaneRuntime) applyStarted(snapshot domain.DesiredSnapshot, _ *domain.DesiredSnapshot) error {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	// Keep admission closed while listener ownership and the authenticated
	// session index move together. Preserve an existing security drain (for
	// example, certificate revocation) instead of reopening it merely because
	// a certificate or listener rebuild happened to succeed.
	wasDraining := d.engine.IsDraining()
	d.engine.BeginDrain()
	defer func() {
		if !wasDraining {
			d.engine.EndDrain()
		}
	}()
	d.mu.Lock()
	ctx := d.ctx
	old := d.state
	started := d.started
	d.state = nil
	d.mu.Unlock()
	// Socket ownership cannot be duplicated while rebuilding a generation.
	// Drain the old generation first, then attempt a best-effort rebuild of it
	// if the new listener set fails. The Controller callback also rolls the
	// engine generation back, so no partially opened state is published.
	if old != nil {
		old.close()
	}
	state, err := d.buildGeneration(ctx, snapshot)
	if err != nil {
		if old != nil && started {
			if restored, restoreErr := d.buildGeneration(ctx, old.snap); restoreErr == nil {
				d.mu.Lock()
				if d.started && d.ctx != nil && d.ctx.Err() == nil {
					d.state = restored
				} else {
					restored.close()
				}
				d.mu.Unlock()
			}
		}
		return err
	}
	d.mu.Lock()
	if !d.started || d.ctx == nil || d.ctx.Err() != nil {
		d.mu.Unlock()
		state.close()
		return context.Canceled
	}
	d.state = state
	d.mu.Unlock()
	return nil
}

func (d *DataPlaneRuntime) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
	}
	state := d.state
	d.state = nil
	d.started = false
	d.ctx = nil
	d.cancel = nil
	d.mu.Unlock()
	if state != nil {
		state.close()
	}
	return nil
}

// CloseSessions tears down only the authenticated AFDP sessions and their
// flow state, leaving locally bound listeners in place.  It is used for an
// explicit Controller reconnect/revocation action: ordinary Controller
// outages deliberately do not call this method, so the last applied data
// plane keeps carrying existing traffic while the control stream retries.
func (d *DataPlaneRuntime) CloseSessions() {
	if d == nil {
		return
	}
	d.mu.Lock()
	state := d.state
	d.mu.Unlock()
	if state != nil {
		state.closeSessions()
	}
}

// ResetSnapshot removes a generation that was activated before its durable
// cache publication failed. It intentionally keeps the runtime started (and
// its parent context alive) so the next control snapshot can be retried; only
// the generation-owned listeners, sessions and flows are torn down.
func (d *DataPlaneRuntime) ResetSnapshot(expectedGeneration uint64) error {
	if d == nil || d.engine == nil {
		return errors.New("data-plane runtime is not initialized")
	}
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.mu.Lock()
	if d.pending != nil {
		if expectedGeneration != 0 && d.pending.Generation != expectedGeneration {
			d.mu.Unlock()
			return errors.New("data-plane pending generation changed during reset")
		}
		d.pending = nil
		d.mu.Unlock()
		return nil
	}
	state := d.state
	if state == nil {
		d.mu.Unlock()
		return nil
	}
	if expectedGeneration != 0 && state.snap.Generation != expectedGeneration {
		d.mu.Unlock()
		return errors.New("data-plane generation changed during reset")
	}
	d.state = nil
	d.mu.Unlock()
	state.close()
	return nil
}

func (d *DataPlaneRuntime) buildGeneration(parent context.Context, snapshot domain.DesiredSnapshot) (*dataGeneration, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	state := &dataGeneration{
		ctx:             ctx,
		cancel:          cancel,
		engine:          d.engine,
		snap:            snapshot.Clone(),
		tcpListeners:    make(map[string]net.Listener),
		udpListeners:    make(map[string]*net.UDPConn),
		proxies:         make(map[string]net.Listener),
		gatewaySessions: make(map[string]*afdp.Session),
		agentSessions:   make(map[string]*afdp.Session),
		udpFlows:        make(map[uint64]*dataUDPFlow),
		udpByKey:        make(map[string]*dataUDPFlow),
	}
	var err error
	if d.engine.Role() == domain.RoleGateway {
		if snapshot.Gateway == nil {
			err = errors.New("gateway data-plane snapshot is required")
		} else {
			err = d.buildGateway(state, *snapshot.Gateway)
		}
	} else {
		if snapshot.Agent == nil {
			err = errors.New("agent data-plane snapshot is required")
		} else {
			err = d.buildAgent(state, *snapshot.Agent)
		}
	}
	if err != nil {
		state.close()
		return nil, err
	}
	return state, nil
}

func (d *DataPlaneRuntime) buildGateway(state *dataGeneration, spec domain.GatewaySpec) error {
	quicOptions := d.quicOptionsForGateway(spec)
	endpoints := append([]string(nil), spec.PublicEndpoints...)
	if d.listenAddress != "" {
		endpoints = []string{d.listenAddress}
	}
	seenListenAddresses := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		address, err := gatewayListenAddress(endpoint, d.listenAddress != "")
		if err != nil {
			return err
		}
		if _, exists := seenListenAddresses[address]; exists {
			continue
		}
		seenListenAddresses[address] = struct{}{}
		d.tlsMu.RLock()
		serverTLS := d.serverTLS
		d.tlsMu.RUnlock()
		listener, err := afdp.ListenWithObfuscation(address, serverTLS, quicOptions, afdpObfuscationOptions(spec.Obfuscation))
		if err != nil {
			return fmt.Errorf("listen AFDP endpoint %s: %w", address, err)
		}
		state.quicListeners = append(state.quicListeners, listener)
		go d.acceptGatewayConnections(state, listener, spec)
	}
	services := make(map[string]domain.Service, len(state.snap.Services))
	for _, service := range state.snap.Services {
		services[service.ID] = service
	}
	for _, assignment := range state.snap.Assignments {
		if assignment.GatewayID != d.engine.NodeID() || assignment.State != domain.AssignmentApplied {
			continue
		}
		for _, binding := range assignment.Bindings {
			service, ok := services[binding.ServiceID]
			if !ok || !service.Enabled {
				continue
			}
			key := dataBindingKey(assignment.ID, binding)
			address := net.JoinHostPort(binding.Bind, strconv.Itoa(int(binding.Port)))
			switch binding.Protocol {
			case domain.ProtocolTCP:
				listener, err := net.Listen("tcp", address)
				if err != nil {
					return fmt.Errorf("listen reverse TCP %s: %w", address, err)
				}
				state.tcpListeners[key] = listener
				go d.serveGatewayTCP(state, listener, assignment.ID, service)
			case domain.ProtocolUDP:
				udpAddress, err := net.ResolveUDPAddr("udp", address)
				if err != nil {
					return fmt.Errorf("resolve reverse UDP %s: %w", address, err)
				}
				socket, err := net.ListenUDP("udp", udpAddress)
				if err != nil {
					return fmt.Errorf("listen reverse UDP %s: %w", address, err)
				}
				state.udpListeners[key] = socket
				go d.serveGatewayUDP(state, socket, assignment.ID, service)
			}
		}
	}
	return nil
}

func (d *DataPlaneRuntime) buildAgent(state *dataGeneration, spec domain.AgentSpec) error {
	for _, proxy := range spec.Proxies {
		if !proxy.Enabled {
			continue
		}
		listener, err := net.Listen("tcp", proxy.Bind)
		if err != nil {
			return fmt.Errorf("listen %s proxy %s: %w", proxy.Protocol, proxy.ID, err)
		}
		state.proxies[proxy.ID] = listener
		proxy := proxy
		go func() {
			if err := dataplane.ServeProxy(state.ctx, d.engine, listener, proxy, func(ctx context.Context, target, route string) (net.Conn, error) {
				return d.dialProxyTarget(state, ctx, target, route)
			}); err != nil && state.ctx.Err() == nil {
				d.logger.Warn("data-plane proxy stopped", "proxy", proxy.ID, "error", err)
			}
		}()
	}
	for _, assignment := range state.snap.Assignments {
		if assignment.AgentID != d.engine.NodeID() || assignment.PublicEndpoint == "" || assignment.State != domain.AssignmentApplied {
			continue
		}
		assignment := assignment
		go d.runAgentAssignment(state, assignment)
	}
	return nil
}

func (d *DataPlaneRuntime) acceptGatewayConnections(state *dataGeneration, listener *quic.Listener, spec domain.GatewaySpec) {
	options := sessionOptionsForGateway(spec)
	for {
		connection, err := listener.Accept(state.ctx)
		if err != nil {
			return
		}
		go d.handleGatewayConnection(state, connection, options)
	}
}

func (d *DataPlaneRuntime) handleGatewayConnection(state *dataGeneration, connection *quic.Conn, options afdp.SessionOptions) {
	session, err := afdp.AcceptServerSession(state.ctx, connection, d.engine.AssignmentForSession, options)
	if err != nil {
		_ = connection.CloseWithError(quic.ApplicationErrorCode(0xAF01), "AFDP session rejected")
		return
	}
	assignment := session.Assignment()
	if err := d.engine.AuthorizeSession(afdp.SessionHello{AssignmentID: assignment.ID, Generation: assignment.Generation, AgentID: assignment.AgentID}); err != nil {
		_ = session.Close()
		return
	}
	defer d.engine.ReleaseSession()
	assignmentID := assignment.ID
	state.setGatewaySession(assignmentID, session)
	defer func() {
		state.clearGatewaySession(assignmentID, session)
		_ = session.Close()
	}()
	go d.receiveGatewayDatagrams(state, session)
	go d.serveGatewayEgress(state, session)
	select {
	case <-state.ctx.Done():
	case <-connection.Context().Done():
	}
}

func (d *DataPlaneRuntime) serveGatewayTCP(state *dataGeneration, listener net.Listener, assignmentID string, service domain.Service) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go func(local net.Conn) {
			defer local.Close()
			session := state.gatewaySession(assignmentID)
			if session == nil {
				return
			}
			metadata := afdp.OpenMetadata{Protocol: domain.ProtocolTCP, ServiceID: service.ID, Target: service.LocalTarget}
			lease, err := d.engine.ReserveOpen(assignmentID, metadata)
			if err != nil {
				return
			}
			defer lease.Release()
			stream, err := session.OpenStream(state.ctx, metadata)
			if err != nil {
				return
			}
			defer func() { _ = stream.Close(); session.ReleaseStream() }()
			copyDataDuplexLimited(local, stream, d.engine.MaxBufferBytes())
		}(connection)
	}
}

func (d *DataPlaneRuntime) serveGatewayUDP(state *dataGeneration, socket *net.UDPConn, assignmentID string, service domain.Service) {
	buffer := make([]byte, dataPlaneDatagramMTU-afdp.DatagramHeaderSize())
	for {
		_ = socket.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, remote, err := socket.ReadFromUDP(buffer)
		if err != nil {
			if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() {
				state.expireUDPFlows(socket, time.Now())
				if state.ctx.Err() != nil {
					return
				}
				continue
			}
			return
		}
		if state.ctx.Err() != nil {
			return
		}
		state.expireUDPFlows(socket, time.Now())
		flowKey := assignmentID + "|" + service.ID + "|" + remote.String()
		flow := state.udpFlowByKey(flowKey)
		if flow == nil {
			session := state.gatewaySession(assignmentID)
			if session == nil {
				continue
			}
			flowID := state.nextFlow.Add(1)
			metadata := afdp.OpenMetadata{Protocol: domain.ProtocolUDP, ServiceID: service.ID, Target: service.LocalTarget, FlowID: flowID}
			lease, err := d.engine.ReserveOpen(assignmentID, metadata)
			if err != nil {
				continue
			}
			stream, err := session.OpenStream(state.ctx, metadata)
			if err != nil {
				lease.Release()
				continue
			}
			candidate := &dataUDPFlow{id: flowID, key: flowKey, assignmentID: assignmentID, serviceID: service.ID, socket: socket, remote: remote, session: session, stream: stream, lease: lease}
			candidate.lastUnixNano.Store(time.Now().UnixNano())
			existing, added := state.addUDPFlow(candidate)
			if !added {
				_ = stream.Close()
				session.ReleaseStream()
				lease.Release()
				if existing == nil {
					continue
				}
				flow = existing
			} else {
				flow = candidate
			}
		}
		flow.lastUnixNano.Store(time.Now().UnixNano())
		sequence := flow.sequence.Add(1) - 1
		if err := flow.session.SendDatagram(flow.id, sequence, buffer[:n], dataPlaneDatagramMTU); err != nil {
			state.removeUDPFlow(flow)
		}
	}
}

func (d *DataPlaneRuntime) receiveGatewayDatagrams(state *dataGeneration, session *afdp.Session) {
	reassembler, err := afdp.NewReassembler(dataPlaneFlowLimit, dataPlaneByteLimit, dataPlaneDatagramMTU-afdp.DatagramHeaderSize(), dataPlaneFlowTTL)
	if err != nil {
		return
	}
	for {
		header, payload, receiveErr := session.ReceiveDatagramPacket(state.ctx, reassembler)
		if receiveErr != nil {
			state.removeUDPFlowsForSession(session)
			// A malformed or resource-exhausting datagram invalidates the
			// session. Closing it prevents a peer from continuing to use the
			// reliable TCP paths after its UDP input has failed validation.
			_ = session.Close()
			return
		}
		flow := state.udpFlow(header.FlowID)
		if flow == nil || flow.session != session {
			continue
		}
		flow.lastUnixNano.Store(time.Now().UnixNano())
		_, _ = flow.socket.WriteToUDP(payload, flow.remote)
	}
}

// serveGatewayEgress terminates proxy routes that an Agent deliberately sends
// through this Gateway.  These streams are assignment-authorized but are not
// associated with a reverse Service; the Gateway's locally applied egress
// policy is therefore evaluated before the outbound dial.
func (d *DataPlaneRuntime) serveGatewayEgress(state *dataGeneration, session *afdp.Session) {
	assignmentID := session.Assignment().ID
	for {
		stream, metadata, err := session.AcceptStream(state.ctx)
		if err != nil {
			// A peer that sends a malformed Open frame must not leave the
			// authenticated session alive with no stream consumer. Closing the
			// session makes the fail-closed decision explicit and lets the Agent
			// reconnect with its last valid assignment.
			if state.ctx.Err() == nil {
				_ = session.Close()
			}
			return
		}
		if !metadata.Egress || metadata.Protocol != domain.ProtocolTCP || metadata.ServiceID != "" {
			_ = stream.Close()
			session.ReleaseStream()
			continue
		}
		lease, err := d.engine.ReserveOpen(assignmentID, metadata)
		if err != nil {
			_ = stream.Close()
			session.ReleaseStream()
			continue
		}
		go func(stream *quic.Stream, metadata afdp.OpenMetadata, lease *dataplane.OpenLease) {
			defer func() {
				_ = stream.Close()
				session.ReleaseStream()
				lease.Release()
			}()
			target, releaseEgress, err := d.engine.AcquireEgress(state.ctx, domain.ProtocolTCP, metadata.Target)
			if err != nil {
				return
			}
			defer releaseEgress()
			local, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(state.ctx, "tcp", target)
			if err != nil {
				return
			}
			defer local.Close()
			copyDataDuplexLimited(local, stream, d.engine.MaxBufferBytes())
		}(stream, metadata, lease)
	}
}

func (d *DataPlaneRuntime) runAgentAssignment(state *dataGeneration, assignment domain.Assignment) {
	backoff := time.Second
	// The QUIC TLS chain is shared by all Controller-issued nodes. Bind the
	// session to the assigned Gateway identity as well, otherwise any
	// CA-signed node could complete the transport handshake and receive this
	// Agent's service streams.
	options := afdp.SessionOptions{ExpectedPeerID: assignment.GatewayID}
	for {
		if state.ctx.Err() != nil {
			return
		}
		d.tlsMu.RLock()
		clientTLS := d.clientTLS
		d.tlsMu.RUnlock()
		connection, packetConn, err := afdp.DialWithObfuscation(state.ctx, assignment.PublicEndpoint, clientTLS, d.quicOptions, afdpObfuscationOptions(assignment.Obfuscation))
		if err == nil {
			session, sessionErr := afdp.ClientSession(state.ctx, connection, afdp.SessionHello{AssignmentID: assignment.ID, Generation: assignment.Generation, AgentID: assignment.AgentID, Capabilities: []string{"tcp", "udp", "http", "socks5"}}, options)
			if sessionErr == nil {
				if admitErr := d.engine.AuthorizeSession(afdp.SessionHello{AssignmentID: assignment.ID, Generation: assignment.Generation, AgentID: assignment.AgentID}); admitErr != nil {
					_ = session.Close()
					_ = connection.CloseWithError(quic.ApplicationErrorCode(0xAF01), "AFDP session limit reached")
				} else {
					state.setAgentSession(assignment.ID, session)
					d.serveAgentSession(state, session)
					state.clearAgentSession(assignment.ID, session)
					d.engine.ReleaseSession()
					_ = session.Close()
				}
			} else {
				_ = connection.CloseWithError(quic.ApplicationErrorCode(0xAF01), "AFDP handshake rejected")
			}
			_ = connection.CloseWithError(quic.ApplicationErrorCode(0xAF00), "AFDP connection closed")
			_ = packetConn.Close()
		}
		wait := backoff
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-state.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (d *DataPlaneRuntime) serveAgentSession(state *dataGeneration, session *afdp.Session) {
	ctx, cancel := context.WithCancel(state.ctx)
	defer cancel()
	assignmentID := session.Assignment().ID
	flows := make(map[uint64]*agentUDPFlow)
	var flowMu sync.RWMutex
	errCh := make(chan error, 2)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				expireAgentUDPFlows(&flowMu, flows, time.Now())
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		reassembler, err := afdp.NewReassembler(dataPlaneFlowLimit, dataPlaneByteLimit, dataPlaneDatagramMTU-afdp.DatagramHeaderSize(), dataPlaneFlowTTL)
		if err != nil {
			errCh <- err
			return
		}
		for {
			header, payload, receiveErr := session.ReceiveDatagramPacket(ctx, reassembler)
			if receiveErr != nil {
				errCh <- receiveErr
				return
			}
			flowMu.RLock()
			flow := flows[header.FlowID]
			flowMu.RUnlock()
			if flow != nil {
				flow.lastUnixNano.Store(time.Now().UnixNano())
				_, _ = flow.conn.Write(payload)
			}
		}
	}()
	go func() {
		for {
			stream, metadata, err := session.AcceptStream(ctx)
			if err != nil {
				errCh <- err
				return
			}
			// Egress opens are Agent-to-Gateway proxy requests and must never
			// be accepted in the opposite direction by an Agent.  Keeping this
			// directional check outside the engine makes the intent explicit at
			// the stream boundary and avoids dialing an arbitrary target from a
			// Gateway-issued frame.
			if metadata.Egress {
				_ = stream.Close()
				session.ReleaseStream()
				continue
			}
			if metadata.Protocol == domain.ProtocolUDP {
				flowMu.RLock()
				_, existing := flows[metadata.FlowID]
				flowCount := len(flows)
				flowMu.RUnlock()
				if !existing && flowCount >= dataPlaneFlowLimit {
					_ = stream.Close()
					session.ReleaseStream()
					continue
				}
			}
			lease, err := d.engine.ReserveOpen(assignmentID, metadata)
			if err != nil {
				_ = stream.Close()
				session.ReleaseStream()
				continue
			}
			switch metadata.Protocol {
			case domain.ProtocolTCP:
				go d.handleAgentTCP(state, session, stream, metadata, lease)
			case domain.ProtocolUDP:
				target, releaseEgress, egressErr := d.engine.AcquireEgress(state.ctx, domain.ProtocolUDP, metadata.Target)
				if egressErr != nil {
					_ = stream.Close()
					session.ReleaseStream()
					lease.Release()
					continue
				}
				addr, resolveErr := net.ResolveUDPAddr("udp", target)
				if resolveErr != nil {
					_ = stream.Close()
					session.ReleaseStream()
					lease.Release()
					releaseEgress()
					continue
				}
				conn, dialErr := net.DialUDP("udp", nil, addr)
				if dialErr != nil {
					_ = stream.Close()
					session.ReleaseStream()
					lease.Release()
					releaseEgress()
					continue
				}
				_ = stream.Close()
				session.ReleaseStream()
				flow := &agentUDPFlow{conn: conn, stream: stream, lease: lease, release: releaseEgress}
				flow.lastUnixNano.Store(time.Now().UnixNano())
				flowMu.Lock()
				old := flows[metadata.FlowID]
				if old == nil && len(flows) >= dataPlaneFlowLimit {
					flowMu.Unlock()
					closeAgentUDPFlow(flow)
					continue
				}
				flows[metadata.FlowID] = flow
				flowMu.Unlock()
				if old != nil {
					closeAgentUDPFlow(old)
				}
				go d.readAgentUDP(state, session, metadata.FlowID, flow, &flowMu, flows)
			default:
				_ = stream.Close()
				session.ReleaseStream()
				lease.Release()
			}
		}
	}()
	select {
	case <-state.ctx.Done():
	case <-errCh:
	}
	cancel()
	flowMu.Lock()

	// Detach all flow entries before closing sockets. The receive goroutines
	// perform their own compare-and-delete cleanup, so removing the entries
	// here makes that cleanup a no-op while still allowing each flow's lease and
	// egress reservation to be released exactly once by the owner that closes
	// it. Without this explicit release, a session ending because of a Gateway
	// reconnect could permanently consume the Agent's UDP/connection budget.
	staleFlows := make([]*agentUDPFlow, 0, len(flows))
	for flowID, flow := range flows {
		delete(flows, flowID)
		staleFlows = append(staleFlows, flow)
	}
	flowMu.Unlock()
	for _, flow := range staleFlows {
		closeAgentUDPFlow(flow)
	}
}

func (d *DataPlaneRuntime) handleAgentTCP(state *dataGeneration, session *afdp.Session, stream *quic.Stream, metadata afdp.OpenMetadata, lease *dataplane.OpenLease) {
	defer func() { _ = stream.Close(); session.ReleaseStream(); lease.Release() }()
	target, releaseEgress, err := d.engine.AcquireEgress(state.ctx, domain.ProtocolTCP, metadata.Target)
	if err != nil {
		return
	}
	defer releaseEgress()
	local, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(state.ctx, "tcp", target)
	if err != nil {
		return
	}
	defer local.Close()
	copyDataDuplexLimited(local, stream, d.engine.MaxBufferBytes())
}

func (d *DataPlaneRuntime) readAgentUDP(state *dataGeneration, session *afdp.Session, flowID uint64, flow *agentUDPFlow, flowMu *sync.RWMutex, flows map[uint64]*agentUDPFlow) {
	defer func() {
		flowMu.Lock()
		if flows[flowID] == flow {
			delete(flows, flowID)
		}
		flowMu.Unlock()
		closeAgentUDPFlow(flow)
	}()
	buffer := make([]byte, dataPlaneDatagramMTU-afdp.DatagramHeaderSize())
	for {
		_ = flow.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := flow.conn.Read(buffer)
		if err != nil {
			if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() {
				if state.ctx.Err() != nil {
					return
				}
				last := flow.lastUnixNano.Load()
				if last == 0 || time.Since(time.Unix(0, last)) < dataPlaneFlowTTL {
					continue
				}
			}
			return
		}
		flow.lastUnixNano.Store(time.Now().UnixNano())
		sequence := flow.sequence.Add(1) - 1
		if err := session.SendDatagram(flowID, sequence, buffer[:n], dataPlaneDatagramMTU); err != nil {
			return
		}
	}
}

func closeAgentUDPFlow(flow *agentUDPFlow) {
	if flow == nil {
		return
	}
	if flow.conn != nil {
		_ = flow.conn.Close()
	}
	if flow.lease != nil {
		flow.lease.Release()
	}
	if flow.release != nil {
		flow.release()
	}
}

func expireAgentUDPFlows(flowMu *sync.RWMutex, flows map[uint64]*agentUDPFlow, now time.Time) {
	if flowMu == nil || flows == nil {
		return
	}
	cutoff := now.Add(-dataPlaneFlowTTL).UnixNano()
	flowMu.Lock()
	stale := make([]*agentUDPFlow, 0)
	for id, flow := range flows {
		if flow != nil && flow.lastUnixNano.Load() > 0 && flow.lastUnixNano.Load() < cutoff {
			delete(flows, id)
			stale = append(stale, flow)
		}
	}
	flowMu.Unlock()
	for _, flow := range stale {
		closeAgentUDPFlow(flow)
	}
}

func (d *DataPlaneRuntime) dialProxyTarget(state *dataGeneration, ctx context.Context, target, route string) (net.Conn, error) {
	selectedRoute := strings.ToLower(strings.TrimSpace(route))
	if selectedRoute == "" && state.snap.Agent != nil {
		selectedRoute = selectAgentRoute(*state.snap.Agent, target)
	}
	if selectedRoute == "gateway" {
		for _, assignment := range state.snap.Assignments {
			if assignment.AgentID != d.engine.NodeID() || assignment.State != domain.AssignmentApplied {
				continue
			}
			session := state.agentSession(assignment.ID)
			if session == nil {
				continue
			}
			metadata := afdp.OpenMetadata{Protocol: domain.ProtocolTCP, Target: target, Egress: true}
			lease, err := d.engine.ReserveOpen(assignment.ID, metadata)
			if err != nil {
				continue
			}
			stream, err := session.OpenStream(ctx, metadata)
			if err != nil {
				lease.Release()
				continue
			}
			return &afdpStreamConn{stream: stream, release: func() { session.ReleaseStream(); lease.Release() }}, nil
		}
		return nil, errors.New("no connected Gateway assignment matches proxy target")
	}
	approvedTarget, releaseEgress, err := d.engine.AcquireEgress(ctx, domain.ProtocolTCP, target)
	if err != nil {
		return nil, err
	}
	local, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", approvedTarget)
	if err != nil {
		releaseEgress()
		return nil, err
	}
	return &egressConn{Conn: local, release: releaseEgress}, nil
}

func selectAgentRoute(spec domain.AgentSpec, target string) string {
	return dataplane.SelectRoute(spec, target)
}

func afdpObfuscationOptions(policy domain.ObfuscationPolicy) afdp.ObfuscationOptions {
	mode := policy.Mode
	if mode == "" {
		mode = afdp.ObfuscationStandard
	}
	// KeyCiphertext is opaque to nodes. The Controller encrypts secret material
	// at rest; AFDP derives a fixed packet key from the opaque snapshot bytes so
	// both participants can authenticate packets without a second key channel.
	return afdp.ObfuscationOptions{
		Mode:               mode,
		CurrentKey:         append([]byte(nil), policy.KeyCiphertext...),
		HandshakeShaping:   policy.HandshakeShaping,
		MinFragmentBytes:   512,
		MaxFragmentBytes:   1200,
		MaxWirePacketBytes: 1280,
	}
}

func (d *DataPlaneRuntime) quicOptionsForGateway(spec domain.GatewaySpec) afdp.QUICOptions {
	options := d.quicOptions
	if spec.Transport.MaxStreams > 0 {
		options.MaxStreams = int64(spec.Transport.MaxStreams)
	}
	if spec.Transport.HandshakeTimeoutSeconds > 0 {
		options.HandshakeTimeout = time.Duration(spec.Transport.HandshakeTimeoutSeconds) * time.Second
	}
	if spec.Transport.IdleTimeoutSeconds > 0 {
		options.IdleTimeout = time.Duration(spec.Transport.IdleTimeoutSeconds) * time.Second
	}
	return options
}

func sessionOptionsForGateway(spec domain.GatewaySpec) afdp.SessionOptions {
	return afdp.SessionOptions{MaxFrame: spec.Transport.MaxFrameBytes, MaxDatagram: spec.Transport.MaxDatagramBytes, MaxStreams: spec.Transport.MaxStreams}
}

func gatewayListenAddress(endpoint string, explicit bool) (string, error) {
	if explicit {
		if _, _, err := net.SplitHostPort(endpoint); err != nil {
			return "", fmt.Errorf("data-plane listen address must be host:port: %w", err)
		}
		return endpoint, nil
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("gateway public endpoint must be host:port: %w", err)
	}
	if parsed := net.ParseIP(host); parsed != nil && (parsed.IsLoopback() || parsed.IsUnspecified()) {
		return endpoint, nil
	}
	// Public endpoints are often advertised DNS/NAT names. Bind the local
	// wildcard in that case while retaining the advertised endpoint in the
	// assignment sent to Agents.
	return net.JoinHostPort("", port), nil
}

func dataBindingKey(assignmentID string, binding domain.Binding) string {
	return assignmentID + "|" + binding.Protocol + "|" + binding.Bind + "|" + strconv.Itoa(int(binding.Port))
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func copyDataDuplex(left io.ReadWriteCloser, right io.ReadWriteCloser) {
	copyDataDuplexLimited(left, right, 0)
}

func copyDataDuplexLimited(left io.ReadWriteCloser, right io.ReadWriteCloser, maxBuffer int) {
	bufferSize := 32 << 10
	if maxBuffer > 0 {
		bufferSize = maxBuffer / 2
		if bufferSize < 1 {
			bufferSize = 1
		}
		if bufferSize > 32<<10 {
			bufferSize = 32 << 10
		}
	}
	leftBuffer := make([]byte, bufferSize)
	rightBuffer := make([]byte, bufferSize)
	var once sync.Once
	closeBoth := func() { once.Do(func() { _ = left.Close(); _ = right.Close() }) }
	done := make(chan struct{}, 2)
	go func() { _, _ = io.CopyBuffer(left, right, leftBuffer); closeBoth(); done <- struct{}{} }()
	go func() { _, _ = io.CopyBuffer(right, left, rightBuffer); closeBoth(); done <- struct{}{} }()
	<-done
}

type afdpStreamConn struct {
	stream  *quic.Stream
	release func()
	once    sync.Once
}

// egressConn couples a successfully policy-approved direct socket to the
// admission reservation held by the Engine. Releasing on Close makes the
// limit cover the full lifetime of a proxy connection rather than only the
// dial syscall; sync.Once keeps cleanup safe when both copy directions close
// the socket concurrently.
type egressConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *egressConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		if c.release != nil {
			c.release()
		}
	})
	return err
}

func (c *afdpStreamConn) Read(p []byte) (int, error)         { return c.stream.Read(p) }
func (c *afdpStreamConn) Write(p []byte) (int, error)        { return c.stream.Write(p) }
func (c *afdpStreamConn) LocalAddr() net.Addr                { return dataPlaneAddr("afdp-local") }
func (c *afdpStreamConn) RemoteAddr() net.Addr               { return dataPlaneAddr("afdp-gateway") }
func (c *afdpStreamConn) SetDeadline(t time.Time) error      { return c.stream.SetDeadline(t) }
func (c *afdpStreamConn) SetReadDeadline(t time.Time) error  { return c.stream.SetReadDeadline(t) }
func (c *afdpStreamConn) SetWriteDeadline(t time.Time) error { return c.stream.SetWriteDeadline(t) }
func (c *afdpStreamConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.stream.Close()
		if c.release != nil {
			c.release()
		}
	})
	return err
}

type dataPlaneAddr string

func (a dataPlaneAddr) Network() string { return "afdp" }
func (a dataPlaneAddr) String() string  { return string(a) }

func (g *dataGeneration) close() {
	if g == nil || g.closed.Swap(true) {
		return
	}
	g.cancel()
	for _, listener := range g.quicListeners {
		_ = listener.Close()
	}
	for _, listener := range g.tcpListeners {
		_ = listener.Close()
	}
	for _, socket := range g.udpListeners {
		_ = socket.Close()
	}
	for _, listener := range g.proxies {
		_ = listener.Close()
	}
	g.closeSessions()
	// closeSessions releases the authenticated sessions before flow cleanup;
	// the generation itself owns the listeners and is already cancelled above.
	g.udpMu.Lock()
	flows := make([]*dataUDPFlow, 0, len(g.udpFlows))
	for id, flow := range g.udpFlows {
		delete(g.udpFlows, id)
		delete(g.udpByKey, flow.key)
		flows = append(flows, flow)
	}
	g.udpMu.Unlock()
	for _, flow := range flows {
		if flow.stream != nil {
			_ = flow.stream.Close()
			flow.session.ReleaseStream()
		}
		if flow.lease != nil {
			flow.lease.Release()
		}
	}
}

// closeSessions is deliberately independent from close: an explicit
// reconnect must invalidate peer sessions while retaining reverse/proxy
// listeners, whereas a generation swap also closes every listener.
func (g *dataGeneration) closeSessions() {
	if g == nil {
		return
	}
	g.sessionMu.Lock()
	gateway := make([]*afdp.Session, 0, len(g.gatewaySessions))
	for id, session := range g.gatewaySessions {
		delete(g.gatewaySessions, id)
		if session != nil {
			gateway = append(gateway, session)
		}
	}
	agent := make([]*afdp.Session, 0, len(g.agentSessions))
	for id, session := range g.agentSessions {
		delete(g.agentSessions, id)
		if session != nil {
			agent = append(agent, session)
		}
	}
	g.sessionMu.Unlock()
	for _, session := range gateway {
		_ = session.Close()
	}
	for _, session := range agent {
		_ = session.Close()
	}

	// A session close does not synchronously run the receive goroutines. Clear
	// and release flow reservations here so an explicit reconnect cannot leave
	// stale UDP slots until the old goroutine observes the closed connection.
	g.udpMu.Lock()
	flows := make([]*dataUDPFlow, 0, len(g.udpFlows))
	for id, flow := range g.udpFlows {
		delete(g.udpFlows, id)
		delete(g.udpByKey, flow.key)
		flows = append(flows, flow)
	}
	g.udpMu.Unlock()
	for _, flow := range flows {
		if flow.stream != nil {
			_ = flow.stream.Close()
			if flow.session != nil {
				flow.session.ReleaseStream()
			}
		}
		if flow.lease != nil {
			flow.lease.Release()
		}
	}
}

func (g *dataGeneration) setGatewaySession(id string, session *afdp.Session) {
	g.sessionMu.Lock()
	old := g.gatewaySessions[id]
	g.gatewaySessions[id] = session
	g.sessionMu.Unlock()
	if old != nil && old != session {
		_ = old.Close()
	}
}

func (g *dataGeneration) clearGatewaySession(id string, session *afdp.Session) {
	g.sessionMu.Lock()
	if g.gatewaySessions[id] == session {
		delete(g.gatewaySessions, id)
	}
	g.sessionMu.Unlock()
}

func (g *dataGeneration) gatewaySession(id string) *afdp.Session {
	g.sessionMu.RLock()
	defer g.sessionMu.RUnlock()
	return g.gatewaySessions[id]
}

func (g *dataGeneration) setAgentSession(id string, session *afdp.Session) {
	g.sessionMu.Lock()
	old := g.agentSessions[id]
	g.agentSessions[id] = session
	g.sessionMu.Unlock()
	if old != nil && old != session {
		_ = old.Close()
	}
}

func (g *dataGeneration) clearAgentSession(id string, session *afdp.Session) {
	g.sessionMu.Lock()
	if g.agentSessions[id] == session {
		delete(g.agentSessions, id)
	}
	g.sessionMu.Unlock()
}

func (g *dataGeneration) agentSession(id string) *afdp.Session {
	g.sessionMu.RLock()
	defer g.sessionMu.RUnlock()
	return g.agentSessions[id]
}

func (g *dataGeneration) addUDPFlow(flow *dataUDPFlow) (*dataUDPFlow, bool) {
	g.udpMu.Lock()
	defer g.udpMu.Unlock()
	if existing := g.udpByKey[flow.key]; existing != nil {
		return existing, false
	}
	if len(g.udpFlows) >= dataPlaneFlowLimit {
		return nil, false
	}
	g.udpFlows[flow.id] = flow
	g.udpByKey[flow.key] = flow
	return flow, true
}

func (g *dataGeneration) udpFlow(id uint64) *dataUDPFlow {
	g.udpMu.Lock()
	defer g.udpMu.Unlock()
	return g.udpFlows[id]
}

func (g *dataGeneration) udpFlowByKey(key string) *dataUDPFlow {
	g.udpMu.Lock()
	defer g.udpMu.Unlock()
	return g.udpByKey[key]
}

func (g *dataGeneration) removeUDPFlow(flow *dataUDPFlow) {
	if flow == nil {
		return
	}
	g.udpMu.Lock()
	if g.udpFlows[flow.id] != flow {
		g.udpMu.Unlock()
		return
	}
	delete(g.udpFlows, flow.id)
	delete(g.udpByKey, flow.key)
	g.udpMu.Unlock()
	if flow.stream != nil {
		_ = flow.stream.Close()
		flow.session.ReleaseStream()
	}
	if flow.lease != nil {
		flow.lease.Release()
	}
}

func (g *dataGeneration) removeUDPFlowsForSession(session *afdp.Session) {
	g.udpMu.Lock()
	flows := make([]*dataUDPFlow, 0)
	for _, flow := range g.udpFlows {
		if flow.session == session {
			delete(g.udpFlows, flow.id)
			delete(g.udpByKey, flow.key)
			flows = append(flows, flow)
		}
	}
	g.udpMu.Unlock()
	for _, flow := range flows {
		if flow.stream != nil {
			_ = flow.stream.Close()
			flow.session.ReleaseStream()
		}
		if flow.lease != nil {
			flow.lease.Release()
		}
	}
}

func (g *dataGeneration) expireUDPFlows(socket *net.UDPConn, now time.Time) {
	cutoff := now.Add(-dataPlaneFlowTTL).UnixNano()
	g.udpMu.Lock()
	flows := make([]*dataUDPFlow, 0)
	for _, flow := range g.udpFlows {
		if flow.socket == socket && flow.lastUnixNano.Load() < cutoff {
			delete(g.udpFlows, flow.id)
			delete(g.udpByKey, flow.key)
			flows = append(flows, flow)
		}
	}
	g.udpMu.Unlock()
	for _, flow := range flows {
		if flow.stream != nil {
			_ = flow.stream.Close()
			flow.session.ReleaseStream()
		}
		if flow.lease != nil {
			flow.lease.Release()
		}
	}
}
