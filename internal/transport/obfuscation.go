package transport

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"

	"asterferry/internal/config"
	"asterferry/internal/protocol"
)

const (
	obfuscationVersion  byte = protocol.ObfuscationVersion
	obfuscationData     byte = 0
	obfuscationFragment byte = 1

	obfuscationSaltBytes = 8
	obfuscationTagBytes  = 8
	fragmentHeaderBytes  = 10
	dataHeaderBytes      = 2

	maxFragmentCount       = 8
	maxReassemblyEntries   = 128
	maxReassemblyPerSource = 16
	maxReassemblyBytes     = 1 << 20
	maxObfuscationDatagram = 64 << 10
	reassemblyTTL          = time.Second
	randomBufferSize       = 32 << 10
)

const (
	obfuscationMaskDomain = protocol.DatagramMaskDomain
	obfuscationTagDomain  = protocol.DatagramTagDomain
	maskInputBytes        = len(obfuscationMaskDomain) + 32 + obfuscationSaltBytes + 4
	tagInputBytes         = 2048
)

var (
	obfuscationReadPool = sync.Pool{New: func() any { return make([]byte, 64<<10) }}
	obfuscationBodyPool = sync.Pool{New: func() any { return make([]byte, 16<<10) }}
)

type obfuscationKey struct {
	key [32]byte
}

// ObfuscationMetrics is deliberately tiny so the transport package can
// publish packet-layer counters without importing the observability package.
type ObfuscationMetrics interface {
	ObfuscationPacketAccepted(previousKey bool)
	ObfuscationPacketRejected()
	ObfuscationFragmentDropped()
}

type obfuscationPacketConn struct {
	conn    *net.UDPConn
	keys    []obfuscationKey
	opts    config.TransportObfuscationOptions
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
	writeMask []byte
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
func NewObfuscatingPacketConn(conn *net.UDPConn, opts config.TransportObfuscationOptions, metrics ...ObfuscationMetrics) (net.PacketConn, error) {
	if conn == nil {
		return nil, errors.New("UDP socket is nil")
	}
	if opts.Mode == "" {
		opts.Mode = config.TransportObfuscationStandard
	}
	if opts.MinFragmentBytes == 0 {
		opts.MinFragmentBytes = 512
	}
	if opts.MaxFragmentBytes == 0 {
		opts.MaxFragmentBytes = 1200
	}
	if opts.MaxWirePacketBytes == 0 {
		opts.MaxWirePacketBytes = 1280
	}
	if opts.Mode == config.TransportObfuscationStandard {
		return conn, nil
	}
	if opts.Mode != config.TransportObfuscationCamouflage {
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
	buffer := obfuscationReadPool.Get().([]byte)
	if required := maxInt(64<<10, int(c.opts.MaxWirePacketBytes)+256); cap(buffer) < required {
		buffer = make([]byte, required)
	}
	buffer = buffer[:cap(buffer)]
	defer obfuscationReadPool.Put(buffer[:64<<10])
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
	wire, err := c.encodeData(p)
	if err != nil {
		return 0, err
	}
	if _, err := c.conn.WriteTo(wire, addr); err != nil {
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

func (c *obfuscationPacketConn) encodeData(payload []byte) ([]byte, error) {
	body := c.writeBuffer(&c.writeBody, dataHeaderBytes+len(payload))
	body[0] = obfuscationVersion
	body[1] = obfuscationData
	copy(body[dataHeaderBytes:], payload)
	wire, err := c.seal(body)
	if err != nil {
		return nil, err
	}
	if len(wire) > maxObfuscationDatagram {
		return nil, errors.New("camouflage data datagram is too large")
	}
	return wire, nil
}

func (c *obfuscationPacketConn) writeFragments(packet []byte, addr net.Addr) error {
	maxWire := int(c.opts.MaxFragmentBytes)
	if maxWire <= fragmentHeaderBytes+obfuscationSaltBytes+obfuscationTagBytes {
		return errors.New("transport obfuscation fragment size is too small")
	}
	chunkSize := maxWire - fragmentHeaderBytes - obfuscationSaltBytes - obfuscationTagBytes
	if chunkSize < 1 {
		return errors.New("transport obfuscation fragment payload is too small")
	}
	total := (len(packet) + chunkSize - 1) / chunkSize
	if total < 2 {
		total = 2
		chunkSize = (len(packet) + total - 1) / total
	}
	if total > maxFragmentCount {
		return errors.New("QUIC datagram requires too many camouflage fragments")
	}
	var idBytes [2]byte
	if err := c.randomBytes(idBytes[:]); err != nil {
		return err
	}
	id := binary.BigEndian.Uint16(idBytes[:])
	for index, offset := 0, 0; offset < len(packet); index++ {
		end := minInt(len(packet), offset+chunkSize)
		chunk := packet[offset:end]
		body, err := c.fragmentBody(id, byte(index), byte(total), chunk)
		if err != nil {
			return err
		}
		wire, err := c.seal(body)
		if err != nil {
			return err
		}
		if len(wire) > int(c.opts.MaxWirePacketBytes) {
			return errors.New("camouflage fragment exceeds max wire packet size")
		}
		_, writeErr := c.conn.WriteTo(wire, addr)
		if writeErr != nil {
			return writeErr
		}
		offset = end
	}
	return nil
}

func (c *obfuscationPacketConn) fragmentBody(id uint16, index, total byte, payload []byte) ([]byte, error) {
	minWire := int(c.opts.MinFragmentBytes)
	maxWire := int(c.opts.MaxFragmentBytes)
	base := fragmentHeaderBytes + obfuscationSaltBytes + obfuscationTagBytes + len(payload)
	if base > maxWire {
		return nil, errors.New("camouflage fragment payload exceeds configured size")
	}
	minPadding := maxInt(0, minWire-base)
	maxPadding := maxInt(0, maxWire-base)
	padding := minPadding
	if maxPadding > minPadding {
		var randomBytes [2]byte
		if err := c.randomBytes(randomBytes[:]); err != nil {
			return nil, err
		}
		padding += int(binary.BigEndian.Uint16(randomBytes[:])) % (maxPadding - minPadding + 1)
	}
	body := c.writeBuffer(&c.writeBody, fragmentHeaderBytes+len(payload)+padding)
	body[0] = obfuscationVersion
	body[1] = obfuscationFragment
	binary.BigEndian.PutUint16(body[2:4], id)
	body[4] = index
	body[5] = total
	binary.BigEndian.PutUint16(body[6:8], uint16(len(payload)))
	binary.BigEndian.PutUint16(body[8:10], uint16(padding))
	copy(body[fragmentHeaderBytes:], payload)
	if padding > 0 {
		if err := c.randomBytes(body[fragmentHeaderBytes+len(payload):]); err != nil {
			return nil, err
		}
	}
	return body, nil
}

func (c *obfuscationPacketConn) seal(body []byte) ([]byte, error) {
	var salt [obfuscationSaltBytes]byte
	if err := c.randomBytes(salt[:]); err != nil {
		return nil, err
	}
	masked := c.writeBuffer(&c.writeMask, len(body))
	copy(masked, body)
	c.mask(masked, salt[:], c.keys[0].key)
	tag := c.tag(salt[:], masked, c.keys[0].key)
	wire := c.writeBuffer(&c.writeWire, len(salt)+len(masked)+len(tag))
	copy(wire, salt[:])
	copy(wire[len(salt):], masked)
	copy(wire[len(salt)+len(masked):], tag[:])
	return wire, nil
}

func (c *obfuscationPacketConn) decode(wire []byte, addr net.Addr, dst []byte) ([]byte, bool) {
	if len(wire) < obfuscationSaltBytes+obfuscationTagBytes+dataHeaderBytes || len(wire) > maxObfuscationDatagram {
		return nil, false
	}
	salt := wire[:obfuscationSaltBytes]
	masked := wire[obfuscationSaltBytes : len(wire)-obfuscationTagBytes]
	gotTag := wire[len(wire)-obfuscationTagBytes:]
	for _, key := range c.keys {
		expected := c.tag(salt, masked, key.key)
		if !hmac.Equal(expected[:], gotTag) {
			continue
		}
		if c.metrics != nil {
			c.metrics.ObfuscationPacketAccepted(key != c.keys[0])
		}
		body := obfuscationBodyPool.Get().([]byte)
		if cap(body) < len(masked) {
			body = make([]byte, len(masked))
		}
		body = body[:len(masked)]
		copy(body, masked)
		c.mask(body, salt, key.key)
		if len(body) < dataHeaderBytes || body[0] != obfuscationVersion {
			if c.metrics != nil {
				c.metrics.ObfuscationFragmentDropped()
			}
			putObfuscationBody(body)
			return nil, false
		}
		switch body[1] {
		case obfuscationData:
			payload := body[dataHeaderBytes:]
			if len(payload) > len(dst) {
				if c.metrics != nil {
					c.metrics.ObfuscationFragmentDropped()
				}
				putObfuscationBody(body)
				return nil, false
			}
			copy(dst, payload)
			putObfuscationBody(body)
			return dst[:len(payload)], true
		case obfuscationFragment:
			packet, ok := c.acceptFragment(body, addr)
			putObfuscationBody(body)
			return packet, ok
		default:
			if c.metrics != nil {
				c.metrics.ObfuscationFragmentDropped()
			}
			putObfuscationBody(body)
			return nil, false
		}
	}
	if c.metrics != nil {
		c.metrics.ObfuscationPacketRejected()
	}
	return nil, false
}

func (c *obfuscationPacketConn) acceptFragment(body []byte, addr net.Addr) ([]byte, bool) {
	if len(body) < fragmentHeaderBytes {
		if c.metrics != nil {
			c.metrics.ObfuscationFragmentDropped()
		}
		return nil, false
	}
	id := binary.BigEndian.Uint16(body[2:4])
	index := body[4]
	total := body[5]
	payloadLen := int(binary.BigEndian.Uint16(body[6:8]))
	paddingLen := int(binary.BigEndian.Uint16(body[8:10]))
	if total < 2 || total > maxFragmentCount || index >= total || payloadLen == 0 || fragmentHeaderBytes+payloadLen+paddingLen != len(body) || len(body)+obfuscationSaltBytes+obfuscationTagBytes > int(c.opts.MaxWirePacketBytes) {
		if c.metrics != nil {
			c.metrics.ObfuscationFragmentDropped()
		}
		return nil, false
	}
	source := ""
	if addr != nil {
		source = addr.String()
	}
	now := time.Now()
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.evictFragmentsLocked(now)
	key := fragmentKey{source: source, id: id}
	assembly := c.parts[key]
	if assembly == nil {
		if c.sources[source] >= maxReassemblyPerSource || len(c.parts) >= maxReassemblyEntries || c.bytes+payloadLen > maxReassemblyBytes {
			if c.metrics != nil {
				c.metrics.ObfuscationFragmentDropped()
			}
			return nil, false
		}
		assembly = &fragmentAssembly{total: total, parts: make([][]byte, total), deadline: now.Add(reassemblyTTL)}
		c.parts[key] = assembly
		c.sources[source]++
	}
	if assembly.total != total || assembly.parts[index] != nil {
		if c.metrics != nil {
			c.metrics.ObfuscationFragmentDropped()
		}
		return nil, false
	}
	payload := append([]byte(nil), body[fragmentHeaderBytes:fragmentHeaderBytes+payloadLen]...)
	assembly.parts[index] = payload
	assembly.bytes += len(payload)
	c.bytes += len(payload)
	for _, part := range assembly.parts {
		if part == nil {
			return nil, false
		}
	}
	packet := make([]byte, 0, assembly.bytes)
	for _, part := range assembly.parts {
		packet = append(packet, part...)
	}
	delete(c.parts, key)
	c.sources[source]--
	c.bytes -= assembly.bytes
	if len(packet) == 0 {
		if c.metrics != nil {
			c.metrics.ObfuscationFragmentDropped()
		}
		return nil, false
	}
	return packet, true
}

func (c *obfuscationPacketConn) evictFragmentsLocked(now time.Time) {
	for key, assembly := range c.parts {
		if now.Before(assembly.deadline) {
			continue
		}
		delete(c.parts, key)
		c.sources[key.source]--
		c.bytes -= assembly.bytes
	}
}

func (c *obfuscationPacketConn) mask(data, salt []byte, key [32]byte) {
	var input [maskInputBytes]byte
	copy(input[:], obfuscationMaskDomain)
	copy(input[len(obfuscationMaskDomain):], key[:])
	copy(input[len(obfuscationMaskDomain)+len(key):], salt)
	counterOffset := len(obfuscationMaskDomain) + len(key) + len(salt)
	for offset, counter := 0, uint32(0); offset < len(data); counter++ {
		var counterBytes [4]byte
		binary.BigEndian.PutUint32(counterBytes[:], counter)
		copy(input[counterOffset:], counterBytes[:])
		blockBytes := blake2b.Sum256(input[:])
		for i := 0; i < len(blockBytes) && offset+i < len(data); i++ {
			data[offset+i] ^= blockBytes[i]
		}
		offset += len(blockBytes)
	}
}

func (c *obfuscationPacketConn) tag(salt, masked []byte, key [32]byte) [obfuscationTagBytes]byte {
	var input [tagInputBytes]byte
	length := copy(input[:], obfuscationTagDomain)
	length += copy(input[length:], key[:])
	length += copy(input[length:], salt)
	if length+len(masked) > len(input) {
		// Configuration validation keeps wire packets below this limit. Keep
		// this guard for direct callers that construct runtime options manually.
		masked = masked[:len(input)-length]
	}
	copy(input[length:], masked)
	digest := blake2b.Sum256(input[:length+len(masked)])
	var tag [obfuscationTagBytes]byte
	copy(tag[:], digest[:obfuscationTagBytes])
	return tag
}

func putObfuscationBody(buffer []byte) {
	if cap(buffer) >= 16<<10 {
		obfuscationBodyPool.Put(buffer[:16<<10])
	}
}

func (c *obfuscationPacketConn) writeBuffer(buffer *[]byte, size int) []byte {
	if cap(*buffer) < size {
		*buffer = make([]byte, size)
	}
	return (*buffer)[:size]
}

// randomBytes amortizes operating-system CSPRNG calls across packets while
// retaining cryptographically random output. It is used only for salts,
// fragment IDs, and camouflage padding; the packet key and tag construction
// remain unchanged.
func (c *obfuscationPacketConn) randomBytes(dst []byte) error {
	if len(dst) == 0 {
		return nil
	}
	c.randomMu.Lock()
	defer c.randomMu.Unlock()
	for len(dst) > 0 {
		if c.randomPos >= len(c.randomBuf) {
			if cap(c.randomBuf) < randomBufferSize {
				c.randomBuf = make([]byte, randomBufferSize)
			} else {
				c.randomBuf = c.randomBuf[:randomBufferSize]
			}
			if _, err := rand.Read(c.randomBuf); err != nil {
				return err
			}
			c.randomPos = 0
		}
		n := copy(dst, c.randomBuf[c.randomPos:])
		c.randomPos += n
		dst = dst[n:]
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
