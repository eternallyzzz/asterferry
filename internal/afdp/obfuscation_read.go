package afdp

import (
	"crypto/hmac"
	"encoding/binary"
	"net"
	"time"
)

func (c *obfuscationPacketConn) decode(wire []byte, addr net.Addr, dst []byte) ([]byte, bool) {
	if len(wire) < obfuscationSaltBytes+obfuscationTagBytes+dataHeaderBytes || len(wire) > maxObfuscationDatagram {
		return nil, false
	}
	salt := wire[:obfuscationSaltBytes]
	masked := wire[obfuscationSaltBytes : len(wire)-obfuscationTagBytes]
	gotTag := wire[len(wire)-obfuscationTagBytes:]
	for _, key := range c.keys {
		expected, err := c.tag(salt, masked, key.key)
		if err != nil {
			if c.metrics != nil {
				c.metrics.ObfuscationPacketRejected()
			}
			return nil, false
		}
		if !hmac.Equal(expected[:], gotTag) {
			continue
		}
		if c.metrics != nil {
			c.metrics.ObfuscationPacketAccepted(key != c.keys[0])
		}
		pooled := obfuscationBodyPool.Get().(*obfuscationPoolBuffer)
		body := pooled.bytes
		if cap(body) < len(masked) {
			body = make([]byte, len(masked))
			pooled.bytes = body
		}
		body = body[:len(masked)]
		copy(body, masked)
		c.mask(body, salt, key.key)
		if len(body) < dataHeaderBytes || body[0] != obfuscationVersion {
			if c.metrics != nil {
				c.metrics.ObfuscationFragmentDropped()
			}
			putObfuscationBody(pooled)
			return nil, false
		}
		switch body[1] {
		case obfuscationData:
			payload := body[dataHeaderBytes:]
			if len(payload) > len(dst) {
				if c.metrics != nil {
					c.metrics.ObfuscationFragmentDropped()
				}
				putObfuscationBody(pooled)
				return nil, false
			}
			copy(dst, payload)
			putObfuscationBody(pooled)
			return dst[:len(payload)], true
		case obfuscationFragment:
			packet, ok := c.acceptFragment(body, addr)
			putObfuscationBody(pooled)
			return packet, ok
		default:
			if c.metrics != nil {
				c.metrics.ObfuscationFragmentDropped()
			}
			putObfuscationBody(pooled)
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
	if total < 2 || total > maxFragmentCount || index >= total || payloadLen == 0 || fragmentHeaderBytes+payloadLen+paddingLen != len(body) || len(body)+obfuscationSaltBytes+obfuscationTagBytes > int(c.opts.MaxHandshakeFragmentWireBytes) {
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
