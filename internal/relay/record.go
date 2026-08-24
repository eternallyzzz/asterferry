package relay

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"asterferry/internal/protocol"
)

const recordHeaderSize = 8

const randomBufferSize = 32 << 10

// Profile controls the application record shape used inside an encrypted
// QUIC stream. The record layer hides the exact write boundaries and, for the
// balanced profile, adds bounded random padding. It never impersonates a
// different protocol.
type Profile struct {
	Name           string
	MaxRecordBytes int64
	MaxPadding     int64
}

func NewProfile(name string, maxRecordBytes, maxPadding int64) (Profile, error) {
	if name != "standard" && name != "balanced" {
		return Profile{}, fmt.Errorf("unsupported relay profile %q", name)
	}
	if maxRecordBytes < recordHeaderSize+1 {
		return Profile{}, errors.New("max record size is too small")
	}
	if maxPadding < 0 || maxPadding > maxRecordBytes-recordHeaderSize {
		return Profile{}, errors.New("max padding is out of range")
	}
	return Profile{Name: name, MaxRecordBytes: maxRecordBytes, MaxPadding: maxPadding}, nil
}

// Conn turns an ordinary byte stream into bounded application records. Both
// ends must use Conn; the underlying stream remains encrypted by QUIC.
type Conn struct {
	underlying io.ReadWriteCloser
	profile    Profile

	writeMu sync.Mutex
	readMu  sync.Mutex
	readBuf []byte

	randomMu  sync.Mutex
	randomBuf []byte
	randomPos int
}

func NewConn(underlying io.ReadWriteCloser, profile Profile) *Conn {
	return &Conn{underlying: underlying, profile: profile}
}

func (c *Conn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	maxPayload := int(c.profile.MaxRecordBytes) - recordHeaderSize
	written := 0
	for written < len(p) {
		end := written + maxPayload
		if end > len(p) {
			end = len(p)
		}
		if err := c.writeRecord(p[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

func (c *Conn) writeRecord(payload []byte) error {
	padding := c.paddingLen(len(payload))
	var header [recordHeaderSize]byte
	header[0] = protocol.RelayRecordVersion
	binary.BigEndian.PutUint32(header[2:6], uint32(len(payload)))
	binary.BigEndian.PutUint16(header[6:8], uint16(padding))
	if err := writeAll(c.underlying, header[:]); err != nil {
		return err
	}
	if err := writeAll(c.underlying, payload); err != nil {
		return err
	}
	if padding > 0 {
		if err := c.writeRandom(padding); err != nil {
			return err
		}
	}
	if flusher, ok := c.underlying.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return nil
}

func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.readBuf) == 0 {
		payload, err := c.readRecord()
		if err != nil {
			return 0, err
		}
		if len(payload) > 0 {
			c.readBuf = payload
		}
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *Conn) readRecord() ([]byte, error) {
	header := make([]byte, recordHeaderSize)
	if _, err := io.ReadFull(c.underlying, header); err != nil {
		return nil, err
	}
	if header[0] != protocol.RelayRecordVersion || header[1] != 0 {
		return nil, errors.New("invalid relay record header")
	}
	payloadLen := int64(binary.BigEndian.Uint32(header[2:6]))
	paddingLen := int64(binary.BigEndian.Uint16(header[6:8]))
	if payloadLen == 0 || payloadLen+paddingLen+recordHeaderSize > c.profile.MaxRecordBytes || paddingLen > c.profile.MaxPadding {
		return nil, errors.New("relay record exceeds configured limit")
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(c.underlying, payload); err != nil {
		return nil, err
	}
	if paddingLen > 0 {
		padding := make([]byte, paddingLen)
		if _, err := io.ReadFull(c.underlying, padding); err != nil {
			return nil, err
		}
	}
	return payload, nil
}

func (c *Conn) paddingLen(payloadLen int) int {
	if c.profile.Name != "balanced" || c.profile.MaxPadding == 0 {
		return 0
	}
	base := int64(recordHeaderSize + payloadLen)
	for _, bucket := range []int{512, 1024, 2048, 4096, 8192, 16384} {
		if int64(bucket) < base || int64(bucket)-base > c.profile.MaxPadding {
			continue
		}
		available := int64(bucket) - base
		if available == 0 {
			return 0
		}
		var random [2]byte
		if err := c.randomBytes(random[:]); err != nil {
			return int(available)
		}
		return int(int64(binary.BigEndian.Uint16(random[:])) % (available + 1))
	}
	return 0
}

func (c *Conn) randomBytes(dst []byte) error {
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

func (c *Conn) writeRandom(size int) error {
	if size <= 0 {
		return nil
	}
	c.randomMu.Lock()
	defer c.randomMu.Unlock()
	for size > 0 {
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
		n := len(c.randomBuf) - c.randomPos
		if n > size {
			n = size
		}
		if err := writeAll(c.underlying, c.randomBuf[c.randomPos:c.randomPos+n]); err != nil {
			return err
		}
		c.randomPos += n
		size -= n
	}
	return nil
}

// PaddingLength returns a bounded random padding length for a payload already
// preceded by headerBytes bytes of record metadata.
func PaddingLength(profile string, headerBytes, maxPadding int64) int {
	if profile != "balanced" || maxPadding == 0 {
		return 0
	}
	base := headerBytes
	for _, bucket := range []int{512, 1024, 2048, 4096, 8192, 16384} {
		if int64(bucket) < base || int64(bucket)-base > maxPadding {
			continue
		}
		available := int64(bucket) - base
		if available == 0 {
			return 0
		}
		var b [2]byte
		if _, err := rand.Read(b[:]); err != nil {
			return int(available)
		}
		return int(int64(binary.BigEndian.Uint16(b[:])) % (available + 1))
	}
	return 0
}

func (c *Conn) Close() error { return c.underlying.Close() }

func (c *Conn) CloseRead() {
	if v, ok := c.underlying.(interface{ CloseRead() }); ok {
		v.CloseRead()
	}
}

func (c *Conn) CloseWrite() {
	if v, ok := c.underlying.(interface{ CloseWrite() }); ok {
		v.CloseWrite()
		return
	}
	_ = c.underlying.Close()
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
