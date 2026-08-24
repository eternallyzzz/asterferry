package gateway

import (
	"fmt"
	"testing"
	"time"
)

func TestHandshakeAdmissionBoundsPendingAndBackoff(t *testing.T) {
	now := time.Now()
	admission := newHandshakeAdmission(1)
	admission.now = func() time.Time { return now }
	release, _, ok := admission.begin(nil)
	if !ok {
		t.Fatal("first handshake should be admitted")
	}
	if _, _, ok := admission.begin(nil); ok {
		t.Fatal("pending handshake limit was bypassed")
	}
	release()
	for i := 0; i < 5; i++ {
		admission.failure("source")
	}
	if admission.allow("source") {
		t.Fatal("source should be in authentication backoff")
	}
	now = now.Add(31 * time.Second)
	if !admission.allow("source") {
		t.Fatal("source backoff did not expire")
	}
}

func TestHandshakeAdmissionTrimsStaleAndExcessBuckets(t *testing.T) {
	now := time.Now()
	admission := newHandshakeAdmission(1)
	admission.now = func() time.Time { return now }
	for i := 0; i < maxAuthBuckets; i++ {
		admission.buckets[fmt.Sprintf("stale-%d", i)] = authBucket{
			windowStart: now.Add(-2 * authFailureWindow),
			lastSeen:    now.Add(-2 * authFailureWindow),
		}
	}
	admission.trimLocked(now)
	if len(admission.buckets) != 0 {
		t.Fatalf("stale authentication buckets retained: %d", len(admission.buckets))
	}

	for i := 0; i < maxAuthBuckets; i++ {
		admission.buckets[fmt.Sprintf("active-%d", i)] = authBucket{lastSeen: now}
	}
	admission.trimLocked(now)
	if len(admission.buckets) != maxAuthBuckets-1 {
		t.Fatalf("excess authentication buckets = %d, want %d", len(admission.buckets), maxAuthBuckets-1)
	}
}
