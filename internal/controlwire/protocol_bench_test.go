package controlwire

import (
	"bytes"
	"testing"

	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
)

func BenchmarkControlwireSnapshotEncodeDecode(b *testing.B) {
	snapshot := domain.DesiredSnapshot{
		SchemaVersion: domain.CurrentControlProtocolVersion,
		NodeID:        "agent-1",
		Generation:    7,
		Agent:         &domain.AgentSpec{NodeID: "agent-1"},
	}
	encoded, err := SnapshotToProto(snapshot)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := SnapshotFromProto(encoded); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkControlwireMessageFraming(b *testing.B) {
	message := &v1.Heartbeat{Healthy: true, AppliedGeneration: 7}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buffer bytes.Buffer
		if err := WriteMessage(&buffer, message, 4096); err != nil {
			b.Fatal(err)
		}
		var decoded v1.Heartbeat
		if err := ReadMessage(&buffer, &decoded, 4096); err != nil {
			b.Fatal(err)
		}
	}
}
