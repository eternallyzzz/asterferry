package relay

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"asterferry/internal/transport"
)

// UDPPumpOptions describes the limits and accounting policy for a framed UDP
// association. The OpenOK handshake is intentionally left to the caller.
type UDPPumpOptions struct {
	MaxFrameBytes   int64
	MaxUDPBytes     int64
	MaxPaddingBytes int64
	Profile         string
	IdleTimeout     time.Duration
	Counters        Counters
}

// BidirectionalUDP relays framed stream data to a connected UDP socket and
// sends UDP datagrams back as framed data. The first terminal error closes
// both endpoints, cancels the sibling loop, waits for it, and is returned.
func BidirectionalUDP(ctx context.Context, stream transport.Stream, conn *net.UDPConn, options UDPPumpOptions) error {
	if stream == nil || conn == nil {
		return errors.New("UDP relay endpoints are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if options.MaxFrameBytes <= 0 {
		options.MaxFrameBytes = transport.DefaultMaxFrame
	}
	if options.MaxUDPBytes <= 0 {
		return errors.New("UDP relay limit is invalid")
	}
	if options.MaxPaddingBytes < 0 {
		options.MaxPaddingBytes = 0
	}

	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	touch, stopIdle := StartIdleWatch(pumpCtx, options.IdleTimeout, func() {
		_ = stream.Close()
		_ = conn.Close()
	})
	defer stopIdle()

	var (
		firstErr error
		errOnce  sync.Once
		wg       sync.WaitGroup
	)
	stop := func(err error) {
		if err == nil {
			err = errors.New("UDP relay stopped")
		}
		errOnce.Do(func() {
			firstErr = err
			cancel()
			_ = stream.Close()
			_ = conn.Close()
		})
	}
	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-pumpCtx.Done():
			if ctx.Err() != nil {
				stop(ctx.Err())
			}
		case <-ctxDone:
		}
	}()

	streamToUDP := func() {
		defer wg.Done()
		for {
			frame, err := transport.ReadFrame(stream, options.MaxFrameBytes)
			if err != nil {
				stop(err)
				return
			}
			if frame.Type != transport.TypeData {
				stop(errors.New("unexpected UDP data frame"))
				return
			}
			data, err := transport.DecodeData(frame, options.MaxUDPBytes, options.MaxPaddingBytes)
			if err != nil {
				stop(err)
				return
			}
			if _, err := conn.Write(data.Payload); err != nil {
				stop(err)
				return
			}
			touch()
			if options.Counters.In != nil {
				options.Counters.In(uint64(len(data.Payload)))
			}
		}
	}
	udpToStream := func() {
		defer wg.Done()
		buf := make([]byte, options.MaxUDPBytes)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			n, err := conn.Read(buf)
			if n > 0 {
				touch()
				frame, frameErr := transport.MessageFrame(transport.TypeData, 0, transport.NewData(buf[:n], options.Profile, options.MaxPaddingBytes))
				if frameErr != nil {
					stop(frameErr)
					return
				}
				if frameErr = transport.WriteFrame(stream, frame, options.MaxFrameBytes); frameErr != nil {
					stop(frameErr)
					return
				}
				if options.Counters.Out != nil {
					options.Counters.Out(uint64(n))
				}
			}
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					select {
					case <-pumpCtx.Done():
						stop(pumpCtx.Err())
						return
					default:
						continue
					}
				}
				stop(err)
				return
			}
		}
	}

	wg.Add(2)
	go streamToUDP()
	go udpToStream()
	wg.Wait()
	close(ctxDone)
	if firstErr == nil {
		firstErr = pumpCtx.Err()
	}
	return firstErr
}
