package transport

import (
	"bytes"
	"testing"
)

func TestFrameRoundTripAndBounds(t *testing.T) {
	want, err := MessageFrame(TypeData, 42, NewData([]byte("payload"), "standard", 0))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteFrame(&b, want, 1024); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&b, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != Version || got.Type != want.Type || got.RequestID != want.RequestID || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("round trip mismatch: %#v", got)
	}
	var data Data
	if err := DecodeMessage(got, &data); err != nil || string(data.Payload) != "payload" {
		t.Fatalf("payload decode mismatch: %#v %v", data, err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 0}), 1024); err == nil {
		t.Fatal("expected invalid length")
	}
	if err := WriteFrame(&b, Frame{Type: TypeData, Payload: make([]byte, 2048)}, 1024); err == nil {
		t.Fatal("expected write bound error")
	}
	if err := WriteFrame(&b, Frame{Version: 3, Type: TypePing}, 1024); err == nil {
		t.Fatal("v3 frame should be rejected")
	}
}

func TestChallengeSignatureBindsNegotiation(t *testing.T) {
	token := []byte("a-very-long-test-token")
	nonce := []byte("nonce")
	caps := []Capability{CapabilityErrorsV1, CapabilityLimitsV1, CapabilityReverseTCP}
	limits := Limits{MaxFrameBytes: 4096, MaxRecordBytes: 4096, MaxUDPBytes: 1024, MaxStreams: 8}
	mac := SignChallenge(token, nonce, "edge-a", caps, limits)
	if !VerifyChallenge(token, nonce, mac, "edge-a", caps, limits) {
		t.Fatal("signature should verify")
	}
	if VerifyChallenge(token, nonce, mac, "edge-b", caps, limits) || VerifyChallenge([]byte("different-token"), nonce, mac, "edge-a", caps, limits) {
		t.Fatal("signature should bind identity and token")
	}
	if VerifyChallenge(token, nonce, mac, "edge-a", []Capability{CapabilityErrorsV1, CapabilityLimitsV1}, limits) {
		t.Fatal("signature should bind capabilities")
	}
	if VerifyChallenge(token, nonce, mac, "edge-a", caps, Limits{MaxFrameBytes: 8192, MaxRecordBytes: 4096, MaxUDPBytes: 1024, MaxStreams: 8}) {
		t.Fatal("signature should bind limits")
	}
}

func TestDataPaddingAndBounds(t *testing.T) {
	f, err := MessageFrame(TypeData, 0, NewData([]byte("payload"), "balanced", 64))
	if err != nil {
		t.Fatal(err)
	}
	data, err := DecodeData(f, 1024, 64)
	if err != nil || string(data.Payload) != "payload" {
		t.Fatalf("decode data: %#v %v", data, err)
	}
	if _, err := DecodeData(f, 3, 64); err == nil {
		t.Fatal("expected payload limit error")
	}
}

func TestStructuredErrorRoundTrip(t *testing.T) {
	want := NewProtocolError(ErrorPolicyDenied, "egress policy denied", false)
	f, err := MessageFrame(TypeOpenError, 7, OpenResult{Error: want})
	if err != nil {
		t.Fatal(err)
	}
	var got OpenResult
	if err := DecodeMessage(f, &got); err != nil || got.Error == nil {
		t.Fatalf("decode error = %#v, err = %v", got.Error, err)
	}
	if got.Error.Code != ErrorPolicyDenied || got.Error.Detail != want.Detail || got.Error.Retryable {
		t.Fatalf("structured error mismatch: %#v", got.Error)
	}
}

func TestNegotiation(t *testing.T) {
	offered := []Capability{CapabilityErrorsV1, CapabilityLimitsV1, CapabilityReverseTCP}
	supported := []Capability{CapabilityErrorsV1, CapabilityLimitsV1, CapabilityRelayBalanced}
	selected, err := NegotiateCapabilities(offered, supported)
	if err != nil || len(selected) != 2 {
		t.Fatalf("selected capabilities = %#v, err = %v", selected, err)
	}
	limits, err := NegotiateLimits(
		Limits{MaxFrameBytes: 8192, MaxRecordBytes: 8192, MaxUDPBytes: 2048, MaxStreams: 8},
		Limits{MaxFrameBytes: 4096, MaxRecordBytes: 4096, MaxUDPBytes: 1024, MaxStreams: 4},
	)
	if err != nil || limits.MaxFrameBytes != 4096 || limits.MaxStreams != 4 {
		t.Fatalf("negotiated limits = %#v, err = %v", limits, err)
	}
}

func FuzzReadV4FrameDoesNotPanic(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ReadFrame(bytes.NewReader(data), 4096)
	})
}

func FuzzDecodeV4PayloadDoesNotPanic(f *testing.F) {
	f.Add([]byte{0x0a, 0x07, 'p', 'a', 'y', 'l', 'o', 'a', 'd'})
	f.Fuzz(func(t *testing.T, payload []byte) {
		var data Data
		_ = DecodeMessage(Frame{Version: Version, Type: TypeData, Payload: payload}, &data)
	})
}
