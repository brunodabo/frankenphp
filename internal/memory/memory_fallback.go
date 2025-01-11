//go:build !linux

// -build linux

package memory

import "syscall"

// Return 0 if the total system memory cannot be determined
func TotalSysMemory() uint64 {
	return 0
}
