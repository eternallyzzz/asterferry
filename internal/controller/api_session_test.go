package controller

import (
	"testing"
	"time"
)

func TestPruneExpiredSessionsRemovesStaleEntries(t *testing.T) {
	server := &Server{}
	now := time.Unix(100, 0)
	server.sessions.Store("expired", session{ExpiresAt: now.Add(-time.Second)})
	server.sessions.Store("active", session{ExpiresAt: now.Add(time.Second)})
	server.sessions.Store("zero", "invalid session value")
	server.pruneExpiredSessions(now)
	if _, ok := server.sessions.Load("expired"); ok {
		t.Fatal("expired session was not reaped")
	}
	if _, ok := server.sessions.Load("active"); !ok {
		t.Fatal("active session was reaped")
	}
	if _, ok := server.sessions.Load("zero"); ok {
		t.Fatal("invalid session entry was not reaped")
	}
}
