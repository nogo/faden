//go:build unix

package jsonl

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/nogo/faden"
)

// lockFile takes an exclusive flock on path without waiting. The kernel drops
// the lock when the process dies.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("jsonl: lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("jsonl: %s: %w", path, faden.ErrLocked)
		}
		return nil, fmt.Errorf("jsonl: lock: %w", err)
	}
	return f, nil
}
