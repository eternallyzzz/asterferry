package node

import (
	"asterferry/internal/domain"
	"time"
)

func (r *Runtime) refreshSystemInfo(force bool) {
	if r == nil || r.systemInfoCollector == nil {
		return
	}
	now := time.Now().UTC()
	r.systemInfoMu.Lock()
	defer r.systemInfoMu.Unlock()
	if !force && r.systemInfo != nil && now.Sub(r.systemInfoAt) < r.runtimeOpts.SystemInfoInterval {
		return
	}
	info := r.systemInfoCollector.Collect()
	if err := info.Validate(); err != nil {
		r.logger.Warn("system information report rejected locally", "error", err)
		return
	}
	r.systemInfo = &info
	r.systemInfoAt = info.CollectedAt
}

func (r *Runtime) currentSystemInfo() *domain.SystemInfo {
	if r == nil {
		return nil
	}
	r.systemInfoMu.RLock()
	defer r.systemInfoMu.RUnlock()
	if r.systemInfo == nil {
		return nil
	}
	copy := *r.systemInfo
	copy.IPAddresses = append([]string(nil), r.systemInfo.IPAddresses...)
	if r.systemInfo.CPUUsagePercent != nil {
		value := *r.systemInfo.CPUUsagePercent
		copy.CPUUsagePercent = &value
	}
	if r.systemInfo.LoadAverage1m != nil {
		value := *r.systemInfo.LoadAverage1m
		copy.LoadAverage1m = &value
	}
	return &copy
}
