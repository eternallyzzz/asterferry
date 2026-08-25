package random

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
)

// Uint16n returns a uniformly distributed value in [0, n). Rejection
// sampling avoids the modulo bias that would otherwise be introduced when n
// does not divide the uint16 sample space.
func Uint16n(n uint32) (uint32, error) {
	if n == 0 || n > 1<<16 {
		return 0, errors.New("random range is out of bounds")
	}
	limit := uint32(1<<16) - (uint32(1<<16) % n)
	var buf [2]byte
	for {
		if _, err := rand.Read(buf[:]); err != nil {
			return 0, err
		}
		value := uint32(binary.BigEndian.Uint16(buf[:]))
		if value < limit {
			return value % n, nil
		}
	}
}
