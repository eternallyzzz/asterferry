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
	if err := store.CreateNode(ctx, domain.Node{ID: "gw", Role: domain.RoleGateway, Name: "gateway", Enabled: true}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	current := []byte("01234567890123456789012345678901")
	previous := []byte("abcdefghijklmnopqrstuvwxyz123456")
	if err := store.PutGatewaySpec(ctx, domain.GatewaySpec{NodeID: "gw", PublicEndpoints: []string{"gw.example:4433"}, Obfuscation: domain.ObfuscationPolicy{Mode: "camouflage", Key: current, PreviousKey: previous}}, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	var persisted []byte
	if err := store.DB().QueryRow(`SELECT document_json FROM gateway_specs WHERE node_id='gw'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), string(current)) || strings.Contains(string(persisted), string(previous)) || strings.Contains(string(persisted), `"key":`) || strings.Contains(string(persisted), `"previous_key":`) {
		t.Fatalf("plaintext data-plane key was persisted: %s", persisted)
	}
	var stored domain.GatewaySpec
	if err := json.Unmarshal(persisted, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Obfuscation.KeyCiphertext) == 0 || len(stored.Obfuscation.PreviousKeyCiphertext) == 0 || stored.Obfuscation.KeyID == "" || stored.Obfuscation.PreviousKeyID == "" {
		t.Fatalf("at-rest key policy is incomplete: %#v", stored.Obfuscation)
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
