package faden

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotRunning means the requested turn cannot receive input now.
var ErrNotRunning = errors.New("faden: turn is not running")

// ErrCannotSteer means the backend does not support input during a turn.
var ErrCannotSteer = errors.New("faden: backend cannot steer")

type steeringBackend interface {
	Steer(context.Context, Turn, string) error
}

// Steer stores another user message, then delivers it to the running backend.
// A delivery error leaves the message stored for continuation and thread reading.
func (r *Runner) Steer(ctx context.Context, requestID, text string) (Turn, error) {
	t, b, err := r.storeSteering(ctx, requestID, text)
	if err != nil {
		return t, err
	}
	if err := b.Steer(ctx, t, text); err != nil {
		return t, fmt.Errorf("faden: steer %s: message stored: %w", requestID, err)
	}
	return t, nil
}

// storeSteering orders the status check and append with cancellation and
// completion. Delivery holds no runner lock, so Cancel can stop a blocked write.
func (r *Runner) storeSteering(ctx context.Context, requestID, text string) (Turn, steeringBackend, error) {
	r.steerMu.Lock()
	defer r.steerMu.Unlock()
	r.mu.Lock()
	t, err := r.store.Turn(ctx, requestID)
	local := r.turns[requestID] != nil && r.ctx.Err() == nil
	r.mu.Unlock()
	if err != nil {
		return t, nil, fmt.Errorf("faden: steer %s: %w", requestID, err)
	}
	if t.Status != Running {
		return t, nil, fmt.Errorf("faden: steer %s: %w", requestID, ErrNotRunning)
	}
	b, ok := r.backends[t.Backend].(steeringBackend)
	if !ok {
		return t, nil, ErrCannotSteer
	}
	if !local {
		return t, nil, fmt.Errorf("faden: steer %s: %w", requestID, ErrLocked)
	}
	if err := r.append(ctx, Event{Session: t.Session, RequestID: requestID, Kind: UserMessage, Text: text, Time: time.Now().UTC()}); err != nil {
		return t, nil, fmt.Errorf("faden: steer %s: %w", requestID, err)
	}
	return t, b, nil
}
