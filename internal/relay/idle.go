package relay

import (
	"context"
	"sync/atomic"
	"time"
)

// StartIdleWatch closes a transport when it has seen no activity for timeout.
// The touch function is safe to call from concurrent reader and writer loops.
// A non-positive timeout disables the watcher. stop waits until the watcher has
// exited, which keeps shutdown deterministic for callers that own both loops.
func StartIdleWatch(ctx context.Context, timeout time.Duration, closeFn func()) (touch func(), stop func()) {
	if timeout <= 0 {
		return func() {}, func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	watchCtx, cancel := context.WithCancel(ctx)
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	touch = func() { last.Store(time.Now().UnixNano()) }
	done := make(chan struct{})
	go func() {
		defer close(done)
		interval := timeout / 2
		if interval < 100*time.Millisecond {
			interval = 100 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case now := <-ticker.C:
				if now.Sub(time.Unix(0, last.Load())) >= timeout {
					if closeFn != nil {
						closeFn()
					}
					return
				}
			}
		}
	}()
	stop = func() {
		cancel()
		<-done
	}
	return touch, stop
}
