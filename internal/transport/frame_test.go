package transport

import (
	"bytes"
	"testing"
)

func TestFrameRoundTripAndBounds(t *testing.T) {
	want := Frame{Type: TypeData, RequestID: 42, Payload: []byte("payload")}
	var b bytes.Buffer
	if err := WriteFrame(&b, want, 1024); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&b, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != Version || got.Type != want.Type || got.RequestID != want.RequestID || string(got.Payload) != string(want.Payload) {
		t.Fatalf("round trip mismatch: %#v", got)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 1}), 1024); err == nil {
		t.Fatal("expected invalid length")
	}
	if err := WriteFrame(&b, Frame{Type: TypeData, Payload: make([]byte, 2048)}, 1024); err == nil {
		t.Fatal("expected write bound error")
	}
}

func TestChallengeSignature(t *testing.T) {
	token := []byte("a-very-long-test-token")
	nonce := []byte("nonce")
	mac := SignChallenge(token, nonce, "edge-a")
	if !VerifyChallenge(token, nonce, mac, "edge-a") {
		t.Fatal("signature should verify")
	}
	if VerifyChallenge(token, nonce, mac, "edge-b") {
		t.Fatal("signature should bind client ID")
	}
	if VerifyChallenge([]byte("different-token"), nonce, mac, "edge-a") {
		t.Fatal("signature should bind token")
	}
}

func TestDataPaddingAndBounds(t *testing.T) {
	f, err := JSONFrame(TypeData, 0, NewData([]byte("payload"), "balanced", 64))
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

func FuzzReadFrameDoesNotPanic(f *testing.F) {
	f.Add([]byte{0, 0, 0, 16, 2, TypePing, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ReadFrame(bytes.NewReader(data), 4096)
	})
}
