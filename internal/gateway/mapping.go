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
	"asterferry/internal/relay"
	"asterferry/internal/transport"
)

type Mapping struct {
	server     *Gateway
	session    *Session
	owner      cluster.Owner
	spec       transport.TunnelRegistration
	key        string
	ctx        context.Context
	cancel     context.CancelFunc
	tcp        net.Listener
	udp        *net.UDPConn
	once       sync.Once
	acceptOnce sync.Once
	draining   atomic.Bool
}

func newMapping(server *Gateway, session *Session, spec transport.TunnelRegistration) (*Mapping, error) {
	ctx, cancel := context.WithCancel(session.ctx)
	m := &Mapping{server: server, session: session, owner: session.owner(server.nodeID), spec: spec, key: mappingKey(spec.Protocol, spec.GatewayPort), ctx: ctx, cancel: cancel}
	if spec.Protocol == "tcp" {
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", spec.GatewayPort))
		if err != nil {
			cancel()
			return nil, err
		}
		m.tcp = l
	} else {
		u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv6zero, Port: int(spec.GatewayPort)})
		if err != nil {
			cancel()
			return nil, err
		}
		m.udp = u
	}
	return m, nil
}

func (m *Mapping) Run() {
	if m.spec.Protocol == "tcp" {
		m.runTCP()
		return
	}
	m.runUDP()
}

// BeginDrain closes only the public listener. Existing TCP connections and
// UDP associations retain the mapping context until the final close.
func (m *Mapping) BeginDrain() {
	if m == nil || m.draining.Swap(true) {
		return
	}
	m.acceptOnce.Do(func() {
		if m.tcp != nil {
			_ = m.tcp.Close()
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
	var err error
	m.once.Do(func() {
		m.server.logger.Info("mapping closing", "event", "gateway.mapping.closing", "mapping", m.spec.Name)
		m.BeginDrain()
		m.cancel()
		if m.udp != nil {
			err = m.udp.Close()
		}
	})
	return err
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
		go func() {
			if m.server.life != nil {
				defer m.server.life.Done()
			}
			m.handleTCP(conn)
		}()
	}
}

func (m *Mapping) handleTCP(local net.Conn) {
	defer local.Close()
	release, ok := m.acquireConnection()
	if !ok {
		m.server.metrics.MappingFailures.Add(1)
		return
	}
	defer release()
	stream, err := m.session.openStream(m.ctx)
	if err != nil {
		m.server.logger.Info("reverse stream open failed", "event", "gateway.reverse.open_failed", "mapping", m.spec.Name, "error_kind", errorKind(err))
		m.server.metrics.MappingFailures.Add(1)
		return
	}
	defer stream.Close()
	open, _ := transport.MessageFrame(transport.TypeOpenReverse, 0, transport.OpenReverse{Name: m.spec.Name, Protocol: "tcp", Profile: m.spec.Profile})
	if err := writeFrame(stream, open, m.session.maxFrame()); err != nil {
		m.server.logger.Info("reverse open write failed", "event", "gateway.reverse.open_write_failed", "mapping", m.spec.Name, "error_kind", errorKind(err))
		return
	}
	if !m.waitOpenOK(stream) {
		m.server.logger.Info("reverse open rejected", "event", "gateway.reverse.rejected", "mapping", m.spec.Name)
		return
	}
	m.server.metrics.ActiveStreams.Add(1)
	defer m.server.metrics.ActiveStreams.Add(-1)
	profile, err := m.session.relayProfile(m.spec.Profile)
	if err != nil {
		return
	}
	remote := relay.NewConn(stream, profile)
	relay.Bidirectional(local, remote, relay.Counters{In: func(n uint64) { m.server.metrics.BytesIn.Add(n) }, Out: func(n uint64) { m.server.metrics.BytesOut.Add(n) }})
}

func (m *Mapping) waitOpenOK(stream transport.Stream) bool {
	handshakeCtx, cancelHandshake := context.WithTimeout(m.ctx, time.Duration(m.server.cfg.Transport.HandshakeTimeoutSec)*time.Second)
	stopStreamContext := transport.SetStreamContext(stream, handshakeCtx)
	f, err := transport.ReadFrame(stream, m.session.maxFrame())
	cancelHandshake()
	stopStreamContext()
	stopStreamContext = transport.SetStreamContext(stream, m.ctx)
	if err != nil {
		m.server.logger.Info("reverse status read failed", "event", "gateway.reverse.status_failed", "mapping", m.spec.Name, "error_kind", errorKind(err))
		return false
	}
	if f.Type != transport.TypeOpenOK {
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
	last    time.Time
	mu      sync.Mutex
	cancel  context.CancelFunc
	release func()
}

func (m *Mapping) runUDP() {
	defer m.Close()
	associations := map[string]*udpAssociation{}
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
			mu.Unlock()
			for _, a := range stale {
				a.cancel()
				_ = a.stream.Close()
				if a.release != nil {
					a.release()
				}
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
				a.cancel()
				_ = a.stream.Close()
				if a.release != nil {
					a.release()
				}
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
			if a == nil {
				if m.draining.Load() || (m.server.life != nil && !m.server.life.IsRunning()) {
					mu.Unlock()
					continue
				}
				release, allowed := m.acquireConnection()
				if !allowed {
					mu.Unlock()
					m.server.metrics.MappingFailures.Add(1)
					continue
				}
				stream, openErr := m.session.openStream(m.ctx)
				if openErr == nil {
					ctx, cancel := context.WithCancel(m.ctx)
					a = &udpAssociation{stream: stream, addr: addr, last: time.Now(), cancel: cancel, release: release}
					open, _ := transport.MessageFrame(transport.TypeOpenReverse, 0, transport.OpenReverse{Name: m.spec.Name, Protocol: "udp", Profile: m.spec.Profile})
					if writeErr := writeFrame(stream, open, m.session.maxFrame()); writeErr == nil && m.waitOpenOK(stream) {
						if m.server.life != nil && !m.server.life.TryAdd() {
							_ = stream.Close()
							cancel()
							release()
							a = nil
						} else {
							associations[key] = a
							association := a
							go func() {
								if m.server.life != nil {
									defer m.server.life.Done()
								}
								m.readUDPAssociation(ctx, association, key, associations, &mu)
							}()
						}
					} else {
						_ = stream.Close()
						cancel()
						release()
						a = nil
					}
				} else {
					release()
				}
			}
			var failed *udpAssociation
			if a != nil {
				a.last = time.Now()
				a.mu.Lock()
				f, _ := transport.MessageFrame(transport.TypeData, 0, transport.NewData(buf[:n], m.spec.Profile, m.session.maxPadding()))
				err = writeFrame(a.stream, f, m.session.maxFrame())
				a.mu.Unlock()
				if err != nil {
					if associations[key] == a {
						delete(associations, key)
					}
					failed = a
				}
			}
			mu.Unlock()
			if failed != nil {
				failed.cancel()
				_ = failed.stream.Close()
				if failed.release != nil {
					failed.release()
				}
			}
			if err != nil {
				m.server.metrics.MappingFailures.Add(1)
			}
		}
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
