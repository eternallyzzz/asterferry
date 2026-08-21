package transport

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"asterferry/internal/config"
)

type fakeStream struct {
	ctx       context.Context
	cancelled chan struct{}
	once      sync.Once
}

func (s *fakeStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (s *fakeStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *fakeStream) Close() error                { return nil }
func (s *fakeStream) SetDeadline(time.Time) error { return nil }
func (s *fakeStream) Context() context.Context    { return s.ctx }
func (s *fakeStream) Cancel()                     { s.once.Do(func() { close(s.cancelled) }) }

func TestSetStreamContextCancelsAdapterStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeStream{ctx: context.Background(), cancelled: make(chan struct{})}
	stop := SetStreamContext(stream, ctx)
	cancel()
	select {
	case <-stream.cancelled:
	case <-time.After(time.Second):
		t.Fatal("stream was not canceled with context")
	}
	stop()
}

func TestQUICConfigAppliesReceiveWindows(t *testing.T) {
	cfg := config.TransportConfig{
		MaxBidiRemoteStreams:           32,
		InitialStreamReceiveWindow:     1 << 20,
		InitialConnectionReceiveWindow: 8 << 20,
		MaxStreamReadBuffer:            16 << 20,
		MaxConnReadBuffer:              64 << 20,
	}
	q := quicConfig(cfg)
	if q.MaxIncomingStreams != cfg.MaxBidiRemoteStreams || q.InitialStreamReceiveWindow != uint64(cfg.InitialStreamReceiveWindow) || q.InitialConnectionReceiveWindow != uint64(cfg.InitialConnectionReceiveWindow) || q.MaxStreamReceiveWindow != uint64(cfg.MaxStreamReadBuffer) || q.MaxConnectionReceiveWindow != uint64(cfg.MaxConnReadBuffer) {
		t.Fatalf("QUIC receive windows were not applied: %#v", q)
	}
}
