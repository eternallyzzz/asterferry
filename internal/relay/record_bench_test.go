package relay

import (
	"bytes"
	"io"
	"testing"
)

type benchmarkBuffer struct{ bytes.Buffer }

func (benchmarkBuffer) Close() error { return nil }

func BenchmarkConnRoundTrip(b *testing.B) {
	payload := bytes.Repeat([]byte("a"), 64<<10)
	for _, profileName := range []string{"standard", "balanced"} {
		b.Run(profileName, func(b *testing.B) {
			profile, err := NewProfileWithBatch(profileName, 16<<10, 2048, 256<<10)
			if err != nil {
				b.Fatal(err)
			}
			var wire benchmarkBuffer
			conn := NewConn(&wire, profile)
			result := make([]byte, len(payload))
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := conn.Write(payload); err != nil {
					b.Fatal(err)
				}
				if _, err := io.ReadFull(conn, result); err != nil {
					b.Fatal(err)
				}
				wire.Reset()
			}
		})
	}
}
