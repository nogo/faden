package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/nogo/faden"
)

// input owns one process's stdin. mu guards messages; writeMu orders writes.
// Result handling never waits for a write: the CLI may be waiting on stdout.
type input struct {
	mu       sync.Mutex
	writeMu  sync.Mutex
	writer   *os.File
	messages int
}

// Steer writes another stream-json user message to the running process.
func (b *Backend) Steer(ctx context.Context, t faden.Turn, text string) error {
	b.mu.Lock()
	in := b.inputs[t.RequestID]
	b.mu.Unlock()
	if in == nil {
		return faden.ErrNotRunning
	}
	stop := context.AfterFunc(ctx, func() { in.writer.Close() })
	defer stop()
	in.writeMu.Lock()
	defer in.writeMu.Unlock()
	in.mu.Lock()
	if in.messages == 0 {
		in.mu.Unlock()
		return faden.ErrNotRunning
	}
	if err := ctx.Err(); err != nil {
		in.mu.Unlock()
		return err
	}
	in.messages++
	in.mu.Unlock()
	r, done := userMessage(text, nil)
	defer done()
	if _, err := io.Copy(in.writer, r); err != nil {
		in.writer.Close()
		return fmt.Errorf("claude: steer: %w", err)
	}
	return nil
}

func (in *input) result() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.messages--
	if in.messages == 0 {
		in.writer.Close()
		return true
	}
	return false
}
