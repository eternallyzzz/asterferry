package agent

import (
	"context"
	"sync"

	"asterferry/internal/transport"
)

// acquireStream reserves one data stream on an Agent session. The control
// stream is created before this semaphore and therefore does not consume a
// data slot.
func (s *Session) acquireStream(ctx context.Context) (func(), bool) {
	if s == nil {
		return func() {}, false
	}
	if s.streamSem == nil {
		return func() {}, true
	}
	if ctx == nil {
		ctx = s.ctx
	}
	select {
	case s.streamSem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.streamSem }) }, true
	case <-ctx.Done():
		return func() {}, false
	case <-s.ctx.Done():
		return func() {}, false
	}
}

func (s *Session) tryAcquireStream() (func(), bool) {
	if s == nil {
		return func() {}, false
	}
	if s.streamSem == nil {
		return func() {}, true
	}
	select {
	case s.streamSem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.streamSem }) }, true
	default:
		return func() {}, false
	}
}

// leasedStream releases its session slot on every terminal path. The context
// watcher handles remote resets; Close and Cancel cover local shutdown.
type leasedStream struct {
	transport.Stream
	release     func()
	releaseOnce sync.Once
	limits      transport.Limits
}

func newLeasedStream(stream transport.Stream, release func(), negotiated ...transport.Limits) *leasedStream {
	var limits transport.Limits
	if len(negotiated) > 0 {
		limits = negotiated[0]
	}
	result := &leasedStream{Stream: stream, release: release, limits: limits}
	if stream != nil {
		go func() {
			<-stream.Context().Done()
			result.releaseLease()
		}()
	}
	return result
}

// sessionLimits lets protocol-aware outbound handlers apply the limits chosen
// during the Agent/Gateway handshake without coupling the proxy package to the
// Session type.
func (s *leasedStream) sessionLimits() transport.Limits {
	if s == nil {
		return transport.Limits{}
	}
	return s.limits
}

func (s *leasedStream) releaseLease() {
	if s == nil {
		return
	}
	s.releaseOnce.Do(func() {
		if s.release != nil {
			s.release()
		}
	})
}

func (s *leasedStream) Close() error {
	if s == nil || s.Stream == nil {
		return nil
	}
	err := s.Stream.Close()
	s.releaseLease()
	return err
}

func (s *leasedStream) Cancel() {
	if s == nil || s.Stream == nil {
		return
	}
	s.Stream.Cancel()
	s.releaseLease()
}
