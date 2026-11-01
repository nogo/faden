package faden

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func (l Local) pidFile() (string, error) {
	if l.PIDDir == "" {
		return "", errors.New("faden: local run: PIDDir is required")
	}
	// The host creates the directory so its parent entry is durable too.
	info, err := os.Stat(l.PIDDir)
	if err != nil {
		return "", fmt.Errorf("faden: local pid directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("faden: local PIDDir is not a directory")
	}
	return filepath.Join(l.PIDDir, "process.pid"), nil
}

func stopLocalGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func stopLocalLeftover(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return fmt.Errorf("invalid process group in %s", path)
	}
	return stopLocalGroup(pid)
}

func recordLocalGroup(path string, pid int) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(file, pid)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
