package dataplane

// TCP forwarding adapters keep socket ownership in the data plane. They are
// intentionally independent of Controller APIs: a caller supplies an
// already-authenticated AFDP session and a service from the applied snapshot.

import (
	"context"
	"errors"
	"io"
	"net"

	"asterferry/internal/afdp"
	"asterferry/internal/domain"
	"asterferry/internal/duplex"
	"github.com/quic-go/quic-go"
)

// ServeTCPReverse accepts public Gateway connections and opens one AFDP raw
// stream per connection. The first bytes on that stream are the bounded Open
// metadata; all following bytes are copied without per-chunk framing.
func ServeTCPReverse(ctx context.Context, engine *Engine, session *afdp.Session, listener net.Listener, service domain.Service) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if engine == nil || session == nil || listener == nil {
		return errors.New("TCP reverse requires engine, session and listener")
	}
	if service.Protocol != domain.ProtocolTCP {
		return errors.New("TCP reverse requires a TCP service")
	}
	listenerDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-listenerDone:
		}
	}()
	defer close(listenerDone)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		lease, err := engine.ReserveOpen(session.Assignment().ID, afdp.OpenMetadata{Protocol: domain.ProtocolTCP, ServiceID: service.ID, Target: service.LocalTarget})
		if err != nil {
			_ = conn.Close()
			continue
		}
		go func(local net.Conn, lease *OpenLease) {
			defer lease.Release()
			defer local.Close()
			stream, err := session.OpenStream(ctx, afdp.OpenMetadata{Protocol: domain.ProtocolTCP, ServiceID: service.ID, Target: service.LocalTarget})
			if err != nil {
				return
			}
			defer func() { _ = stream.Close(); session.ReleaseStream() }()
			copyDuplexLimited(local, stream, engine.MaxBufferBytes())
		}(conn, lease)
	}
}

// ServeTCPAgent consumes reverse streams on an Agent and dials each
// service.LocalTarget. The dial function is injectable for routing/egress
// policy and for deterministic tests.
func ServeTCPAgent(ctx context.Context, engine *Engine, session *afdp.Session, dial func(context.Context, string) (net.Conn, error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if engine == nil || session == nil || dial == nil {
		return errors.New("TCP agent forwarding requires engine, session and dialer")
	}
	for {
		stream, metadata, err := session.AcceptStream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// Gateway egress streams are Agent-to-Gateway proxy opens and are
		// terminated by the Gateway egress adapter.  This reverse-service
		// consumer must never reinterpret one as an Agent local-target dial.
		if metadata.Egress {
			_ = stream.Close()
			session.ReleaseStream()
			continue
		}
		lease, err := engine.ReserveOpen(session.Assignment().ID, metadata)
		if err != nil {
			_ = stream.Close()
			session.ReleaseStream()
			continue
		}
		// Pass the values as arguments.  This is a plain `for` loop rather than
		// a range loop, so capturing the iteration variables directly would let
		// a later accepted stream overwrite the metadata seen by an earlier
		// forwarding goroutine.
		go func(stream *quic.Stream, metadata afdp.OpenMetadata, lease *OpenLease) {
			defer lease.Release()
			defer func() { _ = stream.Close(); session.ReleaseStream() }()
			approvedTarget, releaseEgress, egressErr := engine.AcquireEgress(ctx, domain.ProtocolTCP, metadata.Target)
			if egressErr != nil {
				return
			}
			defer releaseEgress()
			local, dialErr := dial(ctx, approvedTarget)
			if dialErr != nil {
				return
			}
			defer local.Close()
			copyDuplexLimited(local, stream, engine.MaxBufferBytes())
		}(stream, metadata, lease)
	}
}

// copyDuplexLimited keeps the two directional buffers within the node's
// declared MaxBufferBytes budget and preserves TCP/QUIC half-close semantics.
// The default remains two 32 KiB buffers; small configured budgets are honored
// by shrinking each side to half the total instead of silently falling back to
// io.Copy's allocation.
func copyDuplexLimited(left, right io.ReadWriteCloser, maxBuffer int) {
	_ = duplex.CopyDuplex(left, right, maxBuffer)
}
