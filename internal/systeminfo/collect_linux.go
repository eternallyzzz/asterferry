//go:build linux

package systeminfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func collectPlatform(diskPath string) (platformSnapshot, error) {
	result := platformSnapshot{}
	if release, err := os.ReadFile("/etc/os-release"); err == nil {
		values := parseKeyValueFile(string(release))
		result.distribution = values["PRETTY_NAME"]
		if result.distribution == "" {
			result.distribution = values["NAME"]
		}
		result.distributionVersion = values["VERSION_ID"]
	}
	if kernel, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		result.kernel = strings.TrimSpace(string(kernel))
	}
	if cpuInfo, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(cpuInfo), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if ok && (strings.TrimSpace(key) == "model name" || strings.TrimSpace(key) == "Hardware") {
				result.cpuModel = strings.TrimSpace(value)
				break
			}
		}
	}
	if memory, err := os.ReadFile("/proc/meminfo"); err == nil {
		values := parseMeminfo(string(memory))
		result.memoryTotalBytes = values["MemTotal"]
		result.memoryAvailable = values["MemAvailable"]
		if result.memoryAvailable == 0 {
			result.memoryAvailable = values["MemFree"]
		}
	}
	if uptime, err := os.ReadFile("/proc/uptime"); err == nil {
		if fields := strings.Fields(string(uptime)); len(fields) > 0 {
			if seconds, parseErr := strconv.ParseFloat(fields[0], 64); parseErr == nil && seconds >= 0 {
				result.uptimeSeconds = uint64(seconds)
			}
		}
	}
	if load, err := os.ReadFile("/proc/loadavg"); err == nil {
		if values := strings.Fields(string(load)); len(values) > 0 {
			if one, parseErr := strconv.ParseFloat(values[0], 64); parseErr == nil && one >= 0 {
				result.loadAverage1m = &one
			}
		}
	}
	if cpu, err := os.ReadFile("/proc/stat"); err == nil {
		result.cpu, result.cpuValid = parseLinuxCPU(string(cpu))
	}
	if diskPath == "" {
		diskPath = "/"
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(diskPath, &stats); err == nil {
		blockSize := uint64(stats.Bsize)
		result.diskTotalBytes = uint64(stats.Blocks) * blockSize
		result.diskFreeBytes = uint64(stats.Bavail) * blockSize
	}
	return result, nil
}

func parseKeyValueFile(content string) map[string]string {
	values := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "\"")
		values[strings.TrimSpace(key)] = value
	}
	return values
}

func parseMeminfo(content string) map[string]uint64 {
	values := make(map[string]uint64)
	for _, line := range strings.Split(content, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		parsed, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kb") {
			if parsed > ^uint64(0)/1024 {
				continue
			}
			parsed *= 1024
		}
		values[strings.TrimSpace(key)] = parsed
	}
	return values
}

func parseLinuxCPU(content string) (cpuSample, bool) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var values []uint64
		for _, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpuSample{}, false
			}
			values = append(values, value)
		}
		var total uint64
		for _, value := range values {
			total += value
		}
		return cpuSample{idle: values[3], total: total}, true
	}
	return cpuSample{}, false
}
