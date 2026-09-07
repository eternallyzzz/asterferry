package afdp

import (
	"bytes"
	"testing"
)

func BenchmarkAFDPSessionHelloEncodeDecode(b *testing.B) {
	value := SessionHello{AssignmentID: "assignment", Generation: 7, AgentID: "agent", Capabilities: []string{"tcp", "udp"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		encoded, err := EncodeSessionHello(value, 1024)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := DecodeSessionHello(encoded, 1024); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAFDPDatagramEncodeDecode(b *testing.B) {
	header := DatagramHeader{FlowID: 7, Sequence: 9, FragmentCount: 1, Flags: DatagramFlagFin}
	payload := bytes.Repeat([]byte("asterferry"), 128)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		encoded, err := EncodeDatagram(header, payload, 0)
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := DecodeDatagram(encoded, 0); err != nil {
			b.Fatal(err)
		}
	}
}
