package lifecycle

import (
	"sync"
	"sync/atomic"
)

// ShutdownTrigger is the in-process bridge between a management action and
// the command runner's signal wait loop. It is deliberately one-shot: a
// shutdown request must not start multiple concurrent drain procedures.
type ShutdownTrigger struct {
	once    sync.Once
	ch      chan struct{}
	set     atomic.Bool
	restart atomic.Bool
}

func NewShutdownTrigger() *ShutdownTrigger {
	return &ShutdownTrigger{ch: make(chan struct{})}
}

// Request reports whether this call won the one-shot shutdown race.
func (t *ShutdownTrigger) Request() bool {
	return t.request(false)
}

// RequestRestart asks the command runner to drain and exit with a restart
// status so an external supervisor can load the newly written configuration.
func (t *ShutdownTrigger) RequestRestart() bool {
	return t.request(true)
}

func (t *ShutdownTrigger) request(restart bool) bool {
	if t == nil {
		return false
	}
	requested := false
	t.once.Do(func() {
		requested = true
		t.restart.Store(restart)
		t.set.Store(true)
		close(t.ch)
	})
	return requested
}

func (t *ShutdownTrigger) RestartRequested() bool {
	return t != nil && t.restart.Load()
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
