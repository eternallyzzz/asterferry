package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"asterferry/internal/afdp"
	"asterferry/internal/domain"
)

const (
	defaultDatagramMTU = 1200
	defaultUDPFlows    = 256
	defaultUDPBytes    = 8 << 20
	defaultUDPTimeout  = 30 * time.Second
)

type udpReverseFlow struct {
	id     uint64
	addr   *net.UDPAddr
	stream interface{ Close() error }
	lease  *OpenLease
	seq    atomic.Uint32
	last   time.Time
}

// ServeUDPReverse maps one public UDP socket to an AFDP DATAGRAM flow per
// remote address. A reliable OpenMetadata stream establishes the service and
// target; payloads then use the bounded fixed-header datagram format.
func ServeUDPReverse(ctx context.Context, engine *Engine, session *afdp.Session, socket *net.UDPConn, service domain.Service) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if engine == nil || session == nil || socket == nil {
		return errors.New("UDP reverse requires engine, session and socket")
	}
	if service.Protocol != domain.ProtocolUDP {
		return errors.New("UDP reverse requires a UDP service")
	}
	socketDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = socket.Close()
		case <-socketDone:
		}
	}()
	defer close(socketDone)
	flows := make(map[string]*udpReverseFlow)
	var flowsMu sync.Mutex
	var nextID atomic.Uint64
	var recvErr = make(chan error, 1)
	reassembler, err := afdp.NewReassembler(defaultUDPFlows, defaultUDPBytes, defaultDatagramMTU-afdp.DatagramHeaderSize(), defaultUDPTimeout)
	if err != nil {
		return err
	}
	go func() {
		for {
			header, payload, err := session.ReceiveDatagramPacket(ctx, reassembler)
			if err != nil {
				if ctx.Err() == nil {
					recvErr <- err
				}
				return
			}
			if writeErr := writeReverseFlowPacket(socket, &flowsMu, flows, header.FlowID, payload); writeErr != nil {
				if ctx.Err() == nil {
					select {
					case recvErr <- fmt.Errorf("write UDP reverse payload: %w", writeErr):
					default:
					}
				}
				return
			}
		}
	}()
	defer func() {
		flowsMu.Lock()
		for key, flow := range flows {
			delete(flows, key)
			if flow.stream != nil {
				_ = flow.stream.Close()
			}
			session.ReleaseStream()
			if flow.lease != nil {
				flow.lease.Release()
			}
		}
		flowsMu.Unlock()
	}()
	buffer := make([]byte, defaultDatagramMTU-afdp.DatagramHeaderSize())
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case err := <-recvErr:
			return err
		default:
		}
		_ = socket.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, addr, err := socket.ReadFromUDP(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		key := addr.String()
		flowsMu.Lock()
		expireUDPReverseFlows(flows, time.Now(), defaultUDPTimeout, session, engine)
		flow := flows[key]
		if flow != nil {
			flow.last = time.Now()
		}
		flowsMu.Unlock()
		if flow == nil {
			flowsMu.Lock()
			atCapacity := len(flows) >= defaultUDPFlows
			flowsMu.Unlock()
			if atCapacity {
				continue
			}
			flowID := nextID.Add(1)
			metadata := afdp.OpenMetadata{Protocol: domain.ProtocolUDP, ServiceID: service.ID, Target: service.LocalTarget, FlowID: flowID}
			lease, reserveErr := engine.ReserveOpen(session.Assignment().ID, metadata)
			if reserveErr != nil {
				continue
			}
			stream, openErr := session.OpenStream(ctx, metadata)
			if openErr != nil {
				lease.Release()
				continue
			}
			candidate := &udpReverseFlow{id: flowID, addr: addr, stream: stream, lease: lease, last: time.Now()}
			flowsMu.Lock()
			existing := flows[key]
			added := false
			if existing == nil && len(flows) < defaultUDPFlows {
				flows[key] = candidate
				flow = candidate
				added = true
			} else if existing != nil {
				flow = existing
			}
			flowsMu.Unlock()
			if !added {
				// A concurrent packet may have filled the bounded flow table after
				// the optimistic capacity check above. Release the newly opened
				// stream/lease instead of allowing the table to exceed its limit.
				_ = stream.Close()
				session.ReleaseStream()
				lease.Release()
				if existing == nil {
					continue
				}
			}
		}
		sequence := flow.seq.Add(1) - 1
		if err := session.SendDatagram(flow.id, sequence, buffer[:n], defaultDatagramMTU); err != nil {
			return err
		}
	}
}

// ServeUDPAgent accepts UDP flow metadata on an Agent, dials each local target
// and forwards datagrams in both directions. Flow and reassembly limits are
// bounded even when a peer sends arbitrary flow IDs.
func ServeUDPAgent(ctx context.Context, engine *Engine, session *afdp.Session) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if engine == nil || session == nil {
		return errors.New("UDP agent forwarding requires engine and session")
	}
	type flow struct {
		conn     *net.UDPConn
		lease    *OpenLease
		release  func()
		seq      atomic.Uint32
		lastUnix atomic.Int64
	}
	flows := make(map[uint64]*flow)
	var mu sync.RWMutex
	var acceptErr = make(chan error, 1)
	defer func() {
		mu.Lock()
		active := make([]*flow, 0, len(flows))
		for flowID, value := range flows {
			delete(flows, flowID)
			active = append(active, value)
		}
		mu.Unlock()
		for _, value := range active {
			if value == nil {
				continue
			}
			if value.conn != nil {
				_ = value.conn.Close()
			}
			if value.lease != nil {
				value.lease.Release()
			}
			if value.release != nil {
				value.release()
			}
		}
	}()
	go func() {
		for {
			stream, metadata, err := session.AcceptStream(ctx)
			if err != nil {
				if ctx.Err() == nil {
					acceptErr <- err
				}
				return
			}
			// AFDP egress is currently a reliable TCP proxy stream.  UDP
			// reverse opens always carry a ServiceID and flow id; rejecting an
			// egress-marked datagram here prevents a Gateway from turning an
			// assignment-authorized metadata frame into an arbitrary Agent-side
			// UDP dial.
			if metadata.Egress || metadata.Protocol != domain.ProtocolUDP || metadata.FlowID == 0 {
				_ = stream.Close()
				session.ReleaseStream()
				continue
			}
			lease, err := engine.ReserveOpen(session.Assignment().ID, metadata)
			if err != nil {
				_ = stream.Close()
				session.ReleaseStream()
				continue
			}
			mu.RLock()
			_, alreadyPresent := flows[metadata.FlowID]
			atCapacity := len(flows) >= defaultUDPFlows
			mu.RUnlock()
			if !alreadyPresent && atCapacity {
				_ = stream.Close()
				session.ReleaseStream()
				lease.Release()
				continue
			}
			approvedTarget, releaseEgress, err := engine.AcquireEgress(ctx, domain.ProtocolUDP, metadata.Target)
			if err != nil {
				_ = stream.Close()
				session.ReleaseStream()
				lease.Release()
				continue
			}
			addr, err := net.ResolveUDPAddr("udp", approvedTarget)
			if err != nil {
				_ = stream.Close()
				session.ReleaseStream()
				lease.Release()
				releaseEgress()
				continue
			}
			conn, err := net.DialUDP("udp", nil, addr)
			if err != nil {
				_ = stream.Close()
				session.ReleaseStream()
				lease.Release()
				releaseEgress()
				continue
			}
			entry := &flow{conn: conn, lease: lease, release: releaseEgress}
			entry.lastUnix.Store(time.Now().UnixNano())
			mu.Lock()
			if old := flows[metadata.FlowID]; old != nil {
				_ = old.conn.Close()
			}
			flows[metadata.FlowID] = entry
			mu.Unlock()
			_ = stream.Close()
			session.ReleaseStream()
			go func(flowID uint64, value *flow) {
				defer func() {
					_ = value.conn.Close()
					mu.Lock()
					if flows[flowID] == value {
						delete(flows, flowID)
					}
					mu.Unlock()
					if value.lease != nil {
						value.lease.Release()
					}
					if value.release != nil {
						value.release()
					}
				}()
				buffer := make([]byte, defaultDatagramMTU-afdp.DatagramHeaderSize())
				for {
					_ = value.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
					n, err := value.conn.Read(buffer)
					if err != nil {
						if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
							if ctx.Err() != nil {
								return
							}
							last := value.lastUnix.Load()
							if last == 0 || time.Since(time.Unix(0, last)) < defaultUDPTimeout {
								continue
							}
						}
						return
					}
					value.lastUnix.Store(time.Now().UnixNano())
					if err := session.SendDatagram(flowID, value.seq.Add(1)-1, buffer[:n], defaultDatagramMTU); err != nil {
						return
					}
				}
			}(metadata.FlowID, entry)
		}
	}()
	reassembler, err := afdp.NewReassembler(defaultUDPFlows, defaultUDPBytes, defaultDatagramMTU-afdp.DatagramHeaderSize(), defaultUDPTimeout)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-acceptErr:
			return err
		default:
		}
		header, payload, err := session.ReceiveDatagramPacket(ctx, reassembler)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		mu.RLock()
		entry := flows[header.FlowID]
		mu.RUnlock()
		if entry != nil {
			entry.lastUnix.Store(time.Now().UnixNano())
			if _, err := entry.conn.Write(payload); err != nil {
				return fmt.Errorf("write UDP local target: %w", err)
			}
		}
	}
}

func findReverseFlow(flows map[string]*udpReverseFlow, id uint64) *udpReverseFlow {
	for _, flow := range flows {
		if flow.id == id {
			return flow
		}
	}
	return nil
}

// writeReverseFlowPacket keeps the flow address protected by flowsMu through
// the UDP write. Expiration and shutdown can remove a flow concurrently, so
// copying the flow pointer and unlocking before WriteToUDP would allow a
// packet to be sent using stale flow state.
func writeReverseFlowPacket(socket *net.UDPConn, flowsMu *sync.Mutex, flows map[string]*udpReverseFlow, flowID uint64, payload []byte) error {
	flowsMu.Lock()
	defer flowsMu.Unlock()
	flow := findReverseFlow(flows, flowID)
	if flow == nil || flow.addr == nil {
		return nil
	}
	_, err := socket.WriteToUDP(payload, flow.addr)
	return err
}

func expireUDPReverseFlows(flows map[string]*udpReverseFlow, now time.Time, timeout time.Duration, session *afdp.Session, engine *Engine) {
	for key, flow := range flows {
		if flow.last.IsZero() || now.Sub(flow.last) < timeout {
			continue
		}
		delete(flows, key)
		if flow.stream != nil {
			_ = flow.stream.Close()
		}
		session.ReleaseStream()
		if flow.lease != nil {
			flow.lease.Release()
		}
	}
}
