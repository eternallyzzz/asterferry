// Package wireio contains bounded I/O primitives shared by the control and
// data wire implementations.  Keeping the short-write loop in one package
// avoids subtly different framing behaviour at protocol boundaries.
package wireio

import "io"

// WriteFull writes all bytes or returns the first error.  A writer that makes
// no progress without an error is treated as a short write to avoid spinning
// forever on a broken transport.
func WriteFull(w io.Writer, data []byte) error {
	if w == nil {
		return io.ErrClosedPipe
	}
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
