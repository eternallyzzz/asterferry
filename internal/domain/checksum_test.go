package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func checksumSnapshotWithObfuscation(value ObfuscationPolicy) DesiredSnapshot {
	return DesiredSnapshot{
		SchemaVersion: SchemaVersion,
		NodeID:        "gateway-1",
		Generation:    7,
		Gateway: &GatewaySpec{
			NodeID:          "gateway-1",
			PublicEndpoints: []string{"gw.example:4433"},
			Obfuscation:     value,
		},
	}
}

func TestChecksumRequiresCanonicalObfuscationKeyID(t *testing.T) {
	tests := []struct {
		name     string
		policy   ObfuscationPolicy
		contains string
	}{
		{
			name:     "current key",
			policy:   ObfuscationPolicy{Mode: "camouflage", Key: []byte("wire-key")},
			contains: "current",
		},
		{
			name:     "current ciphertext",
			policy:   ObfuscationPolicy{Mode: "camouflage", KeyCiphertext: []byte("sealed-key")},
			contains: "current",
		},
		{
			name:     "previous key",
			policy:   ObfuscationPolicy{Mode: "camouflage", KeyID: "current-id", PreviousKey: []byte("old-wire-key")},
			contains: "previous",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := checksumSnapshotWithObfuscation(test.policy).ComputeChecksum()
			if err == nil || !errors.Is(err, ErrMissingObfuscationKeyID) {
				t.Fatalf("ComputeChecksum() error = %v, want ErrMissingObfuscationKeyID (%s)", err, test.contains)
			}
		})
	}
}

func TestChecksumIsIndependentOfObfuscationStorageRepresentation(t *testing.T) {
	atRest := checksumSnapshotWithObfuscation(ObfuscationPolicy{
		Mode:                  "camouflage",
		KeyCiphertext:         []byte("sealed-current-key"),
		PreviousKeyCiphertext: []byte("sealed-previous-key"),
		KeyID:                 "current-id",
		PreviousKeyID:         "previous-id",
		MaxPaddingBytes:       128,
		HandshakeShaping:      true,
	})
	wire := checksumSnapshotWithObfuscation(ObfuscationPolicy{
		Mode:             "camouflage",
		Key:              []byte("wire-current-key"),
		PreviousKey:      []byte("wire-previous-key"),
		KeyID:            "current-id",
		PreviousKeyID:    "previous-id",
		MaxPaddingBytes:  128,
		HandshakeShaping: true,
	})

	left, err := atRest.ComputeChecksum()
	if err != nil {
		t.Fatal(err)
	}
	right, err := wire.ComputeChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("storage representation changed checksum: %s != %s", left, right)
	}
}

func TestChecksumDocumentDoesNotContainObfuscationMaterial(t *testing.T) {
	document, err := checksumSnapshotWithObfuscation(ObfuscationPolicy{
		Mode:                  "camouflage",
		KeyCiphertext:         []byte("sealed-current-key"),
		PreviousKeyCiphertext: []byte("sealed-previous-key"),
		KeyID:                 "current-id",
		PreviousKeyID:         "previous-id",
	}).ChecksumDocument()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		[]byte("sealed-current-key"),
		[]byte("sealed-previous-key"),
		[]byte("key_ciphertext"),
		[]byte("previous_key_ciphertext"),
		[]byte(`"key":`),
		[]byte(`"previous_key":`),
	} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatalf("checksum document contains forbidden material %q: %s", forbidden, encoded)
		}
	}
	for _, expected := range []string{`"key_id":"current-id"`, `"previous_key_id":"previous-id"`} {
		if !bytes.Contains(encoded, []byte(expected)) {
			t.Fatalf("checksum document omitted canonical identity %q: %s", expected, encoded)
		}
	}
}
