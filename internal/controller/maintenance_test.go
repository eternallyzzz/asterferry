package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneHistoryHonorsRetentionAndZeroDisable(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	recent := now.Add(-30 * time.Minute).Format(time.RFC3339Nano)
	for _, row := range []struct {
		key, created string
	}{
		{"old-idempotency", old},
		{"recent-idempotency", recent},
	} {
		if _, err := store.db.Exec(`INSERT INTO idempotency_keys(key,request_hash,response_json,created_at) VALUES(?,?,?,?)`, row.key, "hash", []byte(`{}`), row.created); err != nil {
			t.Fatal(err)
		}
	}
	for _, created := range []string{old, recent} {
		if _, err := store.db.Exec(`INSERT INTO audit_events(actor,action,resource,resource_id,revision,attributes_json,created_at) VALUES(?,?,?,?,?,?,?)`, "test", "event", "test", "resource", 1, []byte(`{}`), created); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.PruneHistory(ctx, now, time.Hour, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var idempotency, audit int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM idempotency_keys`).Scan(&idempotency); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if idempotency != 1 || audit != 1 {
		t.Fatalf("retained history counts = idempotency:%d audit:%d, want 1/1", idempotency, audit)
	}

	if _, err := store.db.Exec(`INSERT INTO idempotency_keys(key,request_hash,response_json,created_at) VALUES(?,?,?,?)`, "disabled-cleanup", "hash", []byte(`{}`), old); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneHistory(ctx, now, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM idempotency_keys WHERE key='disabled-cleanup'`).Scan(&idempotency); err != nil {
		t.Fatal(err)
	}
	if idempotency != 1 {
		t.Fatal("explicit zero retention did not disable idempotency cleanup")
	}
}
