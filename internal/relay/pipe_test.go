package relay

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
)

type copyEndpoint struct {
	input  []byte
	pos    int
	chunk  int
	wrote  []byte
	closed atomic.Bool
}

func (e *copyEndpoint) Read(p []byte) (int, error) {
	if e.pos >= len(e.input) {
		return 0, io.EOF
	}
	n := copy(p, e.input[e.pos:])
	e.pos += n
	return n, nil
}

func (e *copyEndpoint) Write(p []byte) (int, error) {
	n := len(p)
	if e.chunk > 0 && n > e.chunk {
		n = e.chunk
	}
	e.wrote = append(e.wrote, p[:n]...)
	return n, nil
}

func (e *copyEndpoint) Close() error {
	e.closed.Store(true)
	return nil
}

func TestBidirectionalCopiesPartialWritesAndCounts(t *testing.T) {
	a := &copyEndpoint{input: []byte("from-a"), chunk: 2}
	b := &copyEndpoint{input: []byte("from-b"), chunk: 3}
	var in, out atomic.Uint64
	Bidirectional(a, b, Counters{In: func(n uint64) { in.Add(n) }, Out: func(n uint64) { out.Add(n) }})
	if string(a.wrote) != "from-b" || string(b.wrote) != "from-a" {
		t.Fatalf("copied data = %q/%q", a.wrote, b.wrote)
	}
	if in.Load() != uint64(len("from-b")) || out.Load() != uint64(len("from-a")) {
		t.Fatalf("copy counters = %d/%d", in.Load(), out.Load())
	}
	if !a.closed.Load() || !b.closed.Load() {
		t.Fatal("bidirectional relay did not close both endpoints")
	}
}

type closeWriteNoError struct{ called bool }

func (c *closeWriteNoError) Write(p []byte) (int, error) { return len(p), nil }
func (c *closeWriteNoError) CloseWrite()                 { c.called = true }

type closeWriteError struct{ called bool }

func (c *closeWriteError) Write(p []byte) (int, error) { return len(p), nil }
func (c *closeWriteError) CloseWrite() error {
	c.called = true
	return io.ErrClosedPipe
}

type closeReadNoError struct{ called bool }

func (c *closeReadNoError) Read([]byte) (int, error) { return 0, io.EOF }
func (c *closeReadNoError) CloseRead()               { c.called = true }

type closeReadError struct{ called bool }

func (c *closeReadError) Read([]byte) (int, error) { return 0, io.EOF }
func (c *closeReadError) CloseRead() error {
	c.called = true
	return io.ErrClosedPipe
}

type closeFallback struct{ closed bool }

func (c *closeFallback) Write(p []byte) (int, error) { return len(p), nil }
func (c *closeFallback) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *closeFallback) Close() error {
	c.closed = true
	return nil
}

func TestHalfCloseVariants(t *testing.T) {
	writeNoError := &closeWriteNoError{}
	halfCloseWrite(writeNoError)
	if !writeNoError.called {
		t.Fatal("CloseWrite without error was not called")
	}
	writeError := &closeWriteError{}
	halfCloseWrite(writeError)
	if !writeError.called {
		t.Fatal("CloseWrite with error was not called")
	}
	writeFallback := &closeFallback{}
	halfCloseWrite(writeFallback)
	if !writeFallback.closed {
		t.Fatal("io.Closer fallback was not called")
	}

	readNoError := &closeReadNoError{}
	halfCloseRead(readNoError)
	if !readNoError.called {
		t.Fatal("CloseRead without error was not called")
	}
	readError := &closeReadError{}
	halfCloseRead(readError)
	if !readError.called {
		t.Fatal("CloseRead with error was not called")
	}
}

func TestIdleWatchCanBeDisabled(t *testing.T) {
	touch, stop := StartIdleWatch(context.Background(), 0, func() { t.Fatal("disabled idle watch closed transport") })
	touch()
	stop()
}
