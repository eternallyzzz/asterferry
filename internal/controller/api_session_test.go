package controller

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneExpiredSessionsRemovesStaleEntries(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	user, err := store.CreateUser(ctx, "session-owner", "a-very-long-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.CreateWebSession(ctx, user.ID, "expired", "expired-csrf", now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWebSession(ctx, user.ID, "active", "active-csrf", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneHistory(ctx, now, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetWebSession(ctx, "expired"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired session lookup = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.GetWebSession(ctx, "active"); err != nil {
		t.Fatalf("active session lookup failed: %v", err)
	}
}
