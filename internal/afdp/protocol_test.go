package afdp

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"asterferry/internal/domain"
)

func TestSessionAndOpenRoundTrip(t *testing.T) {
	hello := SessionHello{AssignmentID: "assignment", Generation: 2, AgentID: "agent", Capabilities: []string{"udp", "tcp"}}
	encoded, err := EncodeSessionHello(hello, 1024)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSessionHello(encoded, 1024)
	if err != nil || decoded.AssignmentID != hello.AssignmentID {
		t.Fatalf("hello round trip: %v %#v", err, decoded)
	}
	open := OpenMetadata{Protocol: "tcp", ServiceID: "service", Target: "127.0.0.1:80"}
	encoded, err = EncodeOpen(open, 1024)
	if err != nil {
		t.Fatal(err)
	}
	decodedOpen, err := DecodeOpen(encoded, 1024)
	if err != nil || decodedOpen.Target != open.Target {
		t.Fatalf("open round trip: %v %#v", err, decodedOpen)
	}
	if _, err := DecodeOpen(append(encoded, 0), 1024); err == nil {
		t.Fatal("trailing open bytes accepted")
	}
	egress := OpenMetadata{Protocol: "tcp", Target: "example.com:443", Egress: true}
	encoded, err = EncodeOpen(egress, 1024)
	if err != nil {
		t.Fatal(err)
	}
	decodedEgress, err := DecodeOpen(encoded, 1024)
	if err != nil || !decodedEgress.Egress || decodedEgress.ServiceID != "" {
		t.Fatalf("egress open round trip: %v %#v", err, decodedEgress)
	}
	if _, err := EncodeOpen(OpenMetadata{Protocol: "tcp", ServiceID: "service", Target: "example.com:443", Egress: true}, 1024); err == nil {
		t.Fatal("egress open with a service id was accepted")
	}
}

func TestDatagramFragmentationAndBounds(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1000)
	frames, err := Fragments(4, 9, payload, 128)
	if err != nil {
		t.Fatal(err)
	}
	reassembler, err := NewReassembler(4, 4096, 128, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var result []byte
	for i := len(frames) - 1; i >= 0; i-- {
		value, complete, err := reassembler.Add(frames[i], time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			result = value
		}
	}
	if !bytes.Equal(result, payload) {
		t.Fatalf("reassembled payload mismatch: %d", len(result))
	}
	if _, _, err := DecodeDatagram([]byte{Version}, 128); err == nil {
		t.Fatal("truncated datagram accepted")
	}
}

func TestDegradedAssignmentRejectsNewSessionsAndOpens(t *testing.T) {
	assignment := AssignmentView{
		ID:         "assignment",
		AgentID:    "agent",
		Generation: 1,
		State:      domain.AssignmentDegraded,
		ServiceIDs: map[string]struct{}{"service": {}},
	}
	hello := SessionHello{AssignmentID: "assignment", AgentID: "agent", Generation: 1}
	if err := AuthorizeSession(hello, assignment); !errors.Is(err, ErrUnauthorizedAgent) {
		t.Fatalf("degraded session was accepted: %v", err)
	}
	open := OpenMetadata{Protocol: domain.ProtocolTCP, ServiceID: "service", Target: "127.0.0.1:80"}
	if err := AuthorizeOpen(open, assignment); !errors.Is(err, ErrUnauthorizedAgent) {
		t.Fatalf("degraded open was accepted: %v", err)
	}
}
