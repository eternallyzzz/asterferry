package afdp

import (
	"encoding/binary"
)

// DatagramHeader is the fixed AFDP/2 header carried in a QUIC DATAGRAM. The
// payload itself is not wrapped in a per-record envelope.
type DatagramHeader struct {
	Flags         byte
	FlowID        uint64
	Sequence      uint32
	FragmentIndex uint16
	FragmentCount uint16
}

const (
	DatagramFlagFragmented byte = 1 << iota
	DatagramFlagFin
)

// DatagramHeaderSize exposes the fixed wire size to forwarding adapters
// without allowing callers to mutate the protocol constant.
func DatagramHeaderSize() int { return datagramHeaderBytes }

func validateDatagramHeader(header DatagramHeader) error {
	switch {
	case header.FlowID == 0:
		return ErrMalformedFrame
	case header.FragmentCount == 0:
		return ErrMalformedFrame
	case header.FragmentCount == 1 && header.FragmentIndex != 0:
		return ErrMalformedFrame
	case header.FragmentIndex >= header.FragmentCount:
		return ErrMalformedFrame
	case header.FragmentCount > maxDatagramFragments:
		return ErrFrameTooLarge
	case header.FragmentCount == 1 && header.Flags&DatagramFlagFragmented != 0:
		return ErrMalformedFrame
	case header.FragmentCount > 1 && header.Flags&DatagramFlagFragmented == 0:
		return ErrMalformedFrame
	case header.Flags&^byte(DatagramFlagFragmented|DatagramFlagFin) != 0:
		return ErrMalformedFrame
	case header.FragmentIndex == header.FragmentCount-1 && header.Flags&DatagramFlagFin == 0:
		return ErrMalformedFrame
	case header.FragmentIndex != header.FragmentCount-1 && header.Flags&DatagramFlagFin != 0:
		return ErrMalformedFrame
	default:
		return nil
	}
}

func normalizeDatagramPayloadLimit(maxPayload int) int {
	if maxPayload <= 0 || maxPayload > maxDatagramPayload {
		return maxDatagramPayload
	}
	return maxPayload
}

func EncodeDatagram(header DatagramHeader, payload []byte, maxPayload int) ([]byte, error) {
	if err := validateDatagramHeader(header); err != nil {
		return nil, err
	}
	if len(payload) > normalizeDatagramPayloadLimit(maxPayload) {
		return nil, ErrFrameTooLarge
	}
	result := make([]byte, datagramHeaderBytes+len(payload))
	result[0] = Version
	result[1] = header.Flags
	binary.BigEndian.PutUint16(result[2:4], datagramHeaderBytes)
	binary.BigEndian.PutUint64(result[4:12], header.FlowID)
	binary.BigEndian.PutUint32(result[12:16], header.Sequence)
	binary.BigEndian.PutUint16(result[16:18], header.FragmentIndex)
	binary.BigEndian.PutUint16(result[18:20], header.FragmentCount)
	binary.BigEndian.PutUint16(result[20:22], uint16(len(payload)))
	// Bytes 22..23 are reserved and must stay zero.
	copy(result[datagramHeaderBytes:], payload)
	return result, nil
}

func DecodeDatagram(data []byte, maxPayload int) (DatagramHeader, []byte, error) {
	if len(data) < datagramHeaderBytes {
		return DatagramHeader{}, nil, ErrMalformedFrame
	}
	if data[0] != Version {
		return DatagramHeader{}, nil, ErrInvalidVersion
	}
	if binary.BigEndian.Uint16(data[2:4]) != datagramHeaderBytes || binary.BigEndian.Uint16(data[22:24]) != 0 {
		return DatagramHeader{}, nil, ErrMalformedFrame
	}
	payloadLength := int(binary.BigEndian.Uint16(data[20:22]))
	if payloadLength != len(data)-datagramHeaderBytes {
		return DatagramHeader{}, nil, ErrMalformedFrame
	}
	maxPayload = normalizeDatagramPayloadLimit(maxPayload)
	header := DatagramHeader{Flags: data[1], FlowID: binary.BigEndian.Uint64(data[4:12]), Sequence: binary.BigEndian.Uint32(data[12:16]), FragmentIndex: binary.BigEndian.Uint16(data[16:18]), FragmentCount: binary.BigEndian.Uint16(data[18:20])}
	if err := validateDatagramHeader(header); err != nil {
		return DatagramHeader{}, nil, err
	}
	if payloadLength > maxPayload {
		return DatagramHeader{}, nil, ErrFrameTooLarge
	}
	return header, append([]byte(nil), data[datagramHeaderBytes:]...), nil
}
