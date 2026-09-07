package duplex

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// testEndpoint has independent inbound and outbound state, like the two
// halves of a socket. Closing its write half must not make its read side EOF.
type testEndpoint struct {
	mu          sync.Mutex
	input       bytes.Buffer
	inputClosed bool
	inputErr    error
	inputSignal chan struct{}
	output      bytes.Buffer
	writeClosed bool
	closed      bool
	aborted     bool
}

type noHalfCloseEndpoint struct {
	inner *testEndpoint
}

func (e *noHalfCloseEndpoint) Read(p []byte) (int, error)  { return e.inner.Read(p) }
func (e *noHalfCloseEndpoint) Write(p []byte) (int, error) { return e.inner.Write(p) }
func (e *noHalfCloseEndpoint) Close() error                { return e.inner.Close() }

func newTestEndpoint() *testEndpoint {
	return &testEndpoint{inputSignal: make(chan struct{})}
}

func (e *testEndpoint) Read(p []byte) (int, error) {
	for {
		e.mu.Lock()
		if e.input.Len() > 0 {
			n, _ := e.input.Read(p)
			e.mu.Unlock()
			return n, nil
		}
		if e.inputErr != nil {
			err := e.inputErr
			e.mu.Unlock()
			return 0, err
		}
		if e.inputClosed || e.closed {
			e.mu.Unlock()
			return 0, io.EOF
		}
		signal := e.inputSignal
		e.mu.Unlock()
		<-signal
	}
}

func (e *testEndpoint) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.writeClosed || e.closed {
		return 0, io.ErrClosedPipe
	}
	return e.output.Write(p)
}

func (e *testEndpoint) CloseWrite() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return io.ErrClosedPipe
	}
	e.writeClosed = true
	return nil
}

func (e *testEndpoint) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.writeClosed = true
	e.inputClosed = true
	close(e.inputSignal)
	e.mu.Unlock()
	return nil
}

func (e *testEndpoint) Abort() error {
	e.mu.Lock()
	e.aborted = true
	e.mu.Unlock()
	return e.Close()
}

func (e *testEndpoint) EOF() {
	e.mu.Lock()
	if !e.inputClosed {
		e.inputClosed = true
		close(e.inputSignal)
		e.inputSignal = make(chan struct{})
	}
	e.mu.Unlock()
}

func (e *testEndpoint) Fail(err error) {
	e.mu.Lock()
	e.inputErr = err
	close(e.inputSignal)
	e.inputSignal = make(chan struct{})
	e.mu.Unlock()
}

func (e *testEndpoint) Push(data []byte) {
	e.mu.Lock()
	if e.inputClosed || e.closed {
		e.mu.Unlock()
		return
	}
	_, _ = e.input.Write(data)
	close(e.inputSignal)
	e.inputSignal = make(chan struct{})
	e.mu.Unlock()
}

func (e *testEndpoint) state() (output string, writeClosed, closed, aborted bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.output.String(), e.writeClosed, e.closed, e.aborted
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestCopyDuplexPropagatesEOFWithoutTruncatingOppositeDirection(t *testing.T) {
	left, right := newTestEndpoint(), newTestEndpoint()
	result := make(chan error, 1)
	go func() { result <- CopyDuplex(left, right, 128) }()

	// The left reader reaches EOF. The right write half receives FIN, but
	// neither endpoint may be fully closed while the right side can respond.
	left.EOF()
	waitFor(t, func() bool {
		_, rightWriteClosed, _, _ := right.state()
		return rightWriteClosed
	})
	_, _, leftClosed, _ := left.state()
	_, _, rightClosed, _ := right.state()
	if leftClosed || rightClosed {
		t.Fatalf("single-direction EOF closed both endpoints: left=%v right=%v", leftClosed, rightClosed)
	}

	right.Push([]byte("response"))
	waitFor(t, func() bool {
		output, _, _, _ := left.state()
		return output == "response"
	})
	right.EOF()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("CopyDuplex() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CopyDuplex() did not wait for both directions")
	}
	_, leftWriteClosed, leftClosed, leftAborted := left.state()
	_, rightWriteClosed, rightClosed, rightAborted := right.state()
	if !leftWriteClosed || !rightWriteClosed || !leftClosed || !rightClosed {
		t.Fatalf("both directions did not finish cleanly: left write=%v closed=%v right write=%v closed=%v", leftWriteClosed, leftClosed, rightWriteClosed, rightClosed)
	}
	if leftAborted || rightAborted {
		t.Fatal("normal EOF path used abort semantics")
	}
}

func TestCopyDuplexAbortsBothSidesOnCopyError(t *testing.T) {
	left, right := newTestEndpoint(), newTestEndpoint()
	left.Fail(errors.New("read failed"))
	result := make(chan error, 1)
	go func() { result <- CopyDuplex(left, right, 128) }()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("CopyDuplex() returned nil after a copy error")
		}
	case <-time.After(time.Second):
		t.Fatal("CopyDuplex() did not return after a copy error")
	}
	_, _, leftClosed, leftAborted := left.state()
	_, _, rightClosed, rightAborted := right.state()
	if !leftClosed || !rightClosed || !leftAborted || !rightAborted {
		t.Fatalf("copy error did not abort both endpoints: left closed=%v aborted=%v right closed=%v aborted=%v", leftClosed, leftAborted, rightClosed, rightAborted)
	}
}

func TestCopyDuplexFailsClosedWithoutHalfCloseSupport(t *testing.T) {
	left, right := &noHalfCloseEndpoint{inner: newTestEndpoint()}, &noHalfCloseEndpoint{inner: newTestEndpoint()}
	result := make(chan error, 1)
	go func() { result <- CopyDuplex(left, right, 128) }()
	left.inner.EOF()

	select {
	case err := <-result:
		if !errors.Is(err, ErrHalfCloseUnsupported) {
			t.Fatalf("CopyDuplex() error = %v, want ErrHalfCloseUnsupported", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CopyDuplex() did not fail closed")
	}
	_, _, leftClosed, _ := left.inner.state()
	_, _, rightClosed, _ := right.inner.state()
	if !leftClosed || !rightClosed {
		t.Fatalf("unsupported half-close did not close both endpoints: left=%v right=%v", leftClosed, rightClosed)
	}
}

func TestCopyDuplexWithReaderUsesBufferedInput(t *testing.T) {
	left, right := newTestEndpoint(), newTestEndpoint()
	result := make(chan error, 1)
	go func() {
		result <- CopyDuplexWithReader(left, bytes.NewBufferString("buffered request"), right, 128)
	}()

	waitFor(t, func() bool {
		output, rightWriteClosed, _, _ := right.state()
		return output == "buffered request" && rightWriteClosed
	})
	right.EOF()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("CopyDuplexWithReader() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CopyDuplexWithReader() did not finish")
	}
}
