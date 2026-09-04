package afdp

import (
	"crypto/rand"
)

func putObfuscationBody(buffer *obfuscationPoolBuffer) {
	if buffer != nil && cap(buffer.bytes) >= 16<<10 {
		buffer.bytes = buffer.bytes[:16<<10]
		obfuscationBodyPool.Put(buffer)
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
