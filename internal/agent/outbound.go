package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"asterferry/internal/proxy"
	"asterferry/internal/relay"
	"asterferry/internal/transport"
)

type agentOutbound struct {
	agent *Agent
}

func (o agentOutbound) OpenStream(ctx context.Context, target proxy.Target, path proxy.Path) (io.ReadWriteCloser, error) {
	if o.agent == nil {
		return nil, errors.New("agent outbound is unavailable")
	}
	if err := proxy.ValidatePath(path); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = o.agent.ctx
	}
	if path == proxy.PathDirect {
		return dialTarget(ctx, target, time.Duration(o.agent.cfg.Limits.DialTimeoutSec)*time.Second)
	}
	raw, err := o.agent.OpenProxyCandidates(ctx, target.Network, target.Host, target.Port, target.CandidateAddresses())
	if err != nil {
		return nil, err
	}
	limits := transport.LimitsFromConfig(o.agent.cfg.Limits, o.agent.cfg.StreamLimit)
	if negotiated, ok := raw.(interface{ sessionLimits() transport.Limits }); ok {
		limits = negotiated.sessionLimits()
	}
	return relay.NewConn(raw, o.agent.relayProfileWithLimits(o.agent.cfg.Obfuscation.ProxyProfile, limits)), nil
}

func (o agentOutbound) OpenDatagram(ctx context.Context, target proxy.Target, path proxy.Path) (io.ReadWriteCloser, error) {
	if o.agent == nil {
		return nil, errors.New("agent outbound is unavailable")
	}
	if err := proxy.ValidatePath(path); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = o.agent.ctx
	}
	if path == proxy.PathDirect {
		return dialTarget(ctx, target, time.Duration(o.agent.cfg.Limits.DialTimeoutSec)*time.Second)
	}
	return o.agent.OpenProxyCandidates(ctx, target.Network, target.Host, target.Port, target.CandidateAddresses())
}

func dialTarget(ctx context.Context, target proxy.Target, timeout time.Duration) (net.Conn, error) {
	addresses := target.CandidateAddresses()
	if len(addresses) == 0 {
		return (&net.Dialer{Timeout: timeout}).DialContext(ctx, target.Network, target.Address())
	}
	if target.Network != "tcp" || len(addresses) == 1 {
		var first error
		for _, address := range addresses {
			conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, target.Network, net.JoinHostPort(address, fmt.Sprint(target.Port)))
			if err == nil {
				return conn, nil
			}
			if first == nil {
				first = err
			}
		}
		return nil, first
	}
	return happyEyeballsDial(ctx, addresses, target.Port, timeout)
}

func happyEyeballsDial(ctx context.Context, addresses []string, port uint16, timeout time.Duration) (net.Conn, error) {
	if len(addresses) == 0 {
		return nil, errors.New("no destination addresses")
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
				timer := time.NewTimer(time.Duration(index) * 200 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					return
				}
			}
			conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", net.JoinHostPort(address, fmt.Sprint(port)))
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
