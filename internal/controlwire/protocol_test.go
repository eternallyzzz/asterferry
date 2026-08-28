package controlwire

import (
	"bytes"
	"testing"
	"time"

	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSnapshotProtoRoundTripAndChecksum(t *testing.T) {
	snapshot := domain.DesiredSnapshot{SchemaVersion: domain.SchemaVersion, NodeID: "agent-1", Generation: 3, Agent: &domain.AgentSpec{NodeID: "agent-1"}}
	encoded, err := SnapshotToProto(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := SnapshotFromProto(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.NodeID != snapshot.NodeID || decoded.Generation != snapshot.Generation || decoded.Checksum == "" {
		t.Fatalf("unexpected decoded snapshot: %#v", decoded)
	}
	encoded.DocumentJson[0] ^= 1
	if _, err := SnapshotFromProto(encoded); err == nil {
		t.Fatal("tampered snapshot accepted")
	}
}

func TestBoundedMessageFraming(t *testing.T) {
	message := &v1.Heartbeat{Healthy: true, AppliedGeneration: 2}
	var buffer bytes.Buffer
	if err := WriteMessage(&buffer, message, 1024); err != nil {
		t.Fatal(err)
	}
	var decoded v1.Heartbeat
	if err := ReadMessage(&buffer, &decoded, 1024); err != nil || !decoded.Healthy || decoded.AppliedGeneration != 2 {
		t.Fatalf("framing failed: %v healthy=%t generation=%d", err, decoded.Healthy, decoded.AppliedGeneration)
	}
}

func TestObservedProtoRejectsDuplicatedTimestampMismatch(t *testing.T) {
	state := domain.ObservedState{
		SchemaVersion:     domain.SchemaVersion,
		NodeID:            "agent-1",
		AppliedGeneration: 1,
		Healthy:           true,
		ObservedAt:        time.Unix(100, 0).UTC(),
	}
	encoded, err := ObservedToProto(state)
	if err != nil {
		t.Fatal(err)
	}
	encoded.ObservedAt = timestamppb.New(state.ObservedAt.Add(time.Second))
	if _, err := ObservedFromProto(encoded); err == nil {
		t.Fatal("observed timestamp metadata mismatch was accepted")
	}
}
