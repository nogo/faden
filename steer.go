package faden

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotRunning is what a steering backend returns when the running turn
// takes no more input. The runner then queues the submission as a turn.
var ErrNotRunning = errors.New("faden: turn takes no more input")

// steeringBackend is a backend that takes input into a running turn.
type steeringBackend interface {
	// Steer delivers text to the running turn t. It calls store first and
	// writes only after store succeeds, so a stored message always reaches
	// the agent while the process lives. If t takes no more input, it returns
	// ErrNotRunning without calling store.
	Steer(ctx context.Context, t Turn, text string, store func() error) error
}

// steer joins s to the running turn of its session. It reports false, having
// stored nothing, when s must be queued as a turn of its own.
func (r *Runner) steer(ctx context.Context, s Submission) (Turn, bool, error) {
	if s.Context != "" || len(s.Files) > 0 {
		return Turn{}, false, nil // a steered message is text only
	}
	if t, err := r.store.Turn(ctx, s.RequestID); err == nil {
		return t, true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Turn{}, true, fmt.Errorf("faden: submit %s: %w", s.RequestID, err)
	}
	running, ok, err := r.runningTurn(ctx, s.Session)
	if err != nil {
		return Turn{}, true, fmt.Errorf("faden: submit %s: %w", s.RequestID, err)
	}
	if !ok || running.Backend != s.Backend || running.Model != s.Model {
		return Turn{}, false, nil
	}
	b, ok := r.backends[running.Backend].(steeringBackend)
	if !ok {
		return Turn{}, false, nil
	}
	var joined Turn
	err = b.Steer(ctx, running, s.Input, func() error {
		var err error
		joined, err = r.join(ctx, s, running.RequestID)
		return err
	})
	switch {
	case err == nil:
		return joined, true, nil
	case joined.RequestID == "" && errors.Is(err, ErrNotRunning):
		return Turn{}, false, nil
	case joined.RequestID != "":
		return joined, true, fmt.Errorf("faden: submit %s: joined %s, but not delivered: %w", s.RequestID, running.RequestID, err)
	default:
		return Turn{}, true, fmt.Errorf("faden: submit %s: %w", s.RequestID, err)
	}
}

// runningTurn returns the session's running turn if this runner runs it.
func (r *Runner) runningTurn(ctx context.Context, session string) (Turn, bool, error) {
	turns, err := r.store.Unfinished(ctx)
	if err != nil {
		return Turn{}, false, err
	}
	for _, t := range turns {
		if t.Session != session || t.Status != Running {
			continue
		}
		r.mu.Lock()
		local := r.turns[t.RequestID] != nil && r.ctx.Err() == nil
		r.mu.Unlock()
		return t, local, nil
	}
	return Turn{}, false, nil
}

// join stores s's input as a user message of the running turn, then s as a
// joined turn. steerMu orders it with Cancel and finish, so the running turn
// cannot end between the check and the writes. A crash between the writes
// leaves the message delivered twice at worst, never lost.
func (r *Runner) join(ctx context.Context, s Submission, running string) (Turn, error) {
	r.steerMu.Lock()
	defer r.steerMu.Unlock()
	t, err := r.store.Turn(ctx, running)
	if err != nil {
		return Turn{}, err
	}
	if t.Status != Running {
		return Turn{}, ErrNotRunning
	}
	if err := r.append(ctx, Event{Session: s.Session, RequestID: running, Kind: UserMessage, Text: s.Input, Time: time.Now().UTC()}); err != nil {
		return Turn{}, err
	}
	joined, err := r.store.SubmitTurn(ctx, Turn{
		RequestID: s.RequestID,
		Session:   s.Session,
		Input:     s.Input,
		Backend:   s.Backend,
		Model:     s.Model,
	})
	if err != nil {
		return Turn{}, err
	}
	joined.Status, joined.Joined = Joined, running
	if err := r.store.SaveTurn(ctx, joined); err != nil {
		return Turn{}, err
	}
	return joined, nil
}
