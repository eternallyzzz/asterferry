package transport

import (
	"bytes"
	"encoding/binary"
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
		t.Fatal("old frame version should be rejected")
	}
}

func TestLimitsFallbackAndFrameErrorHelpers(t *testing.T) {
	limits := (Limits{MaxFrameBytes: 2048, MaxRecordBytes: 1024}).WithFallback(Limits{MaxFrameBytes: 4096, MaxRecordBytes: 2048, MaxUDPBytes: 512, MaxStreams: 4})
	if limits.MaxFrameBytes != 2048 || limits.MaxRecordBytes != 1024 || limits.MaxUDPBytes != 512 || limits.MaxStreams != 4 {
		t.Fatalf("limits fallback = %#v", limits)
	}
	if got := limits.EffectivePadding(4096); got != 1012 {
		t.Fatalf("effective padding = %d, want 1012", got)
	}
	var wire bytes.Buffer
	if err := WriteOpenError(&wire, 1, ErrorPolicyDenied, "denied", false, 1024); err != nil {
		t.Fatal(err)
	}
	if frame, err := ReadFrame(&wire, 1024); err != nil || frame.Type != TypeOpenError {
		t.Fatalf("open error frame = %#v, %v", frame, err)
	}
	wire.Reset()
	if err := WriteProtocolError(&wire, 2, ErrorInternal, "internal", true, 1024); err != nil {
		t.Fatal(err)
	}
	if frame, err := ReadFrame(&wire, 1024); err != nil || frame.Type != TypeError {
		t.Fatalf("protocol error frame = %#v, %v", frame, err)
	}
	if got := MustMessageFrame(TypePing, 3, nil); got.Type != TypePing || got.RequestID != 3 {
		t.Fatalf("must frame = %#v", got)
	}
}

func TestV5FrameHeaderAndPayloadAreStrict(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteFrame(&wire, Frame{Type: TypeData, RequestID: 9, Payload: []byte("x")}, 1024); err != nil {
		t.Fatal(err)
	}
	encoded := wire.Bytes()
	if len(encoded) != frameHeaderBytes+1 || encoded[0] != Version || encoded[1] != TypeData || binary.BigEndian.Uint16(encoded[2:4]) != 0 {
		t.Fatalf("unexpected v5 header: %x", encoded)
	}

	badFlags := append([]byte(nil), encoded...)
	binary.BigEndian.PutUint16(badFlags[2:4], 1)
	if _, err := ReadFrame(bytes.NewReader(badFlags), 1024); err == nil {
		t.Fatal("reserved frame flags were accepted")
	}

	badVersion := append([]byte(nil), encoded...)
	badVersion[0] = 4
	if _, err := ReadFrame(bytes.NewReader(badVersion), 1024); err == nil {
		t.Fatal("v4 frame was accepted")
	}

	tooLarge := append([]byte(nil), encoded...)
	binary.BigEndian.PutUint32(tooLarge[4:8], 100)
	if _, err := ReadFrame(bytes.NewReader(tooLarge), frameHeaderBytes+1); err == nil {
		t.Fatal("frame larger than the configured limit was accepted")
	}

	withTrailing := append([]byte(nil), encoded[frameHeaderBytes:]...)
	withTrailing = append(withTrailing, 0)
	if err := DecodeMessage(Frame{Version: Version, Type: TypeData, Payload: withTrailing}, &Data{}); err == nil {
		t.Fatal("trailing payload bytes were accepted")
	}
	if err := DecodeMessage(Frame{Version: Version, Type: TypePing, Payload: []byte{0}}, nil); err == nil {
		t.Fatal("non-empty ping payload was accepted")
	}
}

func TestV5PayloadRejectsMalformedOptionalErrorsAndIntegers(t *testing.T) {
	for _, payload := range [][]byte{
		{2},             // invalid presence marker
		{1, 1, 0},       // truncated boolean
		{1, 1, 0, 2},    // invalid boolean
		{1, 1, 0, 0, 0}, // trailing byte
	} {
		if err := DecodeMessage(Frame{Version: Version, Type: TypeOpenError, Payload: payload}, &OpenResult{}); err == nil {
			t.Fatalf("malformed optional error payload %x was accepted", payload)
		}
	}

	var decoder frameDecoder
	decoder.buf = bytes.Repeat([]byte{0x80}, binary.MaxVarintLen64+1)
	if _, err := decoder.uvarint(); err == nil {
		t.Fatal("overflowing uvarint was accepted")
	}

	var valid frameEncoder
	valid.uvarint(1)
	valid.uvarint(uint64(ErrorPolicyDenied))
	valid.string("denied")
	valid.boolean(true)
	if err := DecodeMessage(Frame{Version: Version, Type: TypeOpenError, Payload: valid.buf}, &OpenResult{}); err != nil {
		t.Fatal(err)
	}

}

func TestChallengeSignatureBindsNegotiation(t *testing.T) {
	token := []byte("a-very-long-test-token")
	nonce := []byte("nonce")
	caps := []Capability{CapabilityErrorsV1, CapabilityLimitsV1, CapabilityReverseTCP}
	limits := Limits{MaxFrameBytes: 4096, MaxRecordBytes: 4096, MaxWriteBatchBytes: 8192, MaxUDPBytes: 1024, MaxStreams: 8}
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
	if VerifyChallenge(token, nonce, mac, "edge-a", caps, Limits{MaxFrameBytes: 8192, MaxRecordBytes: 4096, MaxWriteBatchBytes: 8192, MaxUDPBytes: 1024, MaxStreams: 8}) {
		t.Fatal("signature should bind limits")
	}
	if VerifyChallenge(token, nonce, mac, "edge-a", caps, Limits{MaxFrameBytes: 4096, MaxRecordBytes: 4096, MaxWriteBatchBytes: 16384, MaxUDPBytes: 1024, MaxStreams: 8}) {
		t.Fatal("signature should bind write-batch limit")
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
		Limits{MaxFrameBytes: 8192, MaxRecordBytes: 8192, MaxWriteBatchBytes: 8192, MaxUDPBytes: 2048, MaxStreams: 8},
		Limits{MaxFrameBytes: 4096, MaxRecordBytes: 4096, MaxWriteBatchBytes: 4096, MaxUDPBytes: 1024, MaxStreams: 4},
	)
	if err != nil || limits.MaxFrameBytes != 4096 || limits.MaxWriteBatchBytes != 4096 || limits.MaxStreams != 4 {
		t.Fatalf("negotiated limits = %#v, err = %v", limits, err)
	}
}

func FuzzReadV5FrameDoesNotPanic(f *testing.F) {
	f.Add(make([]byte, frameHeaderBytes))
	valid := make([]byte, frameHeaderBytes+1)
	valid[0] = Version
	valid[1] = TypePing
	f.Add(valid)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ReadFrame(bytes.NewReader(data), 4096)
	})
}

func FuzzDecodeV5PayloadDoesNotPanic(f *testing.F) {
	f.Add([]byte{7, 'p', 'a', 'y', 'l', 'o', 'a', 'd'})
	f.Fuzz(func(t *testing.T, payload []byte) {
		var data Data
		_ = DecodeMessage(Frame{Version: Version, Type: TypeData, Payload: payload}, &data)
	})
}
