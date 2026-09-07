package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"asterferry/internal/domain"
)

func TestObfuscationKeysAreEncryptedAtRestAndDecryptedForWire(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateNode(ctx, domain.Node{ID: "gw", Name: "gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	current := []byte("01234567890123456789012345678901")
	previous := []byte("abcdefghijklmnopqrstuvwxyz123456")
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gw", PublicEndpoints: []string{"gw.example:4433"}, Obfuscation: domain.ObfuscationPolicy{Mode: "camouflage", Key: current, PreviousKey: previous}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	var persisted, persistedPrevious []byte
	var keyID, previousKeyID string
	if err := store.db.QueryRow(`SELECT obfuscation_key_ciphertext,obfuscation_previous_key_ciphertext,obfuscation_key_id,obfuscation_previous_key_id FROM gateway_specs WHERE node_id='gw'`).Scan(&persisted, &persistedPrevious, &keyID, &previousKeyID); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), string(current)) || strings.Contains(string(persistedPrevious), string(previous)) || keyID == "" || previousKeyID == "" {
		t.Fatalf("plaintext or incomplete data-plane key was persisted: current=%x previous=%x key_id=%q previous_key_id=%q", persisted, persistedPrevious, keyID, previousKeyID)
	}

	snapshot, err := store.EnsureDesiredSnapshot(ctx, "gw")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapshot.Document), base64.StdEncoding.EncodeToString(current)) || strings.Contains(string(snapshot.Document), base64.StdEncoding.EncodeToString(previous)) || strings.Contains(string(snapshot.Document), `"key":`) || strings.Contains(string(snapshot.Document), `"previous_key":`) {
		t.Fatal("plaintext data-plane key was persisted in the desired snapshot")
	}
	wire, err := store.SnapshotDocumentForWire(snapshot.Document)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), base64.StdEncoding.EncodeToString(current)) || !strings.Contains(string(wire), base64.StdEncoding.EncodeToString(previous)) || strings.Contains(string(wire), `"key_ciphertext":`) {
		t.Fatalf("wire snapshot did not contain the expected transient key fields: %s", wire)
	}
}

func TestLegacyAssignmentObfuscationChecksumIsCanonicalized(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, node := range []domain.Node{
		{ID: "gw", Name: "gateway", Enabled: true},
		{ID: "agent", Name: "agent", Enabled: true},
	} {
		if err := store.CreateNode(ctx, node, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gw", PublicEndpoints: []string{"gw.example:4433"}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSpec(ctx, domain.AgentSpec{NodeID: "agent"}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutService(ctx, domain.Service{ID: "svc", AgentID: "agent", Protocol: domain.ProtocolTCP, LocalTarget: "127.0.0.1:8080", PublicBind: "0.0.0.0", PublicPort: 18080}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	key := []byte("01234567890123456789012345678901")
	if err := store.PutAssignment(ctx, domain.Assignment{
		ID:             "assignment",
		GatewayID:      "gw",
		AgentID:        "agent",
		PublicEndpoint: "gw.example:4433",
		ServiceIDs:     []string{"svc"},
		Bindings:       []domain.Binding{{ServiceID: "svc", Protocol: domain.ProtocolTCP, Bind: "0.0.0.0", Port: 18080}},
		Generation:     1,
		Obfuscation:    domain.ObfuscationPolicy{Mode: "camouflage", Key: key},
	}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	// Reproduce the v3 form: the protected ciphertext survived migration but
	// the plaintext-derived key identifier was not written into the document.
	var persistedCiphertext []byte
	var persistedKeyID string
	if err := store.db.QueryRow(`SELECT obfuscation_key_ciphertext,obfuscation_key_id FROM assignments WHERE id='assignment'`).Scan(&persistedCiphertext, &persistedKeyID); err != nil {
		t.Fatal(err)
	}
	if persistedKeyID == "" || len(persistedCiphertext) == 0 {
		t.Fatalf("test assignment was not stored in protected form: ciphertext=%x key_id=%q", persistedCiphertext, persistedKeyID)
	}
	if _, err := store.db.Exec(`UPDATE assignments SET obfuscation_key_id='' WHERE id='assignment'`); err != nil {
		t.Fatal(err)
	}

	record, err := store.EnsureDesiredSnapshot(ctx, "gw")
	if err != nil {
		t.Fatal(err)
	}
	var persisted domain.DesiredSnapshot
	if err := json.Unmarshal(record.Document, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Assignments) != 1 || persisted.Assignments[0].Obfuscation.KeyID == "" {
		t.Fatalf("canonical persisted snapshot did not restore assignment key id: %#v", persisted.Assignments)
	}
	wire, err := store.SnapshotDocumentForWire(record.Document)
	if err != nil {
		t.Fatal(err)
	}
	var wireSnapshot domain.DesiredSnapshot
	if err := json.Unmarshal(wire, &wireSnapshot); err != nil {
		t.Fatal(err)
	}
	wireChecksum, err := wireSnapshot.ComputeChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(wireChecksum, record.Checksum) {
		t.Fatalf("wire checksum %q differs from persisted checksum %q", wireChecksum, record.Checksum)
	}
}
