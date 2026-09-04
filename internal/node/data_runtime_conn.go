package node

import (
	"asterferry/internal/afdp"
	"asterferry/internal/dataplane"
	"asterferry/internal/domain"
	"asterferry/internal/duplex"
	"context"
	"github.com/quic-go/quic-go"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type dataGeneration struct {
	ctx    context.Context
	cancel context.CancelFunc
	engine *dataplane.Engine
	snap   domain.DesiredSnapshot
	geoIP  *dataplane.GeoIPResolver

	quicListeners []*quic.Listener
	quicPackets   []net.PacketConn
	tcpListeners  map[string]net.Listener
	udpListeners  map[string]*net.UDPConn
	proxies       map[string]net.Listener

	sessionMu         sync.RWMutex
	gatewaySessions   map[string]*afdp.Session
	agentSessions     map[string]*afdp.Session
	gatewaySessionIDs map[string]string
	agentSessionIDs   map[string]string
	telemetry         *runtimeTelemetry

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
	runtime      *runtimeConnection
}

type agentUDPFlow struct {
	conn         *net.UDPConn
	stream       *quic.Stream
	lease        *dataplane.OpenLease
	release      func()
	sequence     atomic.Uint32
	lastUnixNano atomic.Int64
	runtime      *runtimeConnection
}

type pendingAgentDatagram struct {
	payload []byte
	at      time.Time
}

func afdpObfuscationOptions(policy domain.ObfuscationPolicy) afdp.ObfuscationOptions {
	mode := policy.Mode
	if mode == "" {
		mode = afdp.ObfuscationStandard
	}
	return afdp.ObfuscationOptions{
		Mode:                          mode,
		CurrentKey:                    append([]byte(nil), policy.Key...),
		PreviousKey:                   append([]byte(nil), policy.PreviousKey...),
		HandshakeShaping:              policy.HandshakeShaping,
		MinFragmentBytes:              512,
		MaxFragmentBytes:              1200,
		MaxHandshakeFragmentWireBytes: 1280,
	}
}

func dataBindingKey(assignmentID string, binding domain.Binding) string {
	return assignmentID + "|" + binding.Protocol + "|" + binding.Bind + "|" + strconv.Itoa(int(binding.Port))
}

func copyDataDuplexLimited(left io.ReadWriteCloser, right io.ReadWriteCloser, maxBuffer int) {
	_ = duplex.CopyDuplex(left, right, maxBuffer)
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

// CloseWrite preserves TCP FIN semantics through the wrapper that owns the
// egress reservation. The reservation remains active until full Close.
func (c *egressConn) CloseWrite() error {
	if c == nil || c.Conn == nil {
		return net.ErrClosed
	}
	if halfCloser, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return halfCloser.CloseWrite()
	}
	return duplex.ErrHalfCloseUnsupported
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

func (c *afdpStreamConn) Read(p []byte) (int, error) { return c.stream.Read(p) }

func (c *afdpStreamConn) Write(p []byte) (int, error) { return c.stream.Write(p) }

func (c *afdpStreamConn) LocalAddr() net.Addr { return dataPlaneAddr("afdp-local") }

func (c *afdpStreamConn) RemoteAddr() net.Addr { return dataPlaneAddr("afdp-gateway") }

func (c *afdpStreamConn) SetDeadline(t time.Time) error { return c.stream.SetDeadline(t) }

func (c *afdpStreamConn) SetReadDeadline(t time.Time) error { return c.stream.SetReadDeadline(t) }

func (c *afdpStreamConn) SetWriteDeadline(t time.Time) error { return c.stream.SetWriteDeadline(t) }

func (c *afdpStreamConn) Close() error {
	if c == nil || c.stream == nil {
		return net.ErrClosed
	}
	var err error
	c.once.Do(func() {
		// Close the receive half as well. The normal duplex path has already
		// observed EOF before this final close; on an abort this unblocks a
		// pending read without changing idempotent lease release.
		c.stream.CancelRead(quic.StreamErrorCode(0))
		err = c.stream.Close()
		if c.release != nil {
			c.release()
		}
	})
	return err
}

// CloseWrite propagates a normal EOF without releasing the stream reservation
// or aborting the receive half. quic.Stream.Close is explicitly a send-side
// close, so expose that meaning through the net.Conn wrapper used by proxies.
func (c *afdpStreamConn) CloseWrite() error {
	if c == nil || c.stream == nil {
		return net.ErrClosed
	}
	return c.stream.Close()
}

// Abort terminates both QUIC stream halves after a copy error. It is separate
// from CloseWrite so a failed direction cannot turn into a graceful FIN and
// leave the opposite copy goroutine blocked forever.
func (c *afdpStreamConn) Abort() error {
	if c == nil || c.stream == nil {
		return net.ErrClosed
	}
	c.once.Do(func() {
		c.stream.CancelRead(quic.StreamErrorCode(0))
		c.stream.CancelWrite(quic.StreamErrorCode(0))
		if c.release != nil {
			c.release()
		}
	})
	return nil
}

type dataPlaneAddr string

func (a dataPlaneAddr) Network() string { return "afdp" }

func (g *dataGeneration) close() {
	if g == nil || g.closed.Swap(true) {
		return
	}
	g.cancel()
	g.closeRuntimeConnections(domain.RuntimeCloseGeneration)
	// Tear down authenticated sessions before closing the QUIC transport. A
	// peer can otherwise complete a handshake while the listener is draining,
	// keeping the caller-owned UDP socket busy during a same-address rebuild.
	g.closeSessions()
	for _, listener := range g.quicListeners {
		if listener != nil {
			_ = listener.Close()
		}
	}
	for _, packetConn := range g.quicPackets {
		// The packet connection owner waits for quic-go's receive loop before
		// closing the underlying socket. This makes same-address generation
		// replacement deterministic on platforms with asynchronous UDP close.
		_ = packetConn.Close()
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
	// The generation owns all listeners and is already cancelled above.
	g.removeAllUDPFlows(domain.RuntimeCloseGeneration)
}

func (g *dataGeneration) openRuntime(meta domain.RuntimeConnection, closer func()) *runtimeConnection {
	if g == nil || g.telemetry == nil {
		return nil
	}
	return g.telemetry.open(g.ctx, g, meta, closer)
}

func (g *dataGeneration) closeRuntimeConnections(reason string) {
	if g == nil || g.telemetry == nil {
		return
	}
	g.telemetry.closeOwner(g, reason)
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
		delete(g.gatewaySessionIDs, id)
		if session != nil {
			gateway = append(gateway, session)
		}
	}
	agent := make([]*afdp.Session, 0, len(g.agentSessions))
	for id, session := range g.agentSessions {
		delete(g.agentSessions, id)
		delete(g.agentSessionIDs, id)
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
	g.removeAllUDPFlows(domain.RuntimeCloseSession)
}

func (g *dataGeneration) setGatewaySessionWithRuntimeID(id string, session *afdp.Session, runtimeID string) {
	g.sessionMu.Lock()
	old := g.gatewaySessions[id]
	g.gatewaySessions[id] = session
	if runtimeID != "" {
		g.gatewaySessionIDs[id] = runtimeID
	}
	g.sessionMu.Unlock()
	if old != nil && old != session {
		_ = old.Close()
	}
}

func (g *dataGeneration) clearGatewaySession(id string, session *afdp.Session) {
	g.sessionMu.Lock()
	if g.gatewaySessions[id] == session {
		delete(g.gatewaySessions, id)
		delete(g.gatewaySessionIDs, id)
	}
	g.sessionMu.Unlock()
}

func (g *dataGeneration) gatewaySessionRuntimeID(id string) string {
	g.sessionMu.RLock()
	defer g.sessionMu.RUnlock()
	return g.gatewaySessionIDs[id]
}

func (g *dataGeneration) gatewaySession(id string) *afdp.Session {
	g.sessionMu.RLock()
	defer g.sessionMu.RUnlock()
	return g.gatewaySessions[id]
}

func (g *dataGeneration) setAgentSessionWithRuntimeID(id string, session *afdp.Session, runtimeID string) {
	g.sessionMu.Lock()
	old := g.agentSessions[id]
	g.agentSessions[id] = session
	if runtimeID != "" {
		g.agentSessionIDs[id] = runtimeID
	}
	g.sessionMu.Unlock()
	if old != nil && old != session {
		_ = old.Close()
	}
}

func (g *dataGeneration) clearAgentSession(id string, session *afdp.Session) {
	g.sessionMu.Lock()
	if g.agentSessions[id] == session {
		delete(g.agentSessions, id)
		delete(g.agentSessionIDs, id)
	}
	g.sessionMu.Unlock()
}

func (g *dataGeneration) agentSessionRuntimeID(id string) string {
	g.sessionMu.RLock()
	defer g.sessionMu.RUnlock()
	return g.agentSessionIDs[id]
}

func (g *dataGeneration) agentSession(id string) *afdp.Session {
	g.sessionMu.RLock()
	defer g.sessionMu.RUnlock()
	return g.agentSessions[id]
}

func (g *dataGeneration) addUDPFlow(flow *dataUDPFlow) (*dataUDPFlow, bool) {
	if g == nil || flow == nil {
		return nil, false
	}
	g.udpMu.Lock()
	defer g.udpMu.Unlock()
	if g.closed.Load() {
		return nil, false
	}
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

func (g *dataGeneration) removeUDPFlow(flow *dataUDPFlow, reason string) {
	if g == nil || flow == nil {
		return
	}
	g.udpMu.Lock()
	if g.udpFlows[flow.id] != flow {
		g.udpMu.Unlock()
		return
	}
	delete(g.udpFlows, flow.id)
	if g.udpByKey[flow.key] == flow {
		delete(g.udpByKey, flow.key)
	}
	g.udpMu.Unlock()
	if flow.runtime != nil {
		flow.runtime.close(reason)
	}
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

func (g *dataGeneration) removeUDPFlowsForSession(session *afdp.Session) {
	flows := g.snapshotUDPFlows(func(flow *dataUDPFlow) bool { return flow.session == session })
	for _, flow := range flows {
		g.removeUDPFlow(flow, domain.RuntimeCloseSession)
	}
}

func (g *dataGeneration) expireUDPFlows(socket *net.UDPConn, now time.Time) {
	cutoff := now.Add(-dataPlaneFlowTTL).UnixNano()
	flows := g.snapshotUDPFlows(func(flow *dataUDPFlow) bool {
		return flow.socket == socket && flow.lastUnixNano.Load() < cutoff
	})
	for _, flow := range flows {
		g.removeUDPFlow(flow, domain.RuntimeCloseIdle)
	}
}

func (g *dataGeneration) removeAllUDPFlows(reason string) {
	for _, flow := range g.snapshotUDPFlows(nil) {
		g.removeUDPFlow(flow, reason)
	}
}

func (g *dataGeneration) snapshotUDPFlows(predicate func(*dataUDPFlow) bool) []*dataUDPFlow {
	if g == nil {
		return nil
	}
	g.udpMu.Lock()
	defer g.udpMu.Unlock()
	flows := make([]*dataUDPFlow, 0, len(g.udpFlows))
	for _, flow := range g.udpFlows {
		if flow != nil && (predicate == nil || predicate(flow)) {
			flows = append(flows, flow)
		}
	}
	return flows
}
