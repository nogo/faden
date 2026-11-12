package faden

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// processStart names a process by its start time, so a reused PID does not match a record.
// It reads the kernel's process table entry.
// A process that does not exist gives an error that matches os.ErrNotExist.
func processStart(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	// The kernel answers an unknown PID with an empty entry, which x/sys reports as EIO.
	if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) {
		return "", fmt.Errorf("faden: process %d: %w", pid, os.ErrNotExist)
	}
	if err != nil {
		return "", fmt.Errorf("faden: process %d: %w", pid, err)
	}
	start := info.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", start.Sec, start.Usec), nil
}
