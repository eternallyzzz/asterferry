package observability

import (
	"testing"
	"time"
)

func TestAuthGuardLimitsAndResets(t *testing.T) {
	now := time.Unix(100, 0)
	guard := newAuthGuard(func() time.Time { return now })
	for attempt := 0; attempt < managementAuthFailureLimit; attempt++ {
		limited, retryAfter := guard.recordFailure()
		if limited || retryAfter != 0 {
			t.Fatalf("failure %d was limited: limited=%v retry=%s", attempt+1, limited, retryAfter)
		}
	}
	limited, retryAfter := guard.recordFailure()
	if !limited || retryAfter <= 0 {
		t.Fatalf("failure after limit was not rate limited: limited=%v retry=%s", limited, retryAfter)
	}
	if blocked, _ := guard.blocked(); !blocked {
		t.Fatal("guard should remain blocked during cooldown")
	}

	now = now.Add(managementAuthCooldown)
	if blocked, _ := guard.blocked(); blocked {
		t.Fatal("guard should unblock after cooldown")
	}
	if limited, _ := guard.recordFailure(); limited {
		t.Fatal("new failure window should allow a request")
	}
	guard.reset()
	if limited, _ := guard.recordFailure(); limited {
		t.Fatal("reset guard should allow a request")
	}
}
