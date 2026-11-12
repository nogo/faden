package faden

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// processStart names a process by its start time, so a reused PID does not match a record.
// It reads field 22 after the parenthesized command, which can contain spaces.
// A process that does not exist gives an error that matches os.ErrNotExist.
func processStart(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", errors.New("faden: invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", errors.New("faden: incomplete process stat")
	}
	return fields[19], nil
}
