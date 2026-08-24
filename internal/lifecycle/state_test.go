package lifecycle

import (
	"context"
	"testing"
	"time"
)

func TestGateDrainsAdmittedWork(t *testing.T) {
	g := NewGate()
	if !g.TryAdd() {
		t.Fatal("running gate should admit work")
	}
	if !g.TryAdd() {
		t.Fatal("running gate should admit work")
	}
	if !g.BeginDrain() {
		t.Fatal("first drain transition should succeed")
	}
	if g.TryAdd() {
		t.Fatal("draining gate admitted new work")
	}
	done := make(chan error, 1)
	go func() { done <- g.Wait(context.Background()) }()
	select {
	case <-done:
		t.Fatal("drain completed before admitted work finished")
	case <-time.After(10 * time.Millisecond):
	}
	g.Done()
	g.Done()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not complete")
	}
	if got := g.State(); got != StateDraining {
		t.Fatalf("state after drain barrier = %s", got)
	}
}

func TestGateStopUnblocksWaitAndIsIdempotent(t *testing.T) {
	g := NewGate()
	if !g.BeginDrain() {
		t.Fatal("drain transition failed")
	}
	g.Stop()
	g.Stop()
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := g.State(); got != StateStopped {
		t.Fatalf("state = %s, want stopped", got)
	}
}

func TestGateWaitContext(t *testing.T) {
	g := NewGate()
	if !g.TryAdd() || !g.BeginDrain() {
		t.Fatal("failed to start drain")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := g.Wait(ctx); err == nil {
		t.Fatal("expected context deadline")
	}
	g.Done()
}

func TestStateStringAndNilGateBoundaries(t *testing.T) {
	for _, tc := range []struct {
		state State
		want  string
	}{
		{StateRunning, "running"},
		{StateDraining, "draining"},
		{StateStopped, "stopped"},
		{State(99), "unknown"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Fatalf("state %d string = %q, want %q", tc.state, got, tc.want)
		}
	}
	var gate *Gate
	if gate.State() != StateStopped || gate.IsRunning() || gate.TryAdd() || gate.BeginDrain() || gate.Active() != 0 {
		t.Fatal("nil gate boundary behavior is incorrect")
	}
	if err := gate.Wait(context.Background()); err != nil {
		t.Fatal("nil gate wait should be harmless: ", err)
	}
	gate.Done()
	gate.Stop()
}
