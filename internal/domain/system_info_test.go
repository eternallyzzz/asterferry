package domain

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestSystemInfoValidateAcceptsHostSnapshot(t *testing.T) {
	usage := 37.5
	load := 1.25
	info := SystemInfo{
		Hostname: "edge-01", OS: "linux", Distribution: "Debian GNU/Linux",
		DistributionVersion: "12", Kernel: "6.1", Architecture: "amd64",
		CPUModel: "test CPU", LogicalCPUs: 4, NodeVersion: "1.0.0",
		GoVersion: "go1.26.7", ServiceMode: "systemd", IPAddresses: []string{"192.0.2.10", "2001:db8::10"},
		UptimeSeconds: 12, CPUUsagePercent: &usage, LoadAverage1m: &load,
		MemoryTotalBytes: 1024, MemoryAvailableBytes: 512, DiskTotalBytes: 2048,
		DiskFreeBytes: 1024, CollectedAt: time.Now().UTC(),
	}
	if err := info.Validate(); err != nil {
		t.Fatalf("valid system info rejected: %v", err)
	}
}

func TestSystemInfoValidateRejectsUnsafeOrInconsistentValues(t *testing.T) {
	base := SystemInfo{Hostname: "edge", CollectedAt: time.Now().UTC(), MemoryTotalBytes: 1, MemoryAvailableBytes: 2}
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "available memory") {
		t.Fatalf("inconsistent memory accepted: %v", err)
	}
	base.MemoryAvailableBytes = 0
	base.Hostname = "edge\nforged-log"
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("unsafe hostname accepted: %v", err)
	}
	base.Hostname = "edge"
	base.IPAddresses = []string{"192.0.2.1", "192.0.2.1"}
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate address accepted: %v", err)
	}
	base.IPAddresses = nil
	usage := math.NaN()
	base.CPUUsagePercent = &usage
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "CPU usage") {
		t.Fatalf("NaN CPU usage accepted: %v", err)
	}
	base.CPUUsagePercent = nil
	load := math.Inf(1)
	base.LoadAverage1m = &load
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "load average") {
		t.Fatalf("infinite load average accepted: %v", err)
	}
}

func TestObservedStateValidatesSystemInfo(t *testing.T) {
	state := ObservedState{SchemaVersion: CurrentControlProtocolVersion, NodeID: "node-1", Healthy: true, SystemInfo: &SystemInfo{CollectedAt: time.Now().UTC()}}
	if err := state.Validate(); err != nil {
		t.Fatalf("observed state with system info rejected: %v", err)
	}
}
