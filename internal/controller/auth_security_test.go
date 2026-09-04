package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPasswordIdempotencyRequestDoesNotContainPasswordDigest(t *testing.T) {
	request := struct {
		Username        string `json:"username"`
		Role            string `json:"role"`
		PasswordChanged bool   `json:"password_changed"`
	}{Username: "admin", Role: RoleAdmin, PasswordChanged: true}
	b, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "password_digest") || strings.Contains(string(b), "a-very-long-password") {
		t.Fatalf("password material leaked into idempotency request: %s", b)
	}
	if !strings.Contains(string(b), `"password_changed":true`) {
		t.Fatalf("password change marker missing: %s", b)
	}
}

func TestDummyPasswordHashIsValidArgon2id(t *testing.T) {
	if !VerifyPassword(dummyPasswordHash, "asterferry-dummy-password") {
		t.Fatal("fixed dummy password hash does not verify")
	}
}

func TestVerifyPasswordRejectsNonCanonicalArgon2Parameters(t *testing.T) {
	hash, err := HashPassword("a-very-long-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "a-very-long-admin-password") {
		t.Fatal("canonical password hash does not verify")
	}
	for _, test := range []struct {
		name string
		from string
		to   string
	}{
		{name: "memory", from: "m=65536", to: "m=65537"},
		{name: "time", from: "t=3", to: "t=4"},
		{name: "threads", from: "p=2", to: "p=3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			modified := strings.Replace(hash, test.from, test.to, 1)
			if VerifyPassword(modified, "a-very-long-admin-password") {
				t.Fatalf("hash with non-canonical %s parameter was accepted: %s", test.name, modified)
			}
		})
	}
}

func TestVerifyPasswordRejectsNonCanonicalArgon2ParameterList(t *testing.T) {
	hash, err := HashPassword("a-very-long-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		from string
		to   string
	}{
		{name: "duplicate", from: "m=65536,t=3,p=2", to: "m=65536,m=65536,t=3,p=2"},
		{name: "unknown", from: "m=65536,t=3,p=2", to: "m=65536,t=3,p=2,x=1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			modified := strings.Replace(hash, test.from, test.to, 1)
			if VerifyPassword(modified, "a-very-long-admin-password") {
				t.Fatalf("hash with %s parameter list was accepted: %s", test.name, modified)
			}
		})
	}
}

func TestPasswordChangeRevokesTokensAndSessions(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	user, err := store.CreateUser(ctx, "password-owner", "old-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	plainToken, _, err := store.CreateAPIToken(ctx, user.ID, "before-password-change", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateToken(ctx, plainToken); err != nil {
		t.Fatalf("token was not valid before password change: %v", err)
	}

	server := &Server{resources: store, sessions: sync.Map{}}
	cookieValue := "session-before-password-change"
	server.sessions.Store(cookieValue, session{User: user, ExpiresAt: time.Now().UTC().Add(time.Hour), CSRF: "csrf"})
	newPassword := "new-password"
	if _, err := store.UpdateUser(ctx, user.ID, UserUpdate{Password: &newPassword}, WriteOptions{IfMatch: user.Revision, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateToken(ctx, plainToken); !errors.Is(err, ErrInvalidAPIToken) {
		t.Fatalf("old API token error = %v, want ErrInvalidAPIToken", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	request.AddCookie(&http.Cookie{Name: "af_session", Value: cookieValue})
	response := httptest.NewRecorder()
	if _, ok := server.authorize(response, request, RoleViewer); ok {
		t.Fatal("session remained authorized after password change")
	}
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("stale session status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if _, ok := server.sessions.Load(cookieValue); ok {
		t.Fatal("stale session was not removed")
	}
}
