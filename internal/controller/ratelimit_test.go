package controller

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginLimiterBlocksBothDimensionsAndExpires(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter()
	limiter.now = func() time.Time { return now }
	req := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	keys := loginKeys(req, "Admin")
	for i := 0; i < loginFailureLimit; i++ {
		limiter.failure(keys...)
	}
	if allowed, retry := limiter.allow(keys...); allowed || retry <= 0 {
		t.Fatalf("blocked login allowed=%v retry=%s", allowed, retry)
	}
	// Either dimension is sufficient to block a request.
	if allowed, _ := limiter.allow("user:admin"); allowed {
		t.Fatal("username bucket did not block")
	}
	now = now.Add(loginBlock)
	if allowed, _ := limiter.allow(keys...); !allowed {
		t.Fatal("expired block did not clear")
	}
}

func TestLoginLimiterUsesDirectPeerOnly(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "[2001:db8::1]:443"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	keys := loginKeys(req, "operator")
	if len(keys) != 2 || keys[0] != "ip:2001:db8::1" {
		t.Fatalf("unexpected login keys: %#v", keys)
	}
}

func TestLoginLimiterCapsEntries(t *testing.T) {
	limiter := newLoginLimiter()
	limiter.maxEntries = 2
	limiter.failure("ip:one")
	limiter.failure("ip:two")
	limiter.failure("ip:three")
	if len(limiter.entries) != 2 {
		t.Fatalf("entry cap ignored: %d", len(limiter.entries))
	}
}
