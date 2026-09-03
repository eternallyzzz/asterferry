package controlwire

import (
	"bytes"
	"testing"

	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
)

func FuzzControlwireDecoders(f *testing.F) {
	var seed bytes.Buffer
	if err := WriteMessage(&seed, &v1.Heartbeat{Healthy: true, AppliedGeneration: 1}, 4096); err == nil {
		f.Add(seed.Bytes())
	}
	f.Add([]byte{0, 0, 0, 1, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		var heartbeat v1.Heartbeat
		_ = ReadMessage(bytes.NewReader(data), &heartbeat, 4096)
		_, _ = SnapshotFromProto(&v1.DesiredSnapshot{SchemaVersion: uint32(domain.SchemaVersion), NodeId: "agent-1", Generation: 1, Checksum: "invalid", DocumentJson: data})
		_, _ = ObservedFromProto(&v1.ObservedState{SchemaVersion: uint32(domain.SchemaVersion), NodeId: "agent-1", AppliedGeneration: 1, DocumentJson: data})
	})
}
