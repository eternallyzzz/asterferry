package afdp

import "testing"

func FuzzDecodeAFDPFrames(f *testing.F) {
	for _, seed := range [][]byte{
		{Version, SessionHelloKind, 0, 0, 0, 0},
		{Version, SessionAcceptKind, 0, 0, 0, 0},
		{Version, OpenKind, 0, 0, 0, 0},
		{Version, 0, 0, 0, 0, 0},
		{0xff, 0xff, 0, 0, 0, 0},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = DecodeSessionHello(data, 4096)
		_, _ = DecodeSessionAccept(data, 4096)
		_, _ = DecodeOpen(data, 4096)
		_, _, _ = DecodeDatagram(data, 1200)
	})
}
