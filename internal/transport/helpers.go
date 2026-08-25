package transport

import "io"

// WriteAll writes all bytes or returns the first write error.
func WriteAll(w io.Writer, p []byte) error {
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

// WriteOpenError encodes and writes a structured open failure.
func WriteOpenError(w io.Writer, id uint64, code ErrorCode, detail string, retryable bool, max int64) error {
	frame, err := MessageFrame(TypeOpenError, id, OpenResult{Error: NewProtocolError(code, detail, retryable)})
	if err != nil {
		return err
	}
	return WriteFrame(w, frame, max)
}

// WriteProtocolError encodes and writes a structured protocol failure.
func WriteProtocolError(w io.Writer, id uint64, code ErrorCode, detail string, retryable bool, max int64) error {
	frame, err := MessageFrame(TypeError, id, *NewProtocolError(code, detail, retryable))
	if err != nil {
		return err
	}
	return WriteFrame(w, frame, max)
}

// MustMessageFrame is for static/control messages whose type and payload are
// guaranteed by the caller. Dynamic data frames should use MessageFrame and
// handle its error explicitly.
func MustMessageFrame(typ byte, requestID uint64, value any) Frame {
	frame, err := MessageFrame(typ, requestID, value)
	if err != nil {
		panic(err)
	}
	return frame
}
