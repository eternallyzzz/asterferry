package node

import (
	"context"
	"math"
	"sync"
	"time"
)

// runtimeLimiter is a small token bucket.  It intentionally uses bytes, not
// packets, and wakes on the owning connection context so an operator
// disconnect cannot leave a writer asleep until its next refill.
type runtimeLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	ctx    context.Context
}

func newRuntimeLimiter(rate, burst uint64, ctx context.Context) *runtimeLimiter {
	if rate == 0 {
		return nil
	}
	if burst == 0 {
		burst = rate
	}
	if burst > math.MaxInt64 {
		burst = math.MaxInt64
	}
	return &runtimeLimiter{rate: float64(rate), burst: float64(burst), tokens: float64(burst), last: time.Now(), ctx: ctx}
}

func (l *runtimeLimiter) wait(bytes int) error {
	if l == nil || bytes <= 0 {
		return nil
	}
	remaining := bytes
	for remaining > 0 {
		chunk := remaining
		if maximum := int(l.burst); maximum > 0 && chunk > maximum {
			chunk = maximum
		}
		for {
			l.mu.Lock()
			now := time.Now()
			elapsed := now.Sub(l.last).Seconds()
			if elapsed > 0 {
				l.tokens += elapsed * l.rate
				if l.tokens > l.burst {
					l.tokens = l.burst
				}
				l.last = now
			}
			if l.tokens >= float64(chunk) {
				l.tokens -= float64(chunk)
				l.mu.Unlock()
				break
			}
			wait := time.Duration((float64(chunk) - l.tokens) / l.rate * float64(time.Second))
			if wait < time.Millisecond {
				wait = time.Millisecond
			}
			l.mu.Unlock()
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-l.ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return l.ctx.Err()
			}
		}
		remaining -= chunk
	}
	return nil
}
