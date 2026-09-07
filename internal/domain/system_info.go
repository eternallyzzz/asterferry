package domain

import (
	"fmt"
	"math"
	"net/netip"
	"strings"
	"time"
)

const (
	maxSystemInfoString      = 512
	maxSystemInfoIPs         = 64
	maxSystemInfoIPLength    = 128
	maxSystemInfoServiceMode = 64
	maxSystemInfoCPUCount    = 4096
)

// Validate keeps host metadata bounded before it is persisted or sent over
// the control channel. The collector is best-effort, but a malformed report
// must never be allowed to grow the Controller database or control message.
func (s *SystemInfo) Validate() error {
	if s == nil {
		return nil
	}
	stringsToCheck := []struct {
		name  string
		value string
		limit int
	}{
		{"hostname", s.Hostname, maxSystemInfoString},
		{"os", s.OS, maxSystemInfoString},
		{"distribution", s.Distribution, maxSystemInfoString},
		{"distribution_version", s.DistributionVersion, maxSystemInfoString},
		{"kernel", s.Kernel, maxSystemInfoString},
		{"architecture", s.Architecture, maxSystemInfoString},
		{"cpu_model", s.CPUModel, maxSystemInfoString},
		{"node_version", s.NodeVersion, maxSystemInfoString},
		{"go_version", s.GoVersion, maxSystemInfoString},
		{"service_mode", s.ServiceMode, maxSystemInfoServiceMode},
	}
	for _, field := range stringsToCheck {
		if len(field.value) > field.limit {
			return &ApplyError{Code: "invalid_system_info", Path: field.name, Message: fmt.Sprintf("system info field exceeds %d bytes", field.limit)}
		}
		if strings.IndexFunc(field.value, func(r rune) bool { return r == '\x00' || r == '\r' || r == '\n' }) >= 0 {
			return &ApplyError{Code: "invalid_system_info", Path: field.name, Message: "system info field contains a control character"}
		}
	}
	if s.LogicalCPUs < 0 || s.LogicalCPUs > maxSystemInfoCPUCount {
		return &ApplyError{Code: "invalid_system_info", Path: "logical_cpus", Message: "logical CPU count is out of range"}
	}
	if len(s.IPAddresses) > maxSystemInfoIPs {
		return &ApplyError{Code: "invalid_system_info", Path: "ip_addresses", Message: "too many IP addresses"}
	}
	seenIPs := make(map[string]struct{}, len(s.IPAddresses))
	for index, value := range s.IPAddresses {
		if len(value) > maxSystemInfoIPLength {
			return &ApplyError{Code: "invalid_system_info", Path: fmt.Sprintf("ip_addresses[%d]", index), Message: "IP address is too long"}
		}
		address, err := netip.ParseAddr(value)
		if err != nil {
			return &ApplyError{Code: "invalid_system_info", Path: fmt.Sprintf("ip_addresses[%d]", index), Message: "IP address is invalid"}
		}
		key := address.Unmap().String()
		if _, exists := seenIPs[key]; exists {
			return &ApplyError{Code: "invalid_system_info", Path: fmt.Sprintf("ip_addresses[%d]", index), Message: "IP address is duplicated"}
		}
		seenIPs[key] = struct{}{}
	}
	if s.CPUUsagePercent != nil && (math.IsNaN(*s.CPUUsagePercent) || math.IsInf(*s.CPUUsagePercent, 0) || *s.CPUUsagePercent < 0 || *s.CPUUsagePercent > 100) {
		return &ApplyError{Code: "invalid_system_info", Path: "cpu_usage_percent", Message: "CPU usage must be between 0 and 100"}
	}
	if s.LoadAverage1m != nil && (math.IsNaN(*s.LoadAverage1m) || math.IsInf(*s.LoadAverage1m, 0) || *s.LoadAverage1m < 0 || *s.LoadAverage1m > 1e9) {
		return &ApplyError{Code: "invalid_system_info", Path: "load_average_1m", Message: "load average is out of range"}
	}
	if s.MemoryAvailableBytes > s.MemoryTotalBytes && s.MemoryTotalBytes != 0 {
		return &ApplyError{Code: "invalid_system_info", Path: "memory_available_bytes", Message: "available memory exceeds total memory"}
	}
	if s.DiskFreeBytes > s.DiskTotalBytes && s.DiskTotalBytes != 0 {
		return &ApplyError{Code: "invalid_system_info", Path: "disk_free_bytes", Message: "free disk space exceeds total disk space"}
	}
	if s.CollectedAt.IsZero() {
		return &ApplyError{Code: "invalid_system_info", Path: "collected_at", Message: "system info collection time is required"}
	}
	if s.CollectedAt.After(time.Now().UTC().Add(5 * time.Minute)) {
		return &ApplyError{Code: "invalid_system_info", Path: "collected_at", Message: "system info collection time is too far in the future"}
	}
	return nil
}
