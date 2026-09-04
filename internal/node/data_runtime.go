package node

// This file is the process-level bridge between an applied node snapshot and
// the Controller-independent AFDP/2 data-plane adapters.  It intentionally
// contains no REST, SQLite, YAML or Controller imports: the only inputs are a
// bootstrap identity, an Engine and a typed DesiredSnapshot.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"asterferry/internal/afdp"
	"asterferry/internal/dataplane"
	"asterferry/internal/domain"
)

const (
	dataPlaneDatagramMTU = 1200
	dataPlaneFlowLimit   = 256
	dataPlaneByteLimit   = 8 << 20
	dataPlaneFlowTTL     = 30 * time.Second
	// UDP datagrams can arrive on the QUIC DATAGRAM path before the reliable
	// Open stream has been decoded by the Agent. Keep a small, bounded queue so
	// that this normal reordering does not silently discard the first packet of
	// every flow.
	dataPlanePendingDatagramsPerFlow = 8
	dataPlanePendingDatagramBytes    = 64 << 10
	dataPlanePendingDatagramTTL      = dataPlaneFlowTTL
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

	mu                sync.Mutex
	applyMu           sync.Mutex
	started           bool
	ctx               context.Context
	cancel            context.CancelFunc
	state             *dataGeneration
	pending           *domain.DesiredSnapshot
	oversizeDatagrams atomic.Uint64
	telemetry         *runtimeTelemetry
}

// NewDataPlaneRuntime validates the mTLS material once, before any network
// socket is opened. Node bootstrap certificates are used for both sides of an
// AFDP connection; the peer identity is checked again by the AFDP handshake.
func NewDataPlaneRuntime(options DataPlaneOptions) (*DataPlaneRuntime, error) {
	if options.Engine == nil {
		return nil, errors.New("data-plane engine is required")
	}
	if options.Bootstrap.NodeID == "" || options.Bootstrap.NodeID != options.Engine.NodeID() {
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
		telemetry:     newRuntimeTelemetry(),
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
			"active_streams":     float64(d.engine.ActiveStreams()),
			"active_sessions":    float64(d.engine.ActiveSessions()),
			"active_egress":      float64(d.engine.ActiveEgress()),
			"udp_oversize_drops": float64(d.oversizeDatagrams.Load()),
			"geoip_up":           boolMetric(dataplane.GeoIPAvailable()),
		},
	}
	runtimeSnapshot := d.telemetry.snapshot(observed.NodeID)
	for key, value := range runtimeSnapshot.Metrics {
		observed.Metrics[key] = value
	}
	observed.Metrics["active_connections"] = float64(len(runtimeSnapshot.Connections))
	activeFlows := 0
	var bytesIn, bytesOut uint64
	for _, connection := range runtimeSnapshot.Connections {
		bytesIn += connection.BytesIn
		bytesOut += connection.BytesOut
		if connection.Type == domain.RuntimeConnectionTCP || connection.Type == domain.RuntimeConnectionUDP || connection.Type == domain.RuntimeConnectionEgress {
			activeFlows++
		}
	}
	observed.Metrics["active_flows"] = float64(activeFlows)
	observed.Metrics["runtime_bytes_in_total"] = float64(bytesIn)
	observed.Metrics["runtime_bytes_out_total"] = float64(bytesOut)
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

func boolMetric(value bool) float64 {
	if value {
		return 1
	}
	return 0
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
	if bootstrap.NodeID != d.engine.NodeID() {
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
	applyCtx := d.ctx
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
	if applyCtx == nil {
		applyCtx = context.Background()
	}
	if err := d.applyStarted(applyCtx, *snapshot, nil); err != nil {
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
		if err := d.applyStarted(ctx, *pending, nil); err != nil {
			d.Close()
			return err
		}
	}
	return nil
}

// ApplySnapshot installs listeners/sessions for an already validated engine
// generation. Before Start it merely records the cached snapshot; this keeps
// NewRuntime safe to construct without opening sockets during initialization.
func (d *DataPlaneRuntime) ApplySnapshot(ctx context.Context, snapshot domain.DesiredSnapshot, previous *domain.DesiredSnapshot) error {
	if d == nil || d.engine == nil {
		return errors.New("data-plane runtime is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
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
	return d.applyStarted(ctx, snapshot, previous)
}

func (d *DataPlaneRuntime) applyStarted(operationCtx context.Context, snapshot domain.DesiredSnapshot, _ *domain.DesiredSnapshot) error {
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	if err := operationCtx.Err(); err != nil {
		return err
	}
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	// Keep admission closed while listener ownership and the authenticated
	// session index move together. Preserve an existing security drain (for
	// example, certificate revocation) instead of reopening it merely because
	// a certificate or listener rebuild happened to succeed.
	wasDraining := d.engine.IsDraining()
	d.engine.BeginDrain()
	admissionRestored := false
	defer func() {
		if !wasDraining && admissionRestored {
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
		var rollbackErr error
		if old != nil && started {
			restored, restoreErr := d.buildGeneration(ctx, old.snap)
			if restoreErr != nil {
				d.logger.Error("data-plane rollback after snapshot failure failed", "generation", old.snap.Generation, "error", restoreErr)
				rollbackErr = fmt.Errorf("restore previous data-plane generation: %w", restoreErr)
			} else {
				d.mu.Lock()
				if d.started && d.ctx != nil && d.ctx.Err() == nil {
					d.state = restored
					admissionRestored = true
				} else {
					restored.close()
					rollbackErr = errors.New("data-plane runtime stopped during rollback")
				}
				d.mu.Unlock()
			}
		}
		if rollbackErr != nil {
			return errors.Join(err, rollbackErr)
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
	admissionRestored = true
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
		state.closeRuntimeConnections(domain.RuntimeCloseSession)
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
		ctx:               ctx,
		cancel:            cancel,
		engine:            d.engine,
		snap:              snapshot.Clone(),
		tcpListeners:      make(map[string]net.Listener),
		udpListeners:      make(map[string]*net.UDPConn),
		proxies:           make(map[string]net.Listener),
		gatewaySessions:   make(map[string]*afdp.Session),
		agentSessions:     make(map[string]*afdp.Session),
		gatewaySessionIDs: make(map[string]string),
		agentSessionIDs:   make(map[string]string),
		udpFlows:          make(map[uint64]*dataUDPFlow),
		udpByKey:          make(map[string]*dataUDPFlow),
		telemetry:         d.telemetry,
	}
	var err error
	if d.engine.Kind() == domain.NodeSpecGateway {
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

// RuntimeSnapshot is the read-only metadata snapshot sent over the optional
// runtime-telemetry capability.  It intentionally does not expose payloads or
// packet contents.
func (d *DataPlaneRuntime) RuntimeSnapshot() (domain.RuntimeSnapshot, bool) {
	if d == nil || d.engine == nil || d.telemetry == nil {
		return domain.RuntimeSnapshot{}, false
	}
	d.mu.Lock()
	started := d.started
	d.mu.Unlock()
	if !started {
		return domain.RuntimeSnapshot{}, false
	}
	return d.telemetry.snapshot(d.engine.NodeID()), true
}

func (d *DataPlaneRuntime) DrainRuntimeEvents(max int) []domain.RuntimeEvent {
	if d == nil || d.telemetry == nil {
		return nil
	}
	return d.telemetry.drainEvents(max)
}

func (d *DataPlaneRuntime) PeekRuntimeEvents(max int) []domain.RuntimeEvent {
	if d == nil || d.telemetry == nil {
		return nil
	}
	return d.telemetry.peekEvents(max)
}

func (d *DataPlaneRuntime) AckRuntimeEvents(events []domain.RuntimeEvent) {
	if d == nil || d.telemetry == nil {
		return
	}
	d.telemetry.ackEvents(events)
}

func (d *DataPlaneRuntime) ApplyRuntimeAction(ctx context.Context, name string, payload []byte) (int, error) {
	if d == nil || d.telemetry == nil {
		return 0, errors.New("data-plane runtime is not initialized")
	}
	return d.telemetry.applyAction(ctx, name, payload)
}

func (a dataPlaneAddr) String() string { return string(a) }
