package afdp

import (
	"asterferry/internal/random"
	"encoding/binary"
	"errors"
	"net"
)

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

func (c *obfuscationPacketConn) writeData(payload []byte, addr net.Addr) error {
	wire, err := c.encodeData(payload)
	if err != nil {
		return err
	}
	_, err = c.conn.WriteTo(wire, addr)
	return err
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
		// The fragment wire format requires at least two fragments. A packet
		// that fits in one fragment therefore stays a normal authenticated data
		// datagram instead of advertising a second fragment that is never sent.
		return c.writeData(packet, addr)
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
		if len(wire) > int(c.opts.MaxHandshakeFragmentWireBytes) {
			return errors.New("camouflage handshake fragment exceeds max wire packet size")
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
		value, err := random.Uint16n(uint32(maxPadding - minPadding + 1))
		if err != nil {
			return nil, err
		}
		padding += int(value)
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
	wire := c.writeBuffer(&c.writeWire, len(salt)+len(body)+obfuscationTagBytes)
	copy(wire, salt[:])
	masked := wire[len(salt) : len(salt)+len(body)]
	copy(masked, body)
	c.mask(masked, salt[:], c.keys[0].key)
	tag, err := c.tag(salt[:], masked, c.keys[0].key)
	if err != nil {
		return nil, err
	}
	copy(wire[len(salt)+len(masked):], tag[:])
	return wire, nil
}
