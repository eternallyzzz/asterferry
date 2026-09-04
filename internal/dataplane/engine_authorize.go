package dataplane

import (
	"asterferry/internal/afdp"
	"errors"
	"fmt"
)

func (e *Engine) AuthorizeSession(hello afdp.SessionHello) error {
	if e.closed.Load() || e.draining.Load() {
		return errors.New("data-plane engine is not accepting sessions")
	}
	e.mu.RLock()
	if e.closed.Load() || e.draining.Load() {
		e.mu.RUnlock()
		return errors.New("data-plane engine is not accepting sessions")
	}
	assignment, ok := e.assignments[hello.AssignmentID]
	e.mu.RUnlock()
	if !ok {
		return afdp.ErrUnauthorizedAgent
	}
	if err := afdp.AuthorizeSession(hello, assignment); err != nil {
		return err
	}
	for {
		current := e.sessions.Load()
		if current >= int64(e.maxSessions) {
			return fmt.Errorf("%w: session limit reached", afdp.ErrTransient)
		}
		if e.sessions.CompareAndSwap(current, current+1) {
			return nil
		}
	}
}

// ReleaseSession must be called when the authenticated QUIC session closes.
// Calling it more than once is harmless and never drives the count negative.
func (e *Engine) ReleaseSession() {
	for {
		current := e.sessions.Load()
		if current <= 0 || e.sessions.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (e *Engine) AuthorizeOpen(assignmentID string, open afdp.OpenMetadata) error {
	_, err := e.ReserveOpen(assignmentID, open)
	return err
}

// ReserveOpen authorizes and reserves one reverse/proxy stream.  The lease
// must be released when the stream or UDP flow terminates.
func (e *Engine) ReserveOpen(assignmentID string, open afdp.OpenMetadata) (*OpenLease, error) {
	if e.closed.Load() || e.draining.Load() {
		return nil, errors.New("data-plane engine is not accepting opens")
	}
	// Keep the per-assignment reservation and the global stream reservation in
	// one short critical section. Checking the assignment count under a read
	// lock and incrementing it later would let concurrent opens oversubscribe a
	// small assignment even though each individual check passed.
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() || e.draining.Load() {
		return nil, errors.New("data-plane engine is not accepting opens")
	}
	assignment, assignmentOK := e.assignments[assignmentID]
	if !assignmentOK {
		return nil, afdp.ErrUnauthorizedAgent
	}
	if err := afdp.AuthorizeOpen(open, assignment); err != nil {
		return nil, err
	}
	if !open.Egress {
		service, serviceOK := e.services[open.ServiceID]
		if !serviceOK || !service.Enabled {
			return nil, afdp.ErrUnauthorizedAgent
		}
		if service.Protocol != open.Protocol || service.LocalTarget != open.Target {
			return nil, afdp.ErrUnauthorizedAgent
		}
	}
	maxStreams := e.maxStreams
	if assignment.MaxStreams > 0 && assignment.MaxStreams < maxStreams {
		maxStreams = assignment.MaxStreams
	}
	if e.active[assignmentID] >= maxStreams {
		return nil, fmt.Errorf("%w: assignment stream limit reached", afdp.ErrTransient)
	}
	for {
		current := e.streams.Load()
		if current >= int64(e.maxStreams) {
			return nil, fmt.Errorf("%w: stream limit reached", afdp.ErrTransient)
		}
		// The connection limit is a second global ceiling and must be checked
		// inside the same CAS loop as the stream limit. Checking it only before
		// the loop lets concurrent opens race past a one-connection policy when
		// the first goroutine wins the CAS and the second retries.
		if e.connectionLimit > 0 && current >= int64(e.connectionLimit) {
			return nil, fmt.Errorf("%w: connection limit reached", afdp.ErrTransient)
		}
		if e.streams.CompareAndSwap(current, current+1) {
			break
		}
	}
	e.active[assignmentID]++
	return &OpenLease{engine: e, assignmentID: assignmentID, epoch: e.epoch}, nil
}

// ReserveLocalOpen reserves a node-scoped stream for a local proxy.  It is
// the lease form of AuthorizeLocalOpen and has the same generation-safe
// cleanup semantics.
func (e *Engine) ReserveLocalOpen() (*OpenLease, error) {
	if e == nil || e.closed.Load() || e.draining.Load() {
		return nil, errors.New("data-plane engine is not accepting local opens")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() || e.draining.Load() {
		return nil, errors.New("data-plane engine is not accepting local opens")
	}
	for {
		current := e.streams.Load()
		if current >= int64(e.maxStreams) {
			return nil, fmt.Errorf("%w: stream limit reached", afdp.ErrTransient)
		}
		if e.connectionLimit > 0 && current >= int64(e.connectionLimit) {
			return nil, fmt.Errorf("%w: connection limit reached", afdp.ErrTransient)
		}
		if e.streams.CompareAndSwap(current, current+1) {
			return &OpenLease{engine: e, epoch: e.epoch}, nil
		}
	}
}

// AuthorizeLocalOpen reserves one node-scoped stream for a local Agent proxy
// connection.  Local proxy sockets do not belong to a reverse assignment, but
// they still consume the same bounded connection/stream budget advertised by
// the Agent snapshot.  The returned reservation must be released exactly once
// when the client connection finishes.
func (e *Engine) AuthorizeLocalOpen() error {
	_, err := e.ReserveLocalOpen()
	return err
}

// ReleaseLocalOpen releases a reservation returned by AuthorizeLocalOpen.
// It is saturating so a double cleanup cannot make the global counter
// negative.
func (e *Engine) ReleaseLocalOpen() { e.releaseStreamReservation() }

func (e *Engine) ReleaseOpen(assignmentID string) {
	e.releaseStreamReservation()
	e.mu.Lock()
	if count := e.active[assignmentID]; count <= 1 {
		delete(e.active, assignmentID)
	} else {
		e.active[assignmentID] = count - 1
	}
	e.mu.Unlock()
}

func (e *Engine) releaseLease(assignmentID string, epoch uint64) {
	e.releaseStreamReservation()
	if assignmentID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if epoch != e.epoch {
		return
	}
	if count := e.active[assignmentID]; count <= 1 {
		delete(e.active, assignmentID)
	} else {
		e.active[assignmentID] = count - 1
	}
}

func (e *Engine) releaseStreamReservation() {
	for {
		current := e.streams.Load()
		if current <= 0 || e.streams.CompareAndSwap(current, current-1) {
			return
		}
	}
}
