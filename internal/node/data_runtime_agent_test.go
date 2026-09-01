package node

import (
	"errors"
	"testing"
	"time"
)

func TestAgentReconnectBackoffOnlyResetsAfterAdmittedSession(t *testing.T) {
	current := 30 * time.Second
	if got := agentReconnectBackoffAfterAttempt(current, nil, errors.New("session limit")); got != current {
		t.Fatalf("admission rejection reset backoff to %s, want %s", got, current)
	}
	if got := agentReconnectBackoffAfterAttempt(current, errors.New("handshake failed"), nil); got != current {
		t.Fatalf("handshake failure reset backoff to %s, want %s", got, current)
	}
	if got := agentReconnectBackoffAfterAttempt(current, nil, nil); got != time.Second {
		t.Fatalf("successful session backoff = %s, want %s", got, time.Second)
	}
}
