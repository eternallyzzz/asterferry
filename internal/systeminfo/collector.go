// Package systeminfo collects bounded host metadata for the Node observed
// state. Collection is best-effort: an unavailable OS-specific metric is left
// empty while the node continues to serve its control and data planes.
package systeminfo

import (
	"net"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"asterferry/internal/buildinfo"
	"asterferry/internal/domain"
)

type Options struct {
	ServiceMode string
	DiskPath    string
	NodeVersion string
	GoVersion   string
}

type cpuSample struct {
	idle  uint64
	total uint64
}

type platformSnapshot struct {
	distribution        string
	distributionVersion string
	kernel              string
	cpuModel            string
	uptimeSeconds       uint64
	memoryTotalBytes    uint64
	memoryAvailable     uint64
	diskTotalBytes      uint64
	diskFreeBytes       uint64
	loadAverage1m       *float64
	cpu                 cpuSample
	cpuValid            bool
}

// Collector keeps the previous CPU counters so that CPU usage can be
// calculated on the second and subsequent reports without spawning a helper
// process or retaining any sensitive host data.
type Collector struct {
	options Options
	mu      sync.Mutex
	prev    cpuSample
	prevAt  time.Time
}

func New(options Options) *Collector {
	if strings.TrimSpace(options.ServiceMode) == "" {
		options.ServiceMode = "foreground"
	}
	return &Collector{options: options}
}

func (c *Collector) Collect() domain.SystemInfo {
	if c == nil {
		return domain.SystemInfo{}
	}
	now := time.Now().UTC()
	version := buildinfo.Current()
	nodeVersion := strings.TrimSpace(c.options.NodeVersion)
	if nodeVersion == "" {
		nodeVersion = version.Version
	}
	goVersion := strings.TrimSpace(c.options.GoVersion)
	if goVersion == "" {
		goVersion = version.GoVersion
		if goVersion == "" {
			goVersion = runtime.Version()
		}
	}
	hostname, _ := os.Hostname()
	info := domain.SystemInfo{
		Hostname:     strings.TrimSpace(hostname),
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
		LogicalCPUs:  runtime.NumCPU(),
		NodeVersion:  nodeVersion,
		GoVersion:    goVersion,
		ServiceMode:  c.options.ServiceMode,
		IPAddresses:  localIPAddresses(),
		CollectedAt:  now,
	}
	snapshot, _ := collectPlatform(c.options.DiskPath)
	info.Distribution = snapshot.distribution
	info.DistributionVersion = snapshot.distributionVersion
	info.Kernel = snapshot.kernel
	info.CPUModel = snapshot.cpuModel
	info.UptimeSeconds = snapshot.uptimeSeconds
	info.MemoryTotalBytes = snapshot.memoryTotalBytes
	info.MemoryAvailableBytes = snapshot.memoryAvailable
	info.DiskTotalBytes = snapshot.diskTotalBytes
	info.DiskFreeBytes = snapshot.diskFreeBytes
	info.LoadAverage1m = snapshot.loadAverage1m
	if snapshot.cpuValid {
		c.mu.Lock()
		if c.prevAt.IsZero() {
			c.prev = snapshot.cpu
			c.prevAt = now
		} else if snapshot.cpu.total > c.prev.total && snapshot.cpu.idle >= c.prev.idle {
			totalDelta := snapshot.cpu.total - c.prev.total
			idleDelta := snapshot.cpu.idle - c.prev.idle
			usage := 100 * (1 - float64(idleDelta)/float64(totalDelta))
			if usage < 0 {
				usage = 0
			} else if usage > 100 {
				usage = 100
			}
			info.CPUUsagePercent = &usage
			c.prev = snapshot.cpu
			c.prevAt = now
		}
		c.mu.Unlock()
	}
	return info
}

func localIPAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	addresses := make([]string, 0, 8)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		values, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, value := range values {
			text := strings.TrimSpace(value.String())
			if address, _, splitErr := net.ParseCIDR(text); splitErr == nil {
				text = address.String()
			}
			if _, err := netip.ParseAddr(text); err != nil {
				continue
			}
			if _, ok := seen[text]; ok {
				continue
			}
			seen[text] = struct{}{}
			addresses = append(addresses, text)
		}
	}
	sort.Strings(addresses)
	return addresses
}
