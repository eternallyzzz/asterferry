package node

import (
	"asterferry/internal/afdp"
	"asterferry/internal/dataplane"
	"asterferry/internal/domain"
	"context"
	"errors"
	"fmt"
	"github.com/quic-go/quic-go"
	"net"
	"strings"
	"sync"
	"time"
)

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
				// A completed session is a successful connection attempt. Do not
				// carry an outage-era backoff into the next reconnect after a
				// healthy session ends.
				backoff = time.Second
			} else {
				d.logger.Warn("data-plane Agent session rejected", "assignment_id", assignment.ID, "gateway", assignment.GatewayID, "error", sessionErr)
				_ = connection.CloseWithError(quic.ApplicationErrorCode(0xAF01), "AFDP handshake rejected")
			}
			_ = connection.CloseWithError(quic.ApplicationErrorCode(0xAF00), "AFDP connection closed")
			_ = packetConn.Close()
		} else if state.ctx.Err() == nil {
			d.logger.Warn("data-plane Agent connection failed", "assignment_id", assignment.ID, "endpoint", assignment.PublicEndpoint, "error", err)
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
	pending := make(map[uint64][]pendingAgentDatagram)
	pendingBytes := 0
	var flowMu sync.RWMutex
	errCh := make(chan error, 2)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				expireAgentUDPFlows(&flowMu, flows, time.Now())
				expirePendingAgentDatagrams(&flowMu, pending, &pendingBytes, time.Now())
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
				if errors.Is(receiveErr, afdp.ErrTransient) {
					continue
				}
				d.logger.Warn("data-plane Agent datagram receive failed", "assignment_id", assignmentID, "error", receiveErr)
				errCh <- receiveErr
				return
			}
			flowMu.Lock()
			flow := flows[header.FlowID]
			if flow == nil {
				queue := pending[header.FlowID]
				if len(queue) < dataPlanePendingDatagramsPerFlow && pendingBytes+len(payload) <= dataPlanePendingDatagramBytes {
					pending[header.FlowID] = append(queue, pendingAgentDatagram{payload: append([]byte(nil), payload...), at: time.Now()})
					pendingBytes += len(payload)
				}
				flowMu.Unlock()
				continue
			}
			flowMu.Unlock()
			flow.lastUnixNano.Store(time.Now().UnixNano())
			if _, writeErr := flow.conn.Write(payload); writeErr != nil {
				d.logger.Warn("data-plane Agent UDP target write failed", "flow_id", header.FlowID, "error", writeErr)
			}
		}
	}()
	go func() {
		for {
			stream, metadata, err := session.AcceptStream(ctx)
			if err != nil {
				if errors.Is(err, afdp.ErrTransient) || errors.Is(err, afdp.ErrUnauthorizedOpen) {
					continue
				}
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
					discardPendingAgentDatagrams(&flowMu, pending, &pendingBytes, metadata.FlowID)
					_ = stream.Close()
					session.ReleaseStream()
					continue
				}
			}
			lease, err := d.engine.ReserveOpen(assignmentID, metadata)
			if err != nil {
				discardPendingAgentDatagrams(&flowMu, pending, &pendingBytes, metadata.FlowID)
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
					discardPendingAgentDatagrams(&flowMu, pending, &pendingBytes, metadata.FlowID)
					d.logger.Warn("data-plane Agent UDP egress denied", "service_id", metadata.ServiceID, "target", metadata.Target, "error", egressErr)
					_ = stream.Close()
					session.ReleaseStream()
					lease.Release()
					continue
				}
				addr, resolveErr := net.ResolveUDPAddr("udp", target)
				if resolveErr != nil {
					discardPendingAgentDatagrams(&flowMu, pending, &pendingBytes, metadata.FlowID)
					d.logger.Warn("data-plane Agent UDP target resolve failed", "service_id", metadata.ServiceID, "target", target, "error", resolveErr)
					_ = stream.Close()
					session.ReleaseStream()
					lease.Release()
					releaseEgress()
					continue
				}
				conn, dialErr := net.DialUDP("udp", nil, addr)
				if dialErr != nil {
					discardPendingAgentDatagrams(&flowMu, pending, &pendingBytes, metadata.FlowID)
					d.logger.Warn("data-plane Agent UDP target dial failed", "service_id", metadata.ServiceID, "target", target, "error", dialErr)
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
					discardPendingAgentDatagrams(&flowMu, pending, &pendingBytes, metadata.FlowID)
					closeAgentUDPFlow(flow)
					continue
				}
				flows[metadata.FlowID] = flow
				queued := pending[metadata.FlowID]
				delete(pending, metadata.FlowID)
				for _, datagram := range queued {
					pendingBytes -= len(datagram.payload)
				}
				flowMu.Unlock()
				if old != nil {
					closeAgentUDPFlow(old)
				}
				for _, datagram := range queued {
					flow.lastUnixNano.Store(time.Now().UnixNano())
					if _, writeErr := flow.conn.Write(datagram.payload); writeErr != nil {
						d.logger.Warn("data-plane Agent UDP target write failed", "flow_id", metadata.FlowID, "error", writeErr)
						break
					}
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
	pending = make(map[uint64][]pendingAgentDatagram)
	pendingBytes = 0
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
		d.logger.Warn("data-plane Agent TCP target dial failed", "service_id", metadata.ServiceID, "target", target, "error", err)
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
	maxPayload := dataPlaneDatagramMTU - afdp.DatagramHeaderSize()
	buffer := make([]byte, maxPayload+1)
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
		if n > maxPayload {
			d.oversizeDatagrams.Add(1)
			d.logger.Warn("data-plane Agent UDP datagram exceeds AFDP payload limit", "flow_id", flowID, "limit", maxPayload)
			continue
		}
		flow.lastUnixNano.Store(time.Now().UnixNano())
		sequence := flow.sequence.Add(1) - 1
		if err := session.SendDatagram(flowID, sequence, buffer[:n], dataPlaneDatagramMTU); err != nil {
			d.logger.Warn("data-plane Agent UDP response send failed", "flow_id", flowID, "error", err)
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

func discardPendingAgentDatagrams(flowMu *sync.RWMutex, pending map[uint64][]pendingAgentDatagram, pendingBytes *int, flowID uint64) {
	if flowMu == nil || pending == nil || pendingBytes == nil {
		return
	}
	flowMu.Lock()
	queued := pending[flowID]
	delete(pending, flowID)
	for _, datagram := range queued {
		*pendingBytes -= len(datagram.payload)
	}
	if *pendingBytes < 0 {
		*pendingBytes = 0
	}
	flowMu.Unlock()
}

func expirePendingAgentDatagrams(flowMu *sync.RWMutex, pending map[uint64][]pendingAgentDatagram, pendingBytes *int, now time.Time) {
	if flowMu == nil || pending == nil || pendingBytes == nil {
		return
	}
	cutoff := now.Add(-dataPlanePendingDatagramTTL)
	flowMu.Lock()
	for flowID, queued := range pending {
		kept := queued[:0]
		for _, datagram := range queued {
			if datagram.at.IsZero() || datagram.at.After(cutoff) {
				kept = append(kept, datagram)
				continue
			}
			*pendingBytes -= len(datagram.payload)
		}
		if len(kept) == 0 {
			delete(pending, flowID)
		} else {
			pending[flowID] = kept
		}
	}
	if *pendingBytes < 0 {
		*pendingBytes = 0
	}
	flowMu.Unlock()
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
