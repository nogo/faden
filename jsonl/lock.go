//go:build unix

package jsonl

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/nogo/faden"
)

// lockFile takes an exclusive flock on f. Without wait it fails at once with
// faden.ErrLocked when another open file description holds the lock, in this
// process or another. The kernel drops the lock when the descriptor closes,
// so a process that dies loses its locks.
func lockFile(f *os.File, wait bool) error {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), how)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
		case errors.Is(err, syscall.EWOULDBLOCK):
			return fmt.Errorf("jsonl: %s: %w", f.Name(), faden.ErrLocked)
		default:
			return fmt.Errorf("jsonl: lock %s: %w", f.Name(), err)
		}
	}
}

func unlockFile(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
