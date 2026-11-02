package faden

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Attempt is one run of a turn on its backend, and a backend's only way to
// the store. It is bound to the turn's session and backend: the runner makes
// one per run and passes it to Backend.Run, so a backend needs no store and
// no name of its own.
type Attempt struct {
	store   Store
	turn    Turn
	cursor  json.RawMessage
	catchUp []Event
	budget  int
}

// NewAttempt binds a run of t to store, with the cursor the store holds for
// t.Backend. t must be submitted; an unknown session is ErrNotFound. The
// runner makes its own; call this to run a backend without a runner, as a
// backend's tests do. Such an attempt has no catch-up.
func NewAttempt(ctx context.Context, store Store, t Turn) (*Attempt, error) {
	s, err := store.Session(ctx, t.Session)
	if err != nil {
		return nil, fmt.Errorf("faden: attempt %s: %w", t.RequestID, err)
	}
	return &Attempt{store: store, turn: t, cursor: s.Cursors[t.Backend]}, nil
}

// Cursor is what this backend last saved for the session, or nil.
func (a *Attempt) Cursor() json.RawMessage { return a.cursor }

// SaveCursor stores the backend's resume cursor now, so a crash later in
// the turn keeps it. The next attempt's Cursor returns it.
func (a *Attempt) SaveCursor(ctx context.Context, cursor json.RawMessage) error {
	if err := a.store.SetCursor(ctx, a.turn.Session, a.turn.Backend, cursor); err != nil {
		return err
	}
	a.cursor = cursor
	return nil
}

// Emit stores one completed item of the turn. It sets the session, request
// id and, when zero, the time.
func (a *Attempt) Emit(ctx context.Context, e Event) error {
	e.Session, e.RequestID = a.turn.Session, a.turn.RequestID
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	_, err := a.store.AppendEvent(ctx, e)
	return err
}

// Events returns the session's events with Seq above after, including the
// ones this attempt emitted.
func (a *Attempt) Events(ctx context.Context, after int64) ([]Event, error) {
	return a.store.Events(ctx, a.turn.Session, after)
}

// CatchUp returns the events this backend has not seen and the byte budget
// for selecting from them; zero budget means 16,000. After a handover they
// are the session's events since the backend last ran; on a continuation
// that needs a prompt, they also hold what earlier attempts of the turn
// stored. handover.Prompt renders them.
func (a *Attempt) CatchUp() ([]Event, int) { return a.catchUp, a.budget }
