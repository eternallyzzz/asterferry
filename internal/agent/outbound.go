package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"asterferry/internal/proxy"
	"asterferry/internal/relay"
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
	raw, err := o.agent.OpenProxy(ctx, target.Network, target.Host, target.Port)
	if err != nil {
		return nil, err
	}
	return relay.NewConn(raw, o.agent.relayProfile(o.agent.cfg.Obfuscation.ProxyProfile)), nil
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
	return o.agent.OpenProxy(ctx, target.Network, target.Host, target.Port)
}
