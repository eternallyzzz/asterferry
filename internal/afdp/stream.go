package afdp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
	if err := writeAll(w, size[:]); err != nil {
		return err
	}
	return writeAll(w, frame)
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
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
	// same boundary here instead of rejecting a payload at the configured
	// maximum after it was successfully written.
	if length == 0 || uint64(length) > uint64(max)+6 {
		return OpenMetadata{}, ErrFrameTooLarge
	}
	frame := make([]byte, int(length))
	if _, err := io.ReadFull(r, frame); err != nil {
		return OpenMetadata{}, err
	}
	return DecodeOpen(frame, max)
}

// CopyRaw copies a data stream while enforcing a per-stream upper bound. A
// negative limit means unlimited only when the caller explicitly opts in;
// normal data-plane code should always pass a negotiated positive limit.
func CopyRaw(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit == 0 {
		return 0, errors.New("raw stream limit must be non-zero")
	}
	var reader io.Reader = src
	if limit > 0 {
		// The extra byte lets us distinguish an exact-limit stream from one
		// that continues. Avoid overflowing int64 at the (theoretical) maximum
		// limit; io.Copy cannot report more than MaxInt64 bytes anyway.
		probe := limit
		if limit < int64(^uint64(0)>>1) {
			probe++
		}
		reader = io.LimitReader(src, probe)
	}
	n, err := io.Copy(dst, reader)
	if limit > 0 && n > limit {
		return limit, fmt.Errorf("raw stream exceeds %d bytes", limit)
	}
	return n, err
}
