package afdp

import (
	"encoding/binary"
	"fmt"
	"golang.org/x/crypto/blake2b"
)

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

func (c *obfuscationPacketConn) tag(salt, masked []byte, key [32]byte) ([obfuscationTagBytes]byte, error) {
	// Authenticate the complete masked body. The previous transport helper
	// truncated the MAC input to a small fixed buffer, which left the tail of a
	// large QUIC datagram unauthenticated. A streaming hash keeps the same
	// domain separation without imposing a hidden payload limit.
	hasher, err := blake2b.New256(nil)
	if err != nil {
		return [obfuscationTagBytes]byte{}, fmt.Errorf("initialize obfuscation authenticator: %w", err)
	}
	if _, err := hasher.Write([]byte(obfuscationTagDomain)); err != nil {
		return [obfuscationTagBytes]byte{}, fmt.Errorf("write obfuscation authenticator domain: %w", err)
	}
	if _, err := hasher.Write(key[:]); err != nil {
		return [obfuscationTagBytes]byte{}, fmt.Errorf("write obfuscation authenticator key: %w", err)
	}
	if _, err := hasher.Write(salt); err != nil {
		return [obfuscationTagBytes]byte{}, fmt.Errorf("write obfuscation authenticator salt: %w", err)
	}
	if _, err := hasher.Write(masked); err != nil {
		return [obfuscationTagBytes]byte{}, fmt.Errorf("write obfuscation authenticator body: %w", err)
	}
	digest := hasher.Sum(nil)
	var tag [obfuscationTagBytes]byte
	copy(tag[:], digest[:obfuscationTagBytes])
	return tag, nil
}
