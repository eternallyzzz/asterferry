//go:build windows

package systeminfo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

type fileTime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

var (
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	globalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	getSystemTimes       = kernel32.NewProc("GetSystemTimes")
	getTickCount64       = kernel32.NewProc("GetTickCount64")
)

func collectPlatform(diskPath string) (platformSnapshot, error) {
	result := platformSnapshot{distribution: "Windows"}
	version := windows.RtlGetVersion()
	if version != nil {
		result.distributionVersion = fmt.Sprintf("%d.%d (build %d)", version.MajorVersion, version.MinorVersion, version.BuildNumber)
		result.kernel = result.distributionVersion
	}
	result.cpuModel = strings.TrimSpace(os.Getenv("PROCESSOR_IDENTIFIER"))
	var memory memoryStatusEx
	memory.Length = uint32(unsafe.Sizeof(memory))
	if value, _, _ := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&memory))); value != 0 {
		result.memoryTotalBytes = memory.TotalPhys
		result.memoryAvailable = memory.AvailPhys
	}
	if value, _, _ := getTickCount64.Call(); value != 0 {
		result.uptimeSeconds = uint64(value / 1000)
	}
	var idle, kernel, user fileTime
	if value, _, _ := getSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); value != 0 {
		idleValue := uint64(idle.HighDateTime)<<32 | uint64(idle.LowDateTime)
		kernelValue := uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
		userValue := uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
		result.cpu = cpuSample{idle: idleValue, total: kernelValue + userValue}
		result.cpuValid = true
	}
	if diskPath == "" {
		diskPath = filepath.VolumeName(os.Getenv("SystemDrive"))
		if diskPath == "" {
			diskPath = os.Getenv("SystemDrive")
		}
	}
	if diskPath == "" {
		diskPath = `C:\`
	}
	directory, err := windows.UTF16PtrFromString(diskPath)
	if err == nil {
		var free, total, totalFree uint64
		if windows.GetDiskFreeSpaceEx(directory, &free, &total, &totalFree) == nil {
			result.diskTotalBytes = total
			result.diskFreeBytes = totalFree
		}
	}
	return result, nil
}
