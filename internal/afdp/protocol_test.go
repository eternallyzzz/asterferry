package afdp

import (
	"bytes"
	"encoding/binary"
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

func TestDatagramHeaderValidationIsSymmetric(t *testing.T) {
	tests := []struct {
		name   string
		header DatagramHeader
		want   error
	}{
		{name: "zero flow", header: DatagramHeader{Flags: DatagramFlagFin, FragmentCount: 1}, want: ErrMalformedFrame},
		{name: "zero fragment count", header: DatagramHeader{FlowID: 1}, want: ErrMalformedFrame},
		{name: "single fragment index is nonzero", header: DatagramHeader{FlowID: 1, FragmentIndex: 1, FragmentCount: 1, Flags: DatagramFlagFin}, want: ErrMalformedFrame},
		{name: "fragment index out of range", header: DatagramHeader{FlowID: 1, FragmentIndex: 2, FragmentCount: 2, Flags: DatagramFlagFragmented}, want: ErrMalformedFrame},
		{name: "too many fragments", header: DatagramHeader{FlowID: 1, FragmentCount: maxDatagramFragments + 1, Flags: DatagramFlagFragmented}, want: ErrFrameTooLarge},
		{name: "single fragment marked fragmented", header: DatagramHeader{FlowID: 1, FragmentCount: 1, Flags: DatagramFlagFragmented | DatagramFlagFin}, want: ErrMalformedFrame},
		{name: "multi fragment missing fragmented flag", header: DatagramHeader{FlowID: 1, FragmentCount: 2}, want: ErrMalformedFrame},
		{name: "unknown flag", header: DatagramHeader{FlowID: 1, FragmentCount: 1, Flags: DatagramFlagFin | 0x80}, want: ErrMalformedFrame},
		{name: "non-final fragment marked fin", header: DatagramHeader{FlowID: 1, FragmentIndex: 0, FragmentCount: 2, Flags: DatagramFlagFragmented | DatagramFlagFin}, want: ErrMalformedFrame},
		{name: "final fragment missing fin", header: DatagramHeader{FlowID: 1, FragmentIndex: 1, FragmentCount: 2, Flags: DatagramFlagFragmented}, want: ErrMalformedFrame},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := EncodeDatagram(test.header, nil, maxDatagramPayload); !errors.Is(err, test.want) {
				t.Fatalf("encode error = %v, want %v", err, test.want)
			}
			if _, _, err := DecodeDatagram(makeRawDatagramForTest(test.header), maxDatagramPayload); !errors.Is(err, test.want) {
				t.Fatalf("decode error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDatagramCanonicalRoundTripAndPayloadBounds(t *testing.T) {
	tests := []DatagramHeader{
		{FlowID: 3, Sequence: 4, FragmentCount: 1, Flags: DatagramFlagFin},
		{FlowID: 3, Sequence: 5, FragmentIndex: 0, FragmentCount: 2, Flags: DatagramFlagFragmented},
		{FlowID: 3, Sequence: 5, FragmentIndex: 1, FragmentCount: 2, Flags: DatagramFlagFragmented | DatagramFlagFin},
		{FlowID: 3, Sequence: 6, FragmentIndex: maxDatagramFragments - 1, FragmentCount: maxDatagramFragments, Flags: DatagramFlagFragmented | DatagramFlagFin},
	}
	for _, header := range tests {
		frame, err := EncodeDatagram(header, []byte("payload"), 0)
		if err != nil {
			t.Fatalf("encode %v: %v", header, err)
		}
		decoded, payload, err := DecodeDatagram(frame, 0)
		if err != nil {
			t.Fatalf("decode %v: %v", header, err)
		}
		if decoded != header || !bytes.Equal(payload, []byte("payload")) {
			t.Fatalf("round trip mismatch: header=%v payload=%q", decoded, payload)
		}
	}

	header := DatagramHeader{FlowID: 9, FragmentCount: 1, Flags: DatagramFlagFin}
	payload := bytes.Repeat([]byte{'x'}, maxDatagramPayload)
	frame, err := EncodeDatagram(header, payload, 0)
	if err != nil {
		t.Fatalf("maximum wire payload rejected: %v", err)
	}
	if _, decoded, err := DecodeDatagram(frame, 0); err != nil || len(decoded) != maxDatagramPayload {
		t.Fatalf("maximum wire payload decode failed: %v len=%d", err, len(decoded))
	}
	tooLarge := append(append([]byte(nil), payload...), 'x')
	if _, err := EncodeDatagram(header, tooLarge, 0); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("default payload limit error = %v, want %v", err, ErrFrameTooLarge)
	}
	if _, err := EncodeDatagram(header, tooLarge, maxDatagramPayload+1); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("clamped payload limit error = %v, want %v", err, ErrFrameTooLarge)
	}

	if _, err := NewReassembler(1, maxDatagramPayload, maxDatagramPayload, time.Second); err != nil {
		t.Fatalf("maximum reassembler payload rejected: %v", err)
	}
	if _, err := NewReassembler(1, maxDatagramPayload, maxDatagramPayload+1, time.Second); err == nil {
		t.Fatal("reassembler accepted payload limit above wire maximum")
	}
	if _, err := Fragments(1, 1, make([]byte, maxDatagramFragments+1), datagramHeaderBytes+1); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("fragment count limit error = %v, want %v", err, ErrFrameTooLarge)
	}
}

func makeRawDatagramForTest(header DatagramHeader) []byte {
	data := make([]byte, datagramHeaderBytes)
	data[0] = Version
	data[1] = header.Flags
	binary.BigEndian.PutUint16(data[2:4], datagramHeaderBytes)
	binary.BigEndian.PutUint64(data[4:12], header.FlowID)
	binary.BigEndian.PutUint32(data[12:16], header.Sequence)
	binary.BigEndian.PutUint16(data[16:18], header.FragmentIndex)
	binary.BigEndian.PutUint16(data[18:20], header.FragmentCount)
	binary.BigEndian.PutUint16(data[20:22], 0)
	return data
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
