package faden

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func (l Local) pidFile(key string) (string, error) {
	if l.PIDDir == "" {
		return "", errors.New("faden: local run: PIDDir is required")
	}
	info, err := os.Stat(l.PIDDir)
	if err != nil {
		return "", fmt.Errorf("faden: local pid directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("faden: local PIDDir is not a directory")
	}
	return filepath.Join(l.PIDDir, fmt.Sprintf("%x.pid", sha256.Sum256([]byte(key)))), nil
}

func stopLocalGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// processStart reads field 22 after the parenthesized command, which can contain spaces.
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

func stopLocalLeftover(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 2 {
		pid, parseErr := strconv.Atoi(fields[0])
		if parseErr == nil && pid > 1 {
			start, err := processStart(pid)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil && start == fields[1] {
				if err := stopLocalGroup(pid); err != nil {
					return err
				}
			}
		}
	}
	return os.Remove(path)
}

func recordLocalGroup(path string, pid int) error {
	start, err := processStart(pid)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".process-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := fmt.Fprintln(file, pid, start)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
