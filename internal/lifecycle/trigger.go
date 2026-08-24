package lifecycle

import (
	"sync"
	"sync/atomic"
)

// ShutdownTrigger is the in-process bridge between a management action and
// the command runner's signal wait loop. It is deliberately one-shot: a
// shutdown request must not start multiple concurrent drain procedures.
type ShutdownTrigger struct {
	once sync.Once
	ch   chan struct{}
	set  atomic.Bool
}

func NewShutdownTrigger() *ShutdownTrigger {
	return &ShutdownTrigger{ch: make(chan struct{})}
}

// Request reports whether this call won the one-shot shutdown race.
func (t *ShutdownTrigger) Request() bool {
	if t == nil {
		return false
	}
	requested := false
	t.once.Do(func() {
		requested = true
		t.set.Store(true)
		close(t.ch)
	})
	return requested
}

func (t *ShutdownTrigger) Requested() bool {
	return t != nil && t.set.Load()
}

// C returns a channel that closes after the first shutdown request.
func (t *ShutdownTrigger) C() <-chan struct{} {
	if t == nil {
		return nil
	}
	return t.ch
}
