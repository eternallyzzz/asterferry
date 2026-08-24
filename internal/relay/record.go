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

const (
	recordHeaderSize      = 12
	defaultWriteBatchSize = 256 << 10
	maxWriteBatchSize     = 4 << 20
	randomBufferSize      = 32 << 10
	discardBufferSize     = 32 << 10
	recordFlagPadding     = 1
	recordFlagMask        = recordFlagPadding
)

// Profile controls the application record shape used inside an encrypted
// QUIC stream. The balanced profile adds bounded random padding; it never
// impersonates a different protocol.
type Profile struct {
	Name               string
	MaxRecordBytes     int64
	MaxPadding         int64
	MaxWriteBatchBytes int64
}

func NewProfile(name string, maxRecordBytes, maxPadding int64) (Profile, error) {
	return NewProfileWithBatch(name, maxRecordBytes, maxPadding, 0)
}

func NewProfileWithBatch(name string, maxRecordBytes, maxPadding, maxWriteBatchBytes int64) (Profile, error) {
	if name != "standard" && name != "balanced" {
		return Profile{}, fmt.Errorf("unsupported relay profile %q", name)
	}
	if maxRecordBytes < recordHeaderSize+1 {
		return Profile{}, errors.New("max record size is too small")
	}
	if maxPadding < 0 || maxPadding > maxRecordBytes-recordHeaderSize {
		return Profile{}, errors.New("max padding is out of range")
	}
	if maxWriteBatchBytes == 0 {
		maxWriteBatchBytes = maxRecordBytes * 4
		if maxWriteBatchBytes > defaultWriteBatchSize {
			maxWriteBatchBytes = defaultWriteBatchSize
		}
	}
	if maxWriteBatchBytes < maxRecordBytes || maxWriteBatchBytes > maxWriteBatchSize {
		return Profile{}, errors.New("max write batch size is out of range")
	}
	return Profile{Name: name, MaxRecordBytes: maxRecordBytes, MaxPadding: maxPadding, MaxWriteBatchBytes: maxWriteBatchBytes}, nil
}

// Conn turns an ordinary byte stream into bounded v5 application records.
// Both ends must use Conn; the underlying stream remains encrypted by QUIC.
type Conn struct {
	underlying io.ReadWriteCloser
	profile    Profile

	writeMu sync.Mutex
	readMu  sync.Mutex

	writeBuffer []byte
	readBuffer  []byte
	readOffset  int
	discardBuf  []byte
	readHeader  [recordHeaderSize]byte

	randomMu  sync.Mutex
	randomBuf []byte
	randomPos int
}

func NewConn(underlying io.ReadWriteCloser, profile Profile) *Conn {
	if profile.MaxWriteBatchBytes == 0 && profile.MaxRecordBytes > 0 {
		profile.MaxWriteBatchBytes = profile.MaxRecordBytes * 4
		if profile.MaxWriteBatchBytes > defaultWriteBatchSize {
			profile.MaxWriteBatchBytes = defaultWriteBatchSize
		}
	}
	return &Conn{underlying: underlying, profile: profile}
}

func (c *Conn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	maxPayload := int(c.profile.MaxRecordBytes) - recordHeaderSize
	if maxPayload < 1 {
		return 0, errors.New("relay record profile is invalid")
	}
	batchLimit := int(c.profile.MaxWriteBatchBytes)
	if batchLimit < maxPayload {
		batchLimit = maxPayload
	}
	written := 0
	for written < len(p) {
		batchStart := written
		c.writeBuffer = c.writeBuffer[:0]
		for written < len(p) {
			remainingBatch := batchLimit - len(c.writeBuffer)
			if remainingBatch < recordHeaderSize+1 {
				break
			}
			payloadLen := len(p) - written
			if payloadLen > maxPayload {
				payloadLen = maxPayload
			}
			if payloadLen > remainingBatch-recordHeaderSize {
				payloadLen = remainingBatch - recordHeaderSize
			}
			if payloadLen < 1 {
				break
			}
			payload := p[written : written+payloadLen]
			padding := c.paddingLen(len(payload))
			maxPadding := remainingBatch - recordHeaderSize - len(payload)
			if padding > maxPadding {
				padding = maxPadding
			}
			start := len(c.writeBuffer)
			recordBytes := recordHeaderSize + len(payload) + padding
			if cap(c.writeBuffer)-len(c.writeBuffer) < recordBytes {
				newCap := cap(c.writeBuffer) * 2
				if newCap < len(c.writeBuffer)+recordBytes {
					newCap = len(c.writeBuffer) + recordBytes
				}
				if newCap > batchLimit+int(c.profile.MaxPadding) {
					newCap = batchLimit + int(c.profile.MaxPadding)
				}
				if newCap < len(c.writeBuffer)+recordBytes {
					newCap = len(c.writeBuffer) + recordBytes
				}
				buffer := make([]byte, len(c.writeBuffer), newCap)
				copy(buffer, c.writeBuffer)
				c.writeBuffer = buffer
			}
			c.writeBuffer = c.writeBuffer[:len(c.writeBuffer)+recordBytes]
			header := c.writeBuffer[start : start+recordHeaderSize]
			header[0] = protocol.RelayRecordVersion
			header[1] = 0
			header[2] = 0
			header[3] = 0
			if padding > 0 {
				header[1] = recordFlagPadding
			}
			binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
			binary.BigEndian.PutUint32(header[8:12], uint32(padding))
			copy(c.writeBuffer[start+recordHeaderSize:], payload)
			if padding > 0 {
				paddingStart := start + recordHeaderSize + len(payload)
				if err := c.randomBytes(c.writeBuffer[paddingStart : paddingStart+padding]); err != nil {
					return written, err
				}
			}
			written += len(payload)
		}
		if len(c.writeBuffer) == 0 {
			return written, errors.New("relay record batch has no payload capacity")
		}
		if err := writeAll(c.underlying, c.writeBuffer); err != nil {
			return batchStart, err
		}
	}
	return written, nil
}

func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.readOffset < len(c.readBuffer) {
		n := copy(p, c.readBuffer[c.readOffset:])
		c.readOffset += n
		if c.readOffset == len(c.readBuffer) {
			c.readBuffer = c.readBuffer[:0]
			c.readOffset = 0
		}
		return n, nil
	}
	payload, err := c.readRecord()
	if err != nil {
		return 0, err
	}
	n := copy(p, payload)
	if n < len(payload) {
		c.readBuffer = payload
		c.readOffset = n
	} else {
		c.readBuffer = c.readBuffer[:0]
		c.readOffset = 0
	}
	return n, nil
}

func (c *Conn) readRecord() ([]byte, error) {
	if _, err := io.ReadFull(c.underlying, c.readHeader[:]); err != nil {
		return nil, err
	}
	header := c.readHeader[:]
	if header[0] != protocol.RelayRecordVersion || header[1]&^recordFlagMask != 0 || binary.BigEndian.Uint16(header[2:4]) != 0 {
		return nil, errors.New("invalid relay record header")
	}
	payloadLen := int64(binary.BigEndian.Uint32(header[4:8]))
	paddingLen := int64(binary.BigEndian.Uint32(header[8:12]))
	if payloadLen == 0 || payloadLen+paddingLen+recordHeaderSize > c.profile.MaxRecordBytes || paddingLen > c.profile.MaxPadding || (paddingLen > 0) != (header[1]&recordFlagPadding != 0) {
		return nil, errors.New("relay record exceeds configured limit")
	}
	if cap(c.readBuffer) < int(payloadLen) {
		c.readBuffer = make([]byte, int(payloadLen))
	} else {
		c.readBuffer = c.readBuffer[:int(payloadLen)]
	}
	if _, err := io.ReadFull(c.underlying, c.readBuffer); err != nil {
		return nil, err
	}
	if paddingLen > 0 {
		if cap(c.discardBuf) < discardBufferSize {
			c.discardBuf = make([]byte, discardBufferSize)
		} else {
			c.discardBuf = c.discardBuf[:discardBufferSize]
		}
		if err := discardBytes(c.underlying, c.discardBuf, paddingLen); err != nil {
			return nil, err
		}
	}
	return c.readBuffer, nil
}

func discardBytes(r io.Reader, scratch []byte, count int64) error {
	if len(scratch) == 0 && count > 0 {
		return errors.New("relay discard buffer is empty")
	}
	for count > 0 {
		n := int64(len(scratch))
		if n > count {
			n = count
		}
		if _, err := io.ReadFull(r, scratch[:n]); err != nil {
			return err
		}
		count -= n
	}
	return nil
}

func (c *Conn) paddingLen(payloadLen int) int {
	if c.profile.Name != "balanced" || c.profile.MaxPadding == 0 {
		return 0
	}
	base := int64(recordHeaderSize + payloadLen)
	for _, bucket := range []int{512, 1024, 2048, 4096, 8192, 16384, 32768, 65536} {
		if int64(bucket) < base || int64(bucket)-base > c.profile.MaxPadding {
			continue
		}
		available := int64(bucket) - base
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

// PaddingLength returns a bounded random padding length for a payload already
// preceded by headerBytes bytes of record metadata.
func PaddingLength(profile string, headerBytes, maxPadding int64) int {
	if profile != "balanced" || maxPadding == 0 {
		return 0
	}
	base := headerBytes
	for _, bucket := range []int{512, 1024, 2048, 4096, 8192, 16384, 32768, 65536} {
		if int64(bucket) < base || int64(bucket)-base > maxPadding {
			continue
		}
		available := int64(bucket) - base
		var b [2]byte
		if _, err := rand.Read(b[:]); err != nil {
			return int(available)
		}
		return int(int64(binary.BigEndian.Uint16(b[:])) % (available + 1))
	}
	return 0
}

// Flush forwards an optional underlying flush hook. Writes are already fully
// emitted before Write returns; Flush is used by the bidirectional copier to
// preserve interactive latency for buffered transports.
func (c *Conn) Flush() {
	if flusher, ok := c.underlying.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

func (c *Conn) Close() error {
	c.Flush()
	return c.underlying.Close()
}

func (c *Conn) CloseRead() {
	if v, ok := c.underlying.(interface{ CloseRead() }); ok {
		v.CloseRead()
	}
}

func (c *Conn) CloseWrite() {
	c.Flush()
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
