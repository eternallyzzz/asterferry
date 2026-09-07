package controller

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHighAvailabilityRejectsSQLite(t *testing.T) {
	config := DefaultConfig(t.TempDir())
	config.HighAvailability = true
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "high_availability requires postgres") {
		t.Fatalf("SQLite HA validation error = %v", err)
	}
}

func TestPostgresLeadershipLeaseEpochAndFencing(t *testing.T) {
	storeA, storeB := openTwoPostgresTestStores(t)
	ctx := context.Background()
	leaderA, err := newLeadership(storeA.databaseHandle, true, newControllerMetrics(DatabaseDriverPostgres))
	if err != nil {
		t.Fatal(err)
	}
	leaderB, err := newLeadership(storeB.databaseHandle, true, newControllerMetrics(DatabaseDriverPostgres))
	if err != nil {
		t.Fatal(err)
	}

	epochA, err := leaderA.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	leaderA.setLeader(epochA)
	if _, err := leaderB.acquire(ctx); !errors.Is(err, ErrControllerNotLeader) {
		t.Fatalf("standby acquisition error = %v, want ErrControllerNotLeader", err)
	}

	leaderA.release(ctx)
	epochB, err := leaderB.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epochB <= epochA {
		t.Fatalf("fencing epoch did not advance: first=%d second=%d", epochA, epochB)
	}
	leaderB.setLeader(epochB)

	// Simulate a stale process that has not yet observed the lease loss. The
	// final row lock must reject its transaction even though its local gate
	// still says leader.
	leaderA.setLeader(epochA)
	storeA.leadership = leaderA
	tx, err := storeA.beginWriteTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_settings(key,value,updated_at) VALUES(?,?,?)`, "stale-write", "must-not-commit", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := storeA.commitWriteTx(ctx, tx); !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("stale fenced commit error = %v, want ErrLeadershipLost", err)
	}
	var count int
	if err := storeB.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_settings WHERE key=?`, "stale-write").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale fenced write committed %d rows", count)
	}
	leaderB.release(ctx)
}

func TestPostgresWebSessionsAreSharedAndRevocable(t *testing.T) {
	storeA, storeB := openTwoPostgresTestStores(t)
	ctx := context.Background()
	user, err := storeA.CreateUser(ctx, "shared-session-user", "a-very-long-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	if err := storeA.CreateWebSession(ctx, user.ID, "shared-session", "shared-csrf", expires); err != nil {
		t.Fatal(err)
	}
	if _, err := storeB.GetWebSession(ctx, "shared-session"); err != nil {
		t.Fatalf("session was not visible through the second repository: %v", err)
	}
	if err := storeB.RevokeWebSession(ctx, "shared-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.GetWebSession(ctx, "shared-session"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked session lookup = %v, want sql.ErrNoRows", err)
	}
}

func TestPostgresControllerFailoverGatesReadiness(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("ASTERFERRY_TEST_POSTGRES_URL"))
	if baseURL == "" {
		t.Skip("ASTERFERRY_TEST_POSTGRES_URL is not set")
	}
	_, databaseURL := createPostgresTestSchema(t, baseURL)
	result, err := Init(context.Background(), InitOptions{
		Dir:        filepath.Join(t.TempDir(), "controller"),
		HTTPListen: "127.0.0.1:0", MetricsListen: "", MetricsListenSet: true,
		GRPCListen: "127.0.0.1:0", GRPCAdvertise: "127.0.0.1:9443",
		DatabaseDriver: DatabaseDriverPostgres, DatabaseURL: databaseURL,
		HighAvailability: true, Password: "a-very-long-admin-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := New(result.Config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(result.Config)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := first.Start(ctx); err != nil {
		_ = second.Close()
		t.Fatal(err)
	}
	if err := second.Start(ctx); err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	defer first.Close()
	defer second.Close()

	waitForLeadership := func() *Controller {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			firstLeader, secondLeader := first.leadership.IsLeader(), second.leadership.IsLeader()
			if firstLeader != secondLeader {
				if firstLeader {
					return first
				}
				return second
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the two Controllers did not converge to one leader")
		return nil
	}
	leader := waitForLeadership()
	standby := second
	if leader == second {
		standby = first
	}
	assertReady := func(instance *Controller, want int) {
		t.Helper()
		recorder := httptest.NewRecorder()
		instance.HTTP.readyz(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if recorder.Code != want {
			t.Fatalf("Controller readiness status = %d, want %d; body=%s", recorder.Code, want, recorder.Body.String())
		}
	}
	assertReady(leader, http.StatusOK)
	assertReady(standby, http.StatusServiceUnavailable)
	standbyBusiness := httptest.NewRecorder()
	standby.HTTP.Handler().ServeHTTP(standbyBusiness, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if standbyBusiness.Code != http.StatusServiceUnavailable || !strings.Contains(standbyBusiness.Body.String(), "controller_standby") {
		t.Fatalf("standby business endpoint status=%d body=%s", standbyBusiness.Code, standbyBusiness.Body.String())
	}

	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !standby.leadership.IsLeader() {
		time.Sleep(20 * time.Millisecond)
	}
	if !standby.leadership.IsLeader() {
		t.Fatal("standby did not take over after leader shutdown")
	}
	assertReady(standby, http.StatusOK)
}
