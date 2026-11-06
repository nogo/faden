package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/nogo/faden"
)

// input owns one process's stdin until Claude's first result. writeMu orders
// writes. mu guards closed and steering and is never held during a write, so
// close never waits: the CLI may be waiting on stdout.
type input struct {
	writeMu  sync.Mutex
	writer   *os.File
	mu       sync.Mutex
	closed   bool // no new steer is taken
	steering bool // a steer between its store and its write holds stdin open
}

// Steer writes another stream-json user message to the running process.
// Claude takes it in at its next tool call, or answers it after the current
// answer. Once Claude's first result arrives, Steer returns ErrNotRunning.
func (b *Backend) Steer(ctx context.Context, t faden.Turn, text string, store func() error) error {
	b.mu.Lock()
	in := b.inputs[t.RequestID]
	b.mu.Unlock()
	if in == nil {
		return faden.ErrNotRunning
	}
	in.writeMu.Lock()
	defer in.writeMu.Unlock()
	if !in.reserve() {
		return faden.ErrNotRunning
	}
	defer in.release()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store(); err != nil {
		return err
	}
	// Only a blocked write waits for ctx; it closes stdin to unblock.
	stop := context.AfterFunc(ctx, func() { in.writer.Close() })
	defer stop()
	r, done := userMessage(text, nil)
	defer done()
	if _, err := io.Copy(in.writer, r); err != nil {
		in.close()
		return fmt.Errorf("claude: steer: %w", err)
	}
	return nil
}

// reserve holds stdin open for one steer. It fails once stdin is closed.
func (in *input) reserve() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return false
	}
	in.steering = true
	return true
}

// release ends a steer, and closes stdin if close came during it.
func (in *input) release() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.steering = false
	if in.closed {
		in.writer.Close()
	}
}

// close ends Claude's input. During a steer, release closes stdin after the
// write, so a stored message still reaches Claude. Claude answers every
// message it has read, then exits.
func (in *input) close() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.closed = true
	if !in.steering {
		in.writer.Close()
	}
}
