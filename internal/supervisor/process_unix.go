//go:build !windows

package supervisor

import (
	"os"
	"syscall"
)

func processExists(process *os.Process) bool {
	if process == nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
