package node

import (
	"testing"
	"time"
)

func TestControllerReconnectDelayIsBounded(t *testing.T) {
	for _, base := range []time.Duration{time.Second, 2 * time.Second, controllerReconnectMaxBackoff} {
		for attempt := 0; attempt < 100; attempt++ {
			got := jitteredControllerReconnectDelay(base)
			if got < base*4/5 || got > base*6/5 {
				t.Fatalf("base=%s attempt=%d delay=%s is outside +/-20%% bound", base, attempt, got)
			}
		}
	}
	if got := jitteredControllerReconnectDelay(0); got != 0 {
		t.Fatalf("zero backoff delay = %s, want 0", got)
	}
}
