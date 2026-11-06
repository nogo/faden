package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/nogo/faden"
)

// input owns one process's stdin until Claude's first result. writeMu orders
// writes. close never waits for a write: the CLI may be waiting on stdout.
type input struct {
	writeMu sync.Mutex
	writer  *os.File
	closed  atomic.Bool
}

// Steer writes another stream-json user message to the running process.
// Claude takes it in at its next tool call, or answers it after the current
// answer. Once Claude's first result arrives, stdin is closed and Steer
// returns ErrNotRunning.
func (b *Backend) Steer(ctx context.Context, t faden.Turn, text string) error {
	b.mu.Lock()
	in := b.inputs[t.RequestID]
	b.mu.Unlock()
	if in == nil {
		return faden.ErrNotRunning
	}
	stop := context.AfterFunc(ctx, in.close)
	defer stop()
	in.writeMu.Lock()
	defer in.writeMu.Unlock()
	if in.closed.Load() {
		return faden.ErrNotRunning
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r, done := userMessage(text, nil)
	defer done()
	if _, err := io.Copy(in.writer, r); err != nil {
		if in.closed.Load() && ctx.Err() == nil {
			return faden.ErrNotRunning // the first result closed stdin during the write
		}
		in.close()
		return fmt.Errorf("claude: steer: %w", err)
	}
	return nil
}

// close ends Claude's input. Claude still answers every message it has read,
// then exits.
func (in *input) close() {
	in.closed.Store(true)
	in.writer.Close()
}
