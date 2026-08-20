package relay

import (
	"bytes"
	"testing"
)

func FuzzRecordReaderDoesNotPanic(f *testing.F) {
	f.Add([]byte{1, 0, 0, 1, 0, 0, 0, 0, 'x'})
	f.Fuzz(func(t *testing.T, data []byte) {
		profile, err := NewProfile("balanced", 4096, 512)
		if err != nil {
			t.Fatal(err)
		}
		reader := NewConn(&fuzzStream{Buffer: bytes.NewBuffer(data)}, profile)
		buf := make([]byte, 256)
		_, _ = reader.Read(buf)
	})
}

type fuzzStream struct {
	*bytes.Buffer
}

func (s *fuzzStream) Close() error { return nil }
