package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"asterferry/internal/dial"
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
	fullAddresses := make([]string, 0, len(addresses))
	for _, address := range addresses {
		fullAddresses = append(fullAddresses, net.JoinHostPort(address, fmt.Sprint(target.Port)))
	}
	if target.Network == "tcp" {
		return dial.TCP(ctx, fullAddresses, timeout)
	}
	var first error
	for _, address := range fullAddresses {
		conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, target.Network, address)
		if err == nil {
			return conn, nil
		}
		if first == nil {
			first = err
		}
	}
	return nil, first
}
