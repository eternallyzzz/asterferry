package afdp

import (
	"encoding/binary"
	"io"

	"asterferry/internal/wireio"
)

// WriteOpen writes the one bounded metadata message that precedes a raw TCP
// stream. Once WriteOpen returns, callers must copy bytes directly; AFDP does
// not wrap every payload chunk in another record envelope.
func WriteOpen(w io.Writer, metadata OpenMetadata, max int) error {
	frame, err := EncodeOpen(metadata, max)
	if err != nil {
		return err
	}
	if len(frame) > int(^uint32(0)) {
		return ErrFrameTooLarge
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(frame)))
	if err := wireio.WriteFull(w, size[:]); err != nil {
		return err
	}
	return wireio.WriteFull(w, frame)
}

func ReadOpen(r io.Reader, max int) (OpenMetadata, error) {
	if max <= 0 {
		max = maxSessionFrame
	}
	max = normalizeFrameLimit(max)
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return OpenMetadata{}, err
	}
	length := binary.BigEndian.Uint32(size[:])
	// The AFDP encoder's limit applies to the protobuf payload. The wire
	// frame adds a six-byte version/kind/length prefix, so accept exactly the
	// same protobuf payload boundary after decoding.
	if length == 0 || uint64(length) > uint64(max)+6 {
		return OpenMetadata{}, ErrFrameTooLarge
	}
	frame := make([]byte, int(length))
	if _, err := io.ReadFull(r, frame); err != nil {
		return OpenMetadata{}, err
	}
	return DecodeOpen(frame, max)
}
