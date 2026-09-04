package duplex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

type scriptedRead struct {
	data []byte
	err  error
}

type scriptedEndpoint struct {
	mu          sync.Mutex
	reads       []scriptedRead
	readIndex   int
	pending     []byte
	output      bytes.Buffer
	writeClosed bool
	closed      bool
	aborted     bool
}

type noHalfCloseScriptedEndpoint struct {
	inner *scriptedEndpoint
}

func (e *noHalfCloseScriptedEndpoint) Read(p []byte) (int, error)  { return e.inner.Read(p) }
func (e *noHalfCloseScriptedEndpoint) Write(p []byte) (int, error) { return e.inner.Write(p) }
func (e *noHalfCloseScriptedEndpoint) Close() error                { return e.inner.Close() }

func (e *scriptedEndpoint) Read(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pending) > 0 {
		n := copy(p, e.pending)
		e.pending = e.pending[n:]
		return n, nil
	}
	if e.readIndex >= len(e.reads) {
		return 0, io.EOF
	}
	read := e.reads[e.readIndex]
	e.readIndex++
	if len(read.data) == 0 {
		if read.err == nil {
			return 0, io.EOF
		}
		return 0, read.err
	}
	n := copy(p, read.data)
	if n < len(read.data) {
		e.pending = append(e.pending[:0], read.data[n:]...)
	}
	return n, read.err
}

func (e *scriptedEndpoint) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.writeClosed || e.closed {
		return 0, io.ErrClosedPipe
	}
	return e.output.Write(p)
}

func (e *scriptedEndpoint) CloseWrite() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return io.ErrClosedPipe
	}
	e.writeClosed = true
	return nil
}

func (e *scriptedEndpoint) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	e.writeClosed = true
	return nil
}

func (e *scriptedEndpoint) Abort() error {
	e.mu.Lock()
	e.aborted = true
	e.closed = true
	e.writeClosed = true
	e.mu.Unlock()
	return nil
}

func (e *scriptedEndpoint) state() (string, bool, bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.output.String(), e.writeClosed, e.closed, e.aborted
}

type duplexModelScript struct {
	data  []byte
	fails bool
}

func makeDuplexScript(random *rand.Rand, side string, seed int64) duplexModelScript {
	var data bytes.Buffer
	for index := 0; index < 1+random.Intn(4); index++ {
		_, _ = fmt.Fprintf(&data, "%s-%d-%d|", side, seed, index)
	}
	return duplexModelScript{data: data.Bytes(), fails: random.Intn(3) == 0}
}

func scriptedReads(script duplexModelScript) []scriptedRead {
	if script.fails {
		return []scriptedRead{{data: script.data}, {err: errors.New("scripted read failure")}}
	}
	return []scriptedRead{{data: script.data}, {err: io.EOF}}
}

func TestCopyDuplexStateMachineContract(t *testing.T) {
	for seedOffset := int64(0); seedOffset < 256; seedOffset++ {
		seed := int64(0xA57EF2) + seedOffset
		random := rand.New(rand.NewSource(seed))
		leftScript := makeDuplexScript(random, "left", seed)
		rightScript := makeDuplexScript(random, "right", seed)
		left := &scriptedEndpoint{reads: scriptedReads(leftScript)}
		right := &scriptedEndpoint{reads: scriptedReads(rightScript)}
		trace := []string{"left-open", "right-open"}
		result := CopyDuplex(left, right, 128)
		trace = append(trace, "left-terminal", "right-terminal")

		if leftScript.fails || rightScript.fails {
			if result == nil {
				t.Fatalf("seed=%d trace=%s returned nil after scripted read failure", seed, strings.Join(trace, ","))
			}
			_, _, leftClosed, leftAborted := left.state()
			_, _, rightClosed, rightAborted := right.state()
			if !leftClosed || !rightClosed || !leftAborted || !rightAborted {
				t.Fatalf("seed=%d trace=%s failure state left=(closed:%v aborted:%v) right=(closed:%v aborted:%v)", seed, strings.Join(trace, ","), leftClosed, leftAborted, rightClosed, rightAborted)
			}
			continue
		}

		if result != nil {
			t.Fatalf("seed=%d trace=%s returned error on normal EOF: %v", seed, strings.Join(trace, ","), result)
		}
		leftOutput, leftWriteClosed, leftClosed, leftAborted := left.state()
		rightOutput, rightWriteClosed, rightClosed, rightAborted := right.state()
		if leftOutput != string(rightScript.data) || rightOutput != string(leftScript.data) {
			t.Fatalf("seed=%d trace=%s outputs left=%q right=%q", seed, strings.Join(trace, ","), leftOutput, rightOutput)
		}
		if !leftWriteClosed || !rightWriteClosed || !leftClosed || !rightClosed || leftAborted || rightAborted {
			t.Fatalf("seed=%d trace=%s normal state left=(write:%v closed:%v aborted:%v) right=(write:%v closed:%v aborted:%v)", seed, strings.Join(trace, ","), leftWriteClosed, leftClosed, leftAborted, rightWriteClosed, rightClosed, rightAborted)
		}
	}
}

func TestCopyDuplexUnsupportedHalfCloseIsFailClosedContract(t *testing.T) {
	left := &noHalfCloseScriptedEndpoint{inner: &scriptedEndpoint{reads: []scriptedRead{{err: io.EOF}}}}
	right := &noHalfCloseScriptedEndpoint{inner: &scriptedEndpoint{reads: []scriptedRead{{err: io.EOF}}}}
	if err := CopyDuplex(left, right, 128); !errors.Is(err, ErrHalfCloseUnsupported) {
		t.Fatalf("unsupported half-close error = %v, want ErrHalfCloseUnsupported", err)
	}
	_, _, leftClosed, leftAborted := left.inner.state()
	_, _, rightClosed, rightAborted := right.inner.state()
	if !leftClosed || !rightClosed {
		t.Fatalf("unsupported half-close state left=(closed:%v aborted:%v) right=(closed:%v aborted:%v)", leftClosed, leftAborted, rightClosed, rightAborted)
	}
}
