// Package duplex contains the bounded bidirectional copy primitive used by
// the data-plane forwarding paths.  A normal EOF closes only the destination
// write half, allowing the opposite direction to continue until it also
// finishes.  This is important for TCP and QUIC protocols that use half-close
// to delimit requests while still returning a response.
package duplex

import (
	"errors"
	"io"
	"sync"

	"github.com/quic-go/quic-go"
)

var (
	// ErrInvalidEndpoint indicates that a duplex copy was given a nil side.
	ErrInvalidEndpoint = errors.New("duplex endpoints are required")
	// ErrHalfCloseUnsupported is returned when a side cannot propagate a
	// normal EOF without closing its read direction as well.
	ErrHalfCloseUnsupported = errors.New("duplex endpoint does not support write half-close")
)

const defaultBufferSize = 32 << 10

type endpoint struct {
	rwc    io.ReadWriteCloser
	reader io.Reader
}

type copyState struct {
	left  io.ReadWriteCloser
	right io.ReadWriteCloser

	abortOnce sync.Once
	closeOnce sync.Once
	errMu     sync.Mutex
	firstErr  error
}

// CopyDuplex copies both directions with bounded buffers.  A normal EOF on
// one reader closes only the corresponding destination write half; the other
// direction is left running until it reaches EOF or an error.
func CopyDuplex(left, right io.ReadWriteCloser, maxBuffer int) error {
	return copyEndpoints(endpoint{rwc: left, reader: left}, endpoint{rwc: right, reader: right}, maxBuffer)
}

// CopyDuplexWithReader is CopyDuplex with an alternate reader for the left
// endpoint.  Proxy handshakes use this to drain bytes already buffered by a
// protocol reader before reading directly from the client connection.
func CopyDuplexWithReader(left io.ReadWriteCloser, leftReader io.Reader, right io.ReadWriteCloser, maxBuffer int) error {
	if leftReader == nil {
		leftReader = left
	}
	return copyEndpoints(endpoint{rwc: left, reader: leftReader}, endpoint{rwc: right, reader: right}, maxBuffer)
}

func copyEndpoints(left, right endpoint, maxBuffer int) error {
	if left.rwc == nil || right.rwc == nil || left.reader == nil || right.reader == nil {
		return ErrInvalidEndpoint
	}
	bufferSize := defaultBufferSize
	if maxBuffer > 0 {
		bufferSize = maxBuffer / 2
		if bufferSize < 1 {
			bufferSize = 1
		}
		if bufferSize > defaultBufferSize {
			bufferSize = defaultBufferSize
		}
	}

	state := &copyState{left: left.rwc, right: right.rwc}
	results := make(chan error, 2)
	go state.copy(left, right, make([]byte, bufferSize), results)
	go state.copy(right, left, make([]byte, bufferSize), results)

	<-results
	<-results
	if err := state.error(); err != nil {
		return err
	}
	state.closeBoth()
	return nil
}

func (s *copyState) copy(dst, src endpoint, buffer []byte, results chan<- error) {
	_, err := io.CopyBuffer(dst.rwc, src.reader, buffer)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		s.recordError(err)
		s.abortBoth()
		results <- err
		return
	}
	if err := closeWrite(dst.rwc); err != nil {
		s.recordError(err)
		s.abortBoth()
		results <- err
		return
	}
	results <- nil
}

func (s *copyState) recordError(err error) {
	s.errMu.Lock()
	if s.firstErr == nil {
		s.firstErr = err
	}
	s.errMu.Unlock()
}

func (s *copyState) error() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.firstErr
}

func (s *copyState) abortBoth() {
	s.abortOnce.Do(func() {
		abortEndpoint(s.left)
		abortEndpoint(s.right)
	})
}

func (s *copyState) closeBoth() {
	s.closeOnce.Do(func() {
		_ = s.left.Close()
		_ = s.right.Close()
	})
}

type writeHalfCloser interface {
	CloseWrite() error
}

type writeHalfCloserNoError interface {
	CloseWrite()
}

func closeWrite(w io.ReadWriteCloser) error {
	if halfCloser, ok := w.(writeHalfCloser); ok {
		return halfCloser.CloseWrite()
	}
	if halfCloser, ok := w.(writeHalfCloserNoError); ok {
		halfCloser.CloseWrite()
		return nil
	}
	// quic.Stream.Close closes only its send direction.  Keep this explicit
	// because io.ReadWriteCloser.Close conventionally closes both directions.
	if stream, ok := w.(*quic.Stream); ok {
		return stream.Close()
	}
	return ErrHalfCloseUnsupported
}

type aborter interface {
	Abort() error
}

func abortEndpoint(w io.ReadWriteCloser) {
	if abort, ok := w.(aborter); ok {
		_ = abort.Abort()
		return
	}
	if stream, ok := w.(*quic.Stream); ok {
		// Cancel both QUIC unidirectional halves.  Calling Close here would
		// race a concurrent Write; cancellation is the stream's abort path and
		// is safe for unblocking both copy goroutines.
		stream.CancelRead(quic.StreamErrorCode(0))
		stream.CancelWrite(quic.StreamErrorCode(0))
		return
	}
	_ = w.Close()
}
