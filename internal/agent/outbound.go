package agent

import (
	"context"
	"errors"
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
		return (&net.Dialer{Timeout: time.Duration(o.agent.cfg.Limits.DialTimeoutSec) * time.Second}).DialContext(ctx, target.Network, target.Address())
	}
	host := target.Host
	if target.ResolvedIP.IsValid() {
		host = target.ResolvedIP.String()
	}
	raw, err := o.agent.OpenProxy(ctx, target.Network, host, target.Port)
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
		return (&net.Dialer{Timeout: time.Duration(o.agent.cfg.Limits.DialTimeoutSec) * time.Second}).DialContext(ctx, target.Network, target.Address())
	}
	host := target.Host
	if target.ResolvedIP.IsValid() {
		host = target.ResolvedIP.String()
	}
	return o.agent.OpenProxy(ctx, target.Network, host, target.Port)
}
