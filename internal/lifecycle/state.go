package lifecycle

import (
	"context"
	"errors"
	"sync"
)

// State is the process lifecycle state exposed by role runtimes.
type State uint8

const (
	StateRunning State = iota
	StateDraining
	StateStopped
)

func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateDraining:
		return "draining"
	case StateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Gate combines admission control with a drain barrier. Work must be
// admitted through TryAdd before its goroutine starts. Once BeginDrain
// succeeds, no new work is admitted and Wait returns when all admitted work
// has completed. The mutex makes the TryAdd/BeginDrain boundary safe without
// relying on the fragile Add-while-Waiting pattern of sync.WaitGroup.
type Gate struct {
	mu     sync.Mutex
	state  State
	active int
	done   chan struct{}
	once   sync.Once
}

func NewGate() *Gate {
	return &Gate{state: StateRunning, done: make(chan struct{})}
}

func (g *Gate) State() State {
	if g == nil {
		return StateStopped
	}
	g.mu.Lock()
	state := g.state
	g.mu.Unlock()
	return state
}

func (g *Gate) IsRunning() bool { return g != nil && g.State() == StateRunning }

// TryAdd admits one unit of work while the gate is running.
func (g *Gate) TryAdd() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != StateRunning {
		return false
	}
	g.active++
	return true
}

func (g *Gate) Done() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.active > 0 {
		g.active--
	}
	if g.state == StateDraining && g.active == 0 {
		g.closeDoneLocked()
	}
	g.mu.Unlock()
}

// BeginDrain transitions a running gate to draining. It returns true only
// for the caller that performed the transition; repeated callers can safely
// wait on the same barrier.
func (g *Gate) BeginDrain() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != StateRunning {
		return false
	}
	g.state = StateDraining
	if g.active == 0 {
		g.closeDoneLocked()
	}
	return true
}

// Stop marks the gate stopped and releases any waiter. It is used by the
// hard-close path and is intentionally idempotent.
func (g *Gate) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.state = StateStopped
	g.closeDoneLocked()
	g.mu.Unlock()
}

func (g *Gate) Wait(ctx context.Context) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	state := g.state
	done := g.done
	g.mu.Unlock()
	if state == StateRunning {
		return errors.New("lifecycle gate is not draining")
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Gate) Active() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	active := g.active
	g.mu.Unlock()
	return active
}

func (g *Gate) closeDoneLocked() { g.once.Do(func() { close(g.done) }) }
