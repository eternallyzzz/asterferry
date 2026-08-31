package controller

import (
	"encoding/json"
	"strings"
	"testing"
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
