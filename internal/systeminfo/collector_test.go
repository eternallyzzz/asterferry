package systeminfo

import (
	"runtime"
	"testing"
)

func TestCollectorReturnsBoundedValidHostSnapshot(t *testing.T) {
	collector := New(Options{ServiceMode: "test"})
	first := collector.Collect()
	if first.OS != runtime.GOOS || first.Architecture != runtime.GOARCH {
		t.Fatalf("platform identity = %#v", first)
	}
	if first.CollectedAt.IsZero() || first.LogicalCPUs <= 0 || first.ServiceMode != "test" {
		t.Fatalf("incomplete host snapshot = %#v", first)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("collector returned invalid snapshot: %v", err)
	}
	second := collector.Collect()
	if err := second.Validate(); err != nil {
		t.Fatalf("second collector result invalid: %v", err)
	}
	if second.CollectedAt.Before(first.CollectedAt) {
		t.Fatal("collector timestamp moved backwards")
	}
}
