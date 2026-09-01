package node

import (
	"asterferry/internal/afdp"
	"asterferry/internal/dataplane"
	"asterferry/internal/domain"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/quic-go/quic-go"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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
		listener, packetConn, err := listenAFDPWithRetry(state.ctx, address, serverTLS, quicOptions, afdpObfuscationOptions(spec.Obfuscation))
		if err != nil {
			return fmt.Errorf("listen AFDP endpoint %s: %w", address, err)
		}
		state.quicListeners = append(state.quicListeners, listener)
		state.quicPackets = append(state.quicPackets, packetConn)
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

// listenAFDPWithRetry covers the short handoff window between a QUIC
// generation's listener close and the operating system releasing its UDP
// address. The retry is deliberately bounded and only applies to an address
// collision; malformed TLS or protocol configuration still fails immediately.
func listenAFDPWithRetry(ctx context.Context, address string, tlsConfig *tls.Config, options afdp.QUICOptions, obfuscation afdp.ObfuscationOptions) (*quic.Listener, net.PacketConn, error) {
	listener, packetConn, err := afdp.ListenWithObfuscationPacketConn(address, tlsConfig, options, obfuscation)
	if err == nil || !addressInUse(err) {
		return listener, packetConn, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-deadline.C:
			return nil, nil, err
		case <-ticker.C:
			listener, packetConn, err = afdp.ListenWithObfuscationPacketConn(address, tlsConfig, options, obfuscation)
			if err == nil || !addressInUse(err) {
				return listener, packetConn, err
			}
		}
	}
}

func addressInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "address already in use") || strings.Contains(message, "only one usage of each socket address")
}

func (d *DataPlaneRuntime) acceptGatewayConnections(state *dataGeneration, listener *quic.Listener, spec domain.GatewaySpec) {
	options := sessionOptionsForGateway(spec)
	for {
		connection, err := listener.Accept(state.ctx)
		if err != nil {
			if state.ctx.Err() == nil {
				d.logger.Warn("data-plane Gateway listener stopped", "error", err)
			}
			return
		}
		go d.handleGatewayConnection(state, connection, options)
	}
}

func (d *DataPlaneRuntime) handleGatewayConnection(state *dataGeneration, connection *quic.Conn, options afdp.SessionOptions) {
	session, err := afdp.ServerSessionWithLookup(state.ctx, connection, d.engine.AssignmentForSession, options)
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
				d.logger.Warn("data-plane reverse TCP stream open failed", "assignment_id", assignmentID, "service_id", service.ID, "error", err)
				return
			}
			defer func() { _ = stream.Close(); session.ReleaseStream() }()
			copyDataDuplexLimited(local, stream, d.engine.MaxBufferBytes())
		}(connection)
	}
}

func (d *DataPlaneRuntime) serveGatewayUDP(state *dataGeneration, socket *net.UDPConn, assignmentID string, service domain.Service) {
	maxPayload := dataPlaneDatagramMTU - afdp.DatagramHeaderSize()
	// Read one byte beyond the AFDP payload budget. UDP reads truncate
	// oversized datagrams without returning an error, so an exact-sized buffer
	// would silently forward corrupted payloads.
	buffer := make([]byte, maxPayload+1)
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
		if n > maxPayload {
			d.oversizeDatagrams.Add(1)
			d.logger.Warn("data-plane Gateway UDP datagram exceeds AFDP payload limit", "assignment_id", assignmentID, "service_id", service.ID, "limit", maxPayload)
			continue
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
			d.logger.Warn("data-plane Gateway UDP datagram send failed", "flow_id", flow.id, "sequence", sequence, "error", err)
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
			if errors.Is(receiveErr, afdp.ErrTransient) {
				// UDP reassembly pressure rejects the current datagram only. The
				// authenticated TCP/session channel remains valid and can carry
				// other streams while the bounded reassembly state expires.
				continue
			}
			state.removeUDPFlowsForSession(session)
			// A malformed datagram invalidates the session. Closing it prevents
			// a peer from continuing to use the reliable TCP paths after a
			// protocol violation.
			_ = session.Close()
			return
		}
		flow := state.udpFlow(header.FlowID)
		if flow == nil || flow.session != session {
			continue
		}
		flow.lastUnixNano.Store(time.Now().UnixNano())
		if _, writeErr := flow.socket.WriteToUDP(payload, flow.remote); writeErr != nil {
			d.logger.Warn("data-plane Gateway UDP response write failed", "flow_id", header.FlowID, "error", writeErr)
		}
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
			if errors.Is(err, afdp.ErrTransient) || errors.Is(err, afdp.ErrUnauthorizedOpen) {
				continue
			}
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
