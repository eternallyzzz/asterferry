package agent

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"asterferry/internal/transport"
)

func TestSessionStreamLeaseBlocksAndReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := &Session{ctx: ctx, streamSem: make(chan struct{}, 1)}
	release, ok := sess.acquireStream(ctx)
	if !ok {
		t.Fatal("first stream lease should succeed")
	}
	second := make(chan bool, 1)
	go func() {
		lease, acquired := sess.acquireStream(ctx)
		if acquired {
			lease()
		}
		second <- acquired
	}()
	select {
	case <-second:
		t.Fatal("second lease acquired before first release")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case acquired := <-second:
		if !acquired {
			t.Fatal("second lease did not acquire after release")
		}
	case <-time.After(time.Second):
		t.Fatal("second lease remained blocked")
	}
}

func TestTryAcquireStreamDoesNotOverbook(t *testing.T) {
	sess := &Session{streamSem: make(chan struct{}, 1)}
	release, ok := sess.tryAcquireStream()
	if !ok {
		t.Fatal("first try-acquire should succeed")
	}
	if _, ok := sess.tryAcquireStream(); ok {
		t.Fatal("try-acquire should reject a full stream budget")
	}
	release()
	if release, ok = sess.tryAcquireStream(); !ok {
		t.Fatal("stream budget was not returned")
	} else {
		release()
	}
}

func TestLeasedStreamReleasesOnCloseAndCancel(t *testing.T) {
	stream := newLeaseTestStream()
	var mu sync.Mutex
	releases := 0
	release := func() {
		mu.Lock()
		releases++
		mu.Unlock()
	}
	leased := newLeasedStream(stream, release)
	if err := leased.Close(); err != nil {
		t.Fatal(err)
	}
	leased.Cancel()
	stream.cancel()
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	got := releases
	mu.Unlock()
	if got != 1 {
		t.Fatalf("lease released %d times, want once", got)
	}
}

type leaseTestStream struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func newLeaseTestStream() *leaseTestStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &leaseTestStream{ctx: ctx, cancel: cancel}
}

func (s *leaseTestStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (s *leaseTestStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *leaseTestStream) Close() error                { s.cancel(); return nil }
func (s *leaseTestStream) SetDeadline(time.Time) error { return nil }
func (s *leaseTestStream) Context() context.Context    { return s.ctx }
func (s *leaseTestStream) Cancel()                     { s.cancel() }

var _ transport.Stream = (*leaseTestStream)(nil)
