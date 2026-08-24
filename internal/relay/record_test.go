package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"asterferry/internal/protocol"
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
	data := make([]byte, recordHeaderSize)
	data[0] = protocol.RelayRecordVersion
	binary.BigEndian.PutUint32(data[4:8], 1020)
	underlying.Write(data)
	reader := NewConn(underlying, profile)
	buf := make([]byte, 8)
	if _, err := reader.Read(buf); err == nil {
		t.Fatal("expected oversized record rejection")
	}
}

func TestRecordBatchesStayWithinWireLimitAndRoundTrip(t *testing.T) {
	for _, name := range []string{"standard", "balanced"} {
		t.Run(name, func(t *testing.T) {
			profile, err := NewProfileWithBatch(name, 1024, 128, 2048)
			if err != nil {
				t.Fatal(err)
			}
			wire := &countingStream{}
			writer := NewConn(wire, profile)
			want := bytes.Repeat([]byte("asterferry"), 700)
			if n, err := writer.Write(want); err != nil || n != len(want) {
				t.Fatalf("write: %d %v", n, err)
			}
			if len(wire.writes) < 2 {
				t.Fatalf("write batching did not split the payload: %d writes", len(wire.writes))
			}
			for _, size := range wire.writes {
				if size > 2048 {
					t.Fatalf("wire batch size %d exceeds limit", size)
				}
			}

			reader := NewConn(wire, profile)
			got, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("round trip mismatch: got %d want %d", len(got), len(want))
			}
		})
	}
}

func TestRecordHeaderValidation(t *testing.T) {
	profile, err := NewProfile("standard", 1024, 0)
	if err != nil {
		t.Fatal(err)
	}
	var original bytes.Buffer
	if _, err := NewConn(&testStream{Buffer: &original}, profile).Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	base := append([]byte(nil), original.Bytes()...)
	mutations := []struct {
		name string
		edit func([]byte)
	}{
		{"version", func(data []byte) { data[0] = protocol.Version - 1 }},
		{"flags", func(data []byte) { data[1] = 2 }},
		{"reserved", func(data []byte) { data[2] = 1 }},
		{"zero-payload", func(data []byte) { binary.BigEndian.PutUint32(data[4:8], 0) }},
		{"padding-without-flag", func(data []byte) { binary.BigEndian.PutUint32(data[8:12], 1) }},
		{"flag-without-padding", func(data []byte) { data[1] = recordFlagPadding }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			data := append([]byte(nil), base...)
			mutation.edit(data)
			reader := NewConn(&testStream{Buffer: bytes.NewBuffer(data)}, profile)
			if _, err := reader.Read(make([]byte, 32)); err == nil {
				t.Fatal("malformed record header was accepted")
			}
		})
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

func TestConnFlushForwardsToUnderlyingStream(t *testing.T) {
	profile, err := NewProfile("standard", 1024, 0)
	if err != nil {
		t.Fatal(err)
	}
	underlying := &countingStream{}
	conn := NewConn(underlying, profile)
	conn.Flush()
	conn.CloseWrite()
	if underlying.flushes != 2 {
		t.Fatalf("flush count = %d, want 2", underlying.flushes)
	}
}

func FuzzReadV5RelayRecordDoesNotPanic(f *testing.F) {
	profile, err := NewProfile("balanced", 1024, 128)
	if err != nil {
		f.Fatal(err)
	}
	valid := make([]byte, recordHeaderSize+1)
	valid[0] = protocol.RelayRecordVersion
	binary.BigEndian.PutUint32(valid[4:8], 1)
	valid[recordHeaderSize] = 'x'
	f.Add(valid)
	f.Fuzz(func(t *testing.T, data []byte) {
		reader := NewConn(&testStream{Buffer: bytes.NewBuffer(data)}, profile)
		_, _ = reader.Read(make([]byte, 64))
	})
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

type countingStream struct {
	bytes.Buffer
	writes  []int
	flushes int
}

func (s *countingStream) Write(p []byte) (int, error) {
	s.writes = append(s.writes, len(p))
	return s.Buffer.Write(p)
}

func (s *countingStream) Close() error { return nil }
func (s *countingStream) Flush()       { s.flushes++ }
