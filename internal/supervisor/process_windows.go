//go:build windows

package supervisor

import (
	"os"

	"golang.org/x/sys/windows"
)

const processStillActive = 259

func processExists(process *os.Process) bool {
	if process == nil || process.Pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(process.Pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	return exitCode == processStillActive
}
