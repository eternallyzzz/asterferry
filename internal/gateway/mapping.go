package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"asterferry/internal/cluster"
	"asterferry/internal/config"
	"asterferry/internal/lifecycle"
	"asterferry/internal/relay"
	"asterferry/internal/transport"
)

type Mapping struct {
	server     *Gateway
	session    *Session
	owner      cluster.Owner
	specMu     sync.RWMutex
	spec       transport.TunnelRegistration
	key        string
	ctx        context.Context
	cancel     context.CancelFunc
	tcp        net.Listener
	udp        *net.UDPConn
	once       sync.Once
	acceptOnce sync.Once
	acceptErr  error
	closeErr   error
	draining   atomic.Bool
}

func newMapping(server *Gateway, session *Session, spec transport.TunnelRegistration) (*Mapping, error) {
	ctx, cancel := context.WithCancel(session.ctx)
	if spec.GatewayBind == "" {
		spec.GatewayBind = config.DefaultReverseGatewayBind
	}
	m := &Mapping{server: server, session: session, owner: session.owner(server.nodeID), spec: spec, key: mappingKey(spec.Protocol, spec.GatewayBind, spec.GatewayPort), ctx: ctx, cancel: cancel}
	listenAddress := net.JoinHostPort(spec.GatewayBind, fmt.Sprint(spec.GatewayPort))
	if spec.Protocol == "tcp" {
		l, err := net.Listen("tcp", listenAddress)
		if err != nil {
			cancel()
			return nil, err
		}
		m.tcp = l
	} else {
		addr, err := net.ResolveUDPAddr("udp", listenAddress)
		if err != nil {
			cancel()
			return nil, err
		}
		u, err := net.ListenUDP("udp", addr)
		if err != nil {
			cancel()
			return nil, err
		}
		m.udp = u
	}
	return m, nil
}

func (m *Mapping) specSnapshot() transport.TunnelRegistration {
	if m == nil {
		return transport.TunnelRegistration{}
	}
	m.specMu.RLock()
	spec := m.spec
	m.specMu.RUnlock()
	return spec
}

func (m *Mapping) updateSpec(spec transport.TunnelRegistration) {
	if m == nil {
		return
	}
	m.specMu.Lock()
	m.spec = spec
	m.specMu.Unlock()
}

func (m *Mapping) Run() {
	if m.specSnapshot().Protocol == "tcp" {
		m.runTCP()
		return
	}
	m.runUDP()
}

// BeginDrain closes only the public listener. Existing TCP connections and
// UDP associations retain the mapping context until the final close.
func (m *Mapping) BeginDrain() {
	if m == nil {
		return
	}
	if m.draining.Swap(true) {
		// A concurrent first caller may still be inside acceptOnce; wait for it
		// so Close can safely read the recorded listener error.
		m.acceptOnce.Do(func() {})
		return
	}
	m.acceptOnce.Do(func() {
		if m.tcp != nil {
			m.acceptErr = m.tcp.Close()
		}
	})
}

// Ownership returns the local owner metadata associated with this mapping.
// It is intentionally not used for routing yet; future node coordination can
// inspect it without changing the reverse data path.
func (m *Mapping) Ownership() cluster.Owner {
	if m == nil {
		return cluster.Owner{}
	}
	return m.owner
}

func (m *Mapping) Close() error {
	m.once.Do(func() {
		m.server.logger.Info("mapping closing", "event", "gateway.mapping.closing", "mapping", m.specSnapshot().Name)
		m.BeginDrain()
		m.cancel()
		errs := []error{m.acceptErr}
		if m.udp != nil {
			errs = append(errs, m.udp.Close())
		}
		m.closeErr = errors.Join(errs...)
	})
	return m.closeErr
}

func (m *Mapping) runTCP() {
	defer func() {
		if !m.draining.Load() {
			_ = m.Close()
		}
	}()
	for {
		conn, err := m.tcp.Accept()
		if err != nil {
			return
		}
		if m.server.life != nil && !m.server.life.TryAdd() {
			_ = conn.Close()
			continue
		}
		release, allowed := m.acquireConnection()
		if !allowed {
			if m.server.life != nil {
				m.server.life.Done()
			}
			_ = conn.Close()
			m.server.metrics.MappingFailures.Add(1)
			continue
		}
		go func() {
			if m.server.life != nil {
				defer m.server.life.Done()
			}
			defer release()
			m.handleTCP(conn)
		}()
	}
}

func (m *Mapping) handleTCP(local net.Conn) {
	defer local.Close()
	spec := m.specSnapshot()
	stream, err := m.session.openStream(m.ctx)
	if err != nil {
		m.server.logger.Info("reverse stream open failed", "event", "gateway.reverse.open_failed", "mapping", spec.Name, "error_kind", lifecycle.ErrorKind(err))
		m.server.metrics.MappingFailures.Add(1)
		return
	}
	defer stream.Close()
	requestID := uint64(1)
	open := transport.MustMessageFrame(transport.TypeOpenReverse, requestID, transport.OpenReverse{Name: spec.Name, Protocol: "tcp", Profile: spec.Profile})
	if err := transport.WriteFrame(stream, open, m.session.maxFrame()); err != nil {
		m.server.logger.Info("reverse open write failed", "event", "gateway.reverse.open_write_failed", "mapping", spec.Name, "error_kind", lifecycle.ErrorKind(err))
		return
	}
	if !m.waitOpenOK(stream, requestID, spec.Name) {
		m.server.logger.Info("reverse open rejected", "event", "gateway.reverse.rejected", "mapping", spec.Name)
		return
	}
	m.server.metrics.ActiveStreams.Add(1)
	defer m.server.metrics.ActiveStreams.Add(-1)
	profile, err := m.session.relayProfile(spec.Profile)
	if err != nil {
		return
	}
	remote := relay.NewConn(stream, profile)
	relay.BidirectionalWithIdle(m.ctx, local, remote, time.Duration(m.server.cfg.Limits.RelayIdleTimeoutSec)*time.Second, relay.Counters{In: func(n uint64) { m.server.metrics.BytesIn.Add(n) }, Out: func(n uint64) { m.server.metrics.BytesOut.Add(n) }})
}

func (m *Mapping) waitOpenOK(stream transport.Stream, requestID uint64, mappingName string) bool {
	handshakeCtx, cancelHandshake := context.WithTimeout(m.ctx, time.Duration(m.server.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	f, err := transport.ReadFrame(stream, transport.HandshakeMaxFrame)
	cancelHandshake()
	stopStreamContext()
	_ = transport.SetStreamContext(stream, m.ctx)
	if err != nil {
		m.server.logger.Info("reverse status read failed", "event", "gateway.reverse.status_failed", "mapping", mappingName, "error_kind", lifecycle.ErrorKind(err))
		return false
	}
	if f.Type != transport.TypeOpenOK || f.RequestID != requestID {
		return false
	}
	var result transport.OpenResult
	if len(f.Payload) > 0 && transport.DecodeMessage(f, &result) == nil && result.Error != nil {
		return false
	}
	return true
}

type udpAssociation struct {
	stream  transport.Stream
	addr    *net.UDPAddr
	name    string
	profile string
	last    time.Time
	mu      sync.Mutex
	cancel  context.CancelFunc
	release func()
}

func (m *Mapping) runUDP() {
	defer m.Close()
	associations := map[string]*udpAssociation{}
	pending := map[string]struct{}{}
	var mu sync.Mutex
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			mu.Lock()
			stale := make([]*udpAssociation, 0, len(associations))
			for key, a := range associations {
				delete(associations, key)
				stale = append(stale, a)
			}
			clear(pending)
			mu.Unlock()
			for _, a := range stale {
				closeUDPAssociation(a)
			}
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-time.Duration(m.server.cfg.Limits.UDPIdleTimeoutSec) * time.Second)
			mu.Lock()
			stale := make([]*udpAssociation, 0)
			for key, a := range associations {
				if a.last.Before(cutoff) {
					delete(associations, key)
					stale = append(stale, a)
				}
			}
			mu.Unlock()
			for _, a := range stale {
				closeUDPAssociation(a)
			}
		default:
			buf := make([]byte, m.session.maxUDP())
			m.udp.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, addr, err := m.udp.ReadFromUDP(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
			key := addr.String()
			mu.Lock()
			a := associations[key]
			_, waiting := pending[key]
			var release func()
			reserved := false
			if a == nil && !waiting && !m.draining.Load() && (m.server.life == nil || m.server.life.IsRunning()) {
				release, reserved = m.acquireConnection()
				if reserved {
					pending[key] = struct{}{}
				}
			}
			mu.Unlock()

			if a == nil && !waiting && reserved {
				candidate := m.openUDPAssociation(addr, release)
				discard := candidate
				installed := false
				mu.Lock()
				delete(pending, key)
				if candidate != nil && associations[key] == nil && !m.draining.Load() && (m.server.life == nil || m.server.life.IsRunning()) {
					if m.server.life != nil && !m.server.life.TryAdd() {
						candidate = nil
					} else {
						associations[key] = candidate
						a = candidate
						installed = true
						association := candidate
						go func() {
							if m.server.life != nil {
								defer m.server.life.Done()
							}
							m.readUDPAssociation(m.ctx, association, key, associations, &mu)
						}()
					}
				}
				if a == nil {
					a = associations[key]
				}
				mu.Unlock()
				if !installed {
					closeUDPAssociation(discard)
				}
			}

			var failed *udpAssociation
			if a != nil {
				mu.Lock()
				if associations[key] == a {
					a.last = time.Now()
				}
				mu.Unlock()
				a.mu.Lock()
				f, frameErr := transport.MessageFrame(transport.TypeData, 0, transport.NewData(buf[:n], a.profile, m.session.maxPadding()))
				if frameErr != nil {
					err = frameErr
				} else {
					err = transport.WriteFrame(a.stream, f, m.session.maxFrame())
				}
				a.mu.Unlock()
				if err != nil {
					mu.Lock()
					if associations[key] == a {
						delete(associations, key)
					}
					mu.Unlock()
					failed = a
				}
			}
			if failed != nil {
				closeUDPAssociation(failed)
			}
			if err != nil {
				m.server.metrics.MappingFailures.Add(1)
			}
		}
	}
}

func (m *Mapping) openUDPAssociation(addr *net.UDPAddr, release func()) *udpAssociation {
	spec := m.specSnapshot()
	stream, err := m.session.openStream(m.ctx)
	if err != nil {
		if release != nil {
			release()
		}
		return nil
	}
	_, cancel := context.WithCancel(m.ctx)
	association := &udpAssociation{stream: stream, addr: addr, name: spec.Name, profile: spec.Profile, last: time.Now(), cancel: cancel, release: release}
	requestID := uint64(1)
	open := transport.MustMessageFrame(transport.TypeOpenReverse, requestID, transport.OpenReverse{Name: spec.Name, Protocol: "udp", Profile: spec.Profile})
	if err := transport.WriteFrame(stream, open, m.session.maxFrame()); err != nil || !m.waitOpenOK(stream, requestID, spec.Name) {
		closeUDPAssociation(association)
		return nil
	}
	return association
}

func closeUDPAssociation(association *udpAssociation) {
	if association == nil {
		return
	}
	if association.cancel != nil {
		association.cancel()
	}
	if association.stream != nil {
		_ = association.stream.Close()
	}
	if association.release != nil {
		association.release()
	}
}

func (m *Mapping) readUDPAssociation(ctx context.Context, a *udpAssociation, key string, associations map[string]*udpAssociation, mu *sync.Mutex) {
	defer func() {
		mu.Lock()
		if associations[key] == a {
			delete(associations, key)
		}
		mu.Unlock()
		_ = a.stream.Close()
		if a.release != nil {
			a.release()
		}
	}()
	for {
		f, err := transport.ReadFrame(a.stream, m.session.maxFrame())
		if err != nil {
			return
		}
		if f.Type != transport.TypeData {
			return
		}
		data, err := transport.DecodeData(f, m.session.maxUDP(), m.session.maxPadding())
		if err != nil {
			return
		}
		if _, err := m.udp.WriteToUDP(data.Payload, a.addr); err != nil {
			return
		}
		m.server.metrics.BytesOut.Add(uint64(len(data.Payload)))
		mu.Lock()
		if associations[key] == a {
			a.last = time.Now()
		}
		mu.Unlock()
	}
}

func (s *Session) openStream(ctx context.Context) (transport.Stream, error) {
	if s == nil || s.conn == nil {
		return nil, errors.New("session is closed")
	}
	return s.conn.OpenStream(ctx)
}

func (m *Mapping) acquireConnection() (func(), bool) {
	return m.session.acquireConnection()
}
