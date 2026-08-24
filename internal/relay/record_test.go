package relay

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

type testStream struct {
	*bytes.Buffer
}

func (s *testStream) Close() error { return nil }

func TestBalancedRecordRoundTrip(t *testing.T) {
	profile, err := NewProfile("balanced", 4096, 512)
	if err != nil {
		t.Fatal(err)
	}
	underlying := &testStream{Buffer: &bytes.Buffer{}}
	writer := NewConn(underlying, profile)
	want := bytes.Repeat([]byte("aster"), 1200)
	if n, err := writer.Write(want); err != nil || n != len(want) {
		t.Fatalf("write: %d %v", n, err)
	}
	reader := NewConn(underlying, profile)
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round trip mismatch: got %d want %d", len(got), len(want))
	}
}

func TestRecordLimitRejectsOversizedRecord(t *testing.T) {
	profile, err := NewProfile("standard", 1024, 0)
	if err != nil {
		t.Fatal(err)
	}
	underlying := &testStream{Buffer: &bytes.Buffer{}}
	data := make([]byte, 8+1020+2)
	data[0] = 1
	data[4] = 3
	data[5] = 252
	data[7] = 2
	underlying.Write(data)
	reader := NewConn(underlying, profile)
	buf := make([]byte, 8)
	if _, err := reader.Read(buf); err == nil {
		t.Fatal("expected oversized record rejection")
	}
}

func TestIdleWatchClosesAfterInactivity(t *testing.T) {
	closed := make(chan struct{})
	touch, stop := StartIdleWatch(context.Background(), 40*time.Millisecond, func() { close(closed) })
	touch()
	select {
	case <-closed:
		t.Fatal("idle watcher closed too early")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-closed:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("idle watcher did not close")
	}
	stop()
}

func TestRelayProfilesPaddingAndHalfClose(t *testing.T) {
	for _, name := range []string{"", "unsupported"} {
		if _, err := NewProfile(name, 1024, 0); err == nil {
			t.Fatalf("profile %q should fail", name)
		}
	}
	if _, err := NewProfile("standard", 8, 0); err == nil {
		t.Fatal("record size without payload space should fail")
	}
	if _, err := NewProfile("balanced", 1024, -1); err == nil {
		t.Fatal("negative padding should fail")
	}
	if _, err := NewProfile("balanced", 1024, 1024); err == nil {
		t.Fatal("padding larger than record should fail")
	}
	if PaddingLength("standard", 512, 10) != 0 || PaddingLength("balanced", 100, 0) != 0 || PaddingLength("balanced", 20000, 100) != 0 {
		t.Fatal("padding bounds were not enforced")
	}
	if got := PaddingLength("balanced", 100, 512); got < 0 || got > 512 {
		t.Fatalf("padding length = %d", got)
	}

	underlying := &closeProbe{}
	conn := NewConn(underlying, Profile{Name: "standard", MaxRecordBytes: 1024})
	conn.CloseRead()
	conn.CloseWrite()
	if !underlying.readClosed || !underlying.writeClosed {
		t.Fatal("relay half-close methods were not forwarded")
	}
}

type closeProbe struct {
	readClosed  bool
	writeClosed bool
}

func (c *closeProbe) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *closeProbe) Write(p []byte) (int, error) { return len(p), nil }
func (c *closeProbe) Close() error                { return nil }
func (c *closeProbe) CloseRead()                  { c.readClosed = true }
func (c *closeProbe) CloseWrite()                 { c.writeClosed = true }
