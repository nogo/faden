package llama

import (
	"context"
	"sync"
	"time"
)

// RecordWaits makes b retry at once. The returned func lists the delays b
// would have waited, in order.
func RecordWaits(b *Backend) func() []time.Duration {
	var mu sync.Mutex
	var waits []time.Duration
	b.wait = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
		return ctx.Err()
	}
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), waits...)
	}
}
