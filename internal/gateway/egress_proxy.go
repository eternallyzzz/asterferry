package gateway

import (
	"asterferry/internal/relay"
	"asterferry/internal/transport"
)

// egressProxy is the Gateway-side traffic service. Keeping the dispatch
// boundary separate from session authentication makes the latter easier to
// test without opening a destination connection.
type egressProxy struct {
	gateway *Gateway
}

func newEgressProxy(gateway *Gateway) *egressProxy {
	return &egressProxy{gateway: gateway}
}

func (p *egressProxy) TCP(sess *Session, stream transport.Stream, addresses []string, requestID uint64, profile relay.Profile) {
	p.gateway.proxyTCP(sess, stream, addresses, requestID, profile)
}

func (p *egressProxy) UDP(sess *Session, stream transport.Stream, addresses []string, requestID uint64, profile string) {
	p.gateway.proxyUDP(sess, stream, addresses, requestID, profile)
}
