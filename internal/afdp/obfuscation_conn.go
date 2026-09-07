package afdp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

const (
	obfuscationVersion  byte = Version
	obfuscationData     byte = 0
	obfuscationFragment byte = 1

	// A 128-bit per-packet salt keeps the keystream collision probability
	// negligible even at sustained multi-gigabit packet rates.
	obfuscationSaltBytes = 16
	obfuscationTagBytes  = 8
	fragmentHeaderBytes  = 10
	dataHeaderBytes      = 2

	maxFragmentCount       = 8
	maxReassemblyEntries   = 128
	maxReassemblyPerSource = 16
	maxReassemblyBytes     = 1 << 20
	reassemblyTTL          = time.Second
	randomBufferSize       = 32 << 10
)

const (
	obfuscationMaskDomain = "asterferry-data/2/mask/"
	obfuscationTagDomain  = "asterferry-data/2/tag/"
	maskInputBytes        = len(obfuscationMaskDomain) + 32 + obfuscationSaltBytes + 4
)

var (
	obfuscationReadPool = sync.Pool{New: func() any {
		return &obfuscationPoolBuffer{bytes: make([]byte, 64<<10)}
	}}
	obfuscationBodyPool = sync.Pool{New: func() any {
		return &obfuscationPoolBuffer{bytes: make([]byte, 16<<10)}
	}}
)

type obfuscationPoolBuffer struct {
	bytes []byte
}

type obfuscationKey struct {
	key [32]byte
}

// ObfuscationMetrics is deliberately tiny so an embedding runtime can publish
// packet-layer counters without coupling the wire package to a metrics store.
type ObfuscationMetrics interface {
	ObfuscationPacketAccepted(previousKey bool)
	ObfuscationPacketRejected()
	ObfuscationFragmentDropped()
}

const (
	ObfuscationStandard   = "standard"
	ObfuscationCamouflage = "camouflage"
)

// ObfuscationOptions is the data-plane-only representation of the negotiated
// packet camouflage policy. Keys are supplied by the authenticated node
// snapshot; persistence/encryption of those keys belongs to the Controller.
type ObfuscationOptions struct {
	Mode             string
	CurrentKey       []byte
	PreviousKey      []byte
	HandshakeShaping bool
	MinFragmentBytes int
	MaxFragmentBytes int
	// MaxHandshakeFragmentWireBytes is the maximum on-wire size of one
	// handshake camouflage fragment. Short-header data datagrams may contain
	// coalesced QUIC packets and intentionally use the separate overall AFDP
	// datagram limit instead.
	MaxHandshakeFragmentWireBytes int
}

type obfuscationPacketConn struct {
	conn    *net.UDPConn
	keys    []obfuscationKey
	opts    ObfuscationOptions
	metrics ObfuscationMetrics

	writeMu  sync.Mutex
	stateMu  sync.Mutex
	parts    map[fragmentKey]*fragmentAssembly
	sources  map[string]int
	bytes    int
	closeMu  sync.Once
	closeErr error

	// QUIC calls WriteTo concurrently from multiple goroutines. writeMu
	// serializes the wrapper, allowing these scratch buffers to be reused for
	// every datagram without per-packet heap churn.
	writeBody []byte
	writeWire []byte

	randomMu  sync.Mutex
	randomBuf []byte
	randomPos int
}

type fragmentKey struct {
	source string
	id     uint16
}

type fragmentAssembly struct {
	total    byte
	parts    [][]byte
	bytes    int
	deadline time.Time
}

// NewObfuscatingPacketConn wraps a UDP socket with AsterFerry's versioned
// datagram camouflage. Standard mode is returned unchanged so quic-go keeps
// its native ECN/GSO/batched I/O fast path.
func NewObfuscatingPacketConn(conn *net.UDPConn, opts ObfuscationOptions, metrics ...ObfuscationMetrics) (net.PacketConn, error) {
	if conn == nil {
		return nil, errors.New("UDP socket is nil")
	}
	if opts.Mode == "" {
		opts.Mode = ObfuscationStandard
	}
	if opts.MinFragmentBytes == 0 {
		opts.MinFragmentBytes = 512
	}
	if opts.MaxFragmentBytes == 0 {
		opts.MaxFragmentBytes = 1200
	}
	if opts.MaxHandshakeFragmentWireBytes == 0 {
		opts.MaxHandshakeFragmentWireBytes = 1280
	}
	if opts.MinFragmentBytes < fragmentHeaderBytes+obfuscationSaltBytes+obfuscationTagBytes || opts.MaxFragmentBytes < opts.MinFragmentBytes || opts.MaxHandshakeFragmentWireBytes < opts.MaxFragmentBytes || opts.MaxHandshakeFragmentWireBytes > maxObfuscationDatagram {
		return nil, errors.New("invalid AFDP obfuscation packet limits")
	}
	if opts.Mode == ObfuscationStandard {
		return conn, nil
	}
	if opts.Mode != ObfuscationCamouflage {
		return nil, fmt.Errorf("unsupported transport obfuscation mode %q", opts.Mode)
	}
	keys := make([]obfuscationKey, 0, 2)
	for _, raw := range [][]byte{opts.CurrentKey, opts.PreviousKey} {
		if len(raw) == 0 {
			continue
		}
		digest := sha256.Sum256(raw)
		keys = append(keys, obfuscationKey{key: digest})
	}
	if len(keys) == 0 {
		return nil, errors.New("camouflage mode requires a transport obfuscation key")
	}
	var metricSink ObfuscationMetrics
	if len(metrics) > 0 {
		metricSink = metrics[0]
	}
	return &obfuscationPacketConn{
		conn:    conn,
		keys:    keys,
		opts:    opts,
		metrics: metricSink,
		parts:   make(map[fragmentKey]*fragmentAssembly),
		sources: make(map[string]int),
	}, nil
}

func (c *obfuscationPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if c == nil || c.conn == nil {
		return 0, nil, net.ErrClosed
	}
	pooled := obfuscationReadPool.Get().(*obfuscationPoolBuffer)
	buffer := pooled.bytes
	if required := maxInt(64<<10, int(c.opts.MaxHandshakeFragmentWireBytes)+256); cap(buffer) < required {
		buffer = make([]byte, required)
		pooled.bytes = buffer
	}
	buffer = buffer[:cap(buffer)]
	defer func() {
		pooled.bytes = buffer[:64<<10]
		obfuscationReadPool.Put(pooled)
	}()
	for {
		n, addr, err := c.conn.ReadFrom(buffer)
		if err != nil {
			return 0, addr, err
		}
		packet, ok := c.decode(buffer[:n], addr, p)
		if !ok {
			// Invalid packets are intentionally indistinguishable from an idle
			// socket to an unauthenticated observer.
			continue
		}
		if len(packet) > len(p) {
			continue
		}
		if len(packet) > 0 && (len(p) == 0 || &packet[0] != &p[0]) {
			copy(p, packet)
		}
		return len(packet), addr, nil
	}
}

func (c *obfuscationPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if c == nil || c.conn == nil {
		return 0, net.ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// Only QUIC long-header handshake datagrams are shaped. QUIC may coalesce
	// several short-header packets into one datagram; that datagram is masked as
	// one unit so packet coalescing is preserved and the data path stays cheap.
	if len(p) > 0 && c.opts.HandshakeShaping && p[0]&0x80 != 0 {
		if err := c.writeFragments(p, addr); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if err := c.writeData(p, addr); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *obfuscationPacketConn) Close() error {
	if c == nil {
		return nil
	}
	c.closeMu.Do(func() {
		if c.conn != nil {
			c.closeErr = c.conn.Close()
		}
	})
	return c.closeErr
}

func (c *obfuscationPacketConn) LocalAddr() net.Addr { return c.conn.LocalAddr() }

func (c *obfuscationPacketConn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

func (c *obfuscationPacketConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *obfuscationPacketConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

func (c *obfuscationPacketConn) SetReadBuffer(bytes int) error {
	return c.conn.SetReadBuffer(bytes)
}

func (c *obfuscationPacketConn) SetWriteBuffer(bytes int) error {
	return c.conn.SetWriteBuffer(bytes)
}
