// Package dial contains the shared outbound dialing policies used by both
// runtime roles.
package dial

import (
	"context"
	"errors"
	"net"
	"time"
)

const happyEyeballsDelay = 200 * time.Millisecond

// TCP dials complete host:port addresses with a staggered Happy-Eyeballs
// schedule. The first successful connection wins and losing connections are
// canceled and closed.
func TCP(ctx context.Context, addresses []string, timeout time.Duration) (net.Conn, error) {
	if len(addresses) == 0 {
		return nil, errors.New("no destination addresses")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		conn net.Conn
		err  error
	}
	results := make(chan result)
	for i, address := range addresses {
		go func(index int, address string) {
			if index > 0 {
				timer := time.NewTimer(time.Duration(index) * happyEyeballsDelay)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					return
				}
			}
			conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
			if err == nil {
				select {
				case results <- result{conn: conn}:
				case <-ctx.Done():
					_ = conn.Close()
				}
				return
			}
			select {
			case results <- result{err: err}:
			case <-ctx.Done():
			}
		}(i, address)
	}

	var first error
	for range addresses {
		select {
		case result := <-results:
			if result.err == nil {
				cancel()
				return result.conn, nil
			}
			if first == nil {
				first = result.err
			}
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
	}
	if first == nil {
		first = errors.New("all destination addresses failed")
	}
	return nil, first
}
