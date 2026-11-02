package faden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	maxAttempts    = 3
	continuePrompt = "Continue where you left off."
)

// Runner runs turns, one at a time per session, and continues the turns a
// crash cut off.
type Runner struct {
	budget   int
	store    Store
	backends map[string]Backend
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu      sync.Mutex      // guards active, changed and fatal
	active  map[string]bool // sessions with a worker
	changed chan struct{}   // closed and replaced when a turn reaches a final status
	fatal   error           // first store error a worker met
}

// catchUp derives the boundary from executed turns, not the queued session backend.
func (r *Runner) catchUp(t *Turn, emit func(Event) error) error {
	events, err := r.store.Events(r.ctx, t.Session, 0)
	if err != nil {
		return err
	}
	var previous string
	var after int64
	var marker *Event
	backends := make(map[string]string)
	for i, e := range events {
		if e.RequestID == t.RequestID {
			if e.Kind == Handover {
				marker = &events[i]
			}
			continue
		}
		name, ok := backends[e.RequestID]
		if !ok {
			turn, err := r.store.Turn(r.ctx, e.RequestID)
			if err != nil {
				return err
			}
			name = turn.Backend
			backends[e.RequestID] = name
		}
		previous = name
		if name == t.Backend {
			after = e.Seq
		}
	}
	if marker == nil && (previous == "" || previous == t.Backend) {
		return nil
	}
	boundary := HandoverBoundary{From: previous, To: t.Backend, After: after, Budget: r.budget}
	if marker != nil {
		if boundary, err = HandoverOf(*marker); err != nil {
			return err
		}
	} else if err := emit(boundary.event()); err != nil {
		return err
	}
	t.HandoverBudget = boundary.Budget
	for _, e := range events {
		if e.Seq > boundary.After && e.RequestID != t.RequestID {
			t.CatchUp = append(t.CatchUp, e)
		}
	}
	return nil
}

// HandoverBoundary is what a handover event records: the backend that
// held the session, the one that takes it, the last event the taking
// backend had seen, and the host's byte budget for the catch-up. A retry of
// the turn reads it back, so it sees the same range and budget.
type HandoverBoundary struct {
	From   string `json:"from"`
	To     string `json:"to"`
	After  int64  `json:"after"`
	Budget int    `json:"budget"`
}

// HandoverOf returns the boundary a handover event records. It fails for an
// event of another kind. Read a handover only through it: the event's
// encoding belongs to this package.
func HandoverOf(e Event) (HandoverBoundary, error) {
	var b HandoverBoundary
	if e.Kind != Handover {
		return b, fmt.Errorf("faden: event %d is a %s, not a handover", e.Seq, e.Kind)
	}
	if err := json.Unmarshal(e.Input, &b); err != nil {
		return b, fmt.Errorf("faden: handover %d: %w", e.Seq, err)
	}
	return b, nil
}

// event encodes the boundary as a handover event. Text is for people only.
func (b HandoverBoundary) event() Event {
	data, _ := json.Marshal(b) // plain strings and numbers cannot fail
	return Event{Kind: Handover, Action: b.To, Text: b.From + " -> " + b.To, Input: data}
}

// Submission is what a host asks the runner to run.
type Submission struct {
	Session   string
	RequestID string
	Input     string
	Backend   string
	Model     string
}

// Options controls handover context. Zero Budget uses 16,000 bytes.
type Options struct{ Budget int }

// Open starts a runner on store with the backends by name. Before it returns,
// every running turn has its attempt count raised in the store; each then
// continues in the background. The host owns the store and closes it after Close.
func Open(store Store, backends map[string]Backend, options ...Options) (*Runner, error) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		store:    store,
		backends: backends,
		ctx:      ctx,
		cancel:   cancel,
		active:   map[string]bool{},
		changed:  make(chan struct{}),
	}
	if len(options) > 0 {
		r.budget = options[0].Budget
	}
	turns, err := store.Unfinished(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("faden: open: %w", err)
	}
	for _, t := range turns {
		if t.Status != Running {
			continue
		}
		if t.Attempts >= maxAttempts {
			t.Status = Failed
			t.Error = fmt.Sprintf("faden: turn stopped after %d attempts; send a new turn", t.Attempts)
			// A given-up turn never continues, so nothing else would stop
			// what its last attempt left running.
			if b, ok := backends[t.Backend]; ok {
				if err := b.Stop(ctx, t); err != nil {
					t.Error += fmt.Sprintf("; its leftover could not be stopped: %v", err)
				}
			}
		} else {
			t.Attempts++
		}
		if err := store.SaveTurn(ctx, t); err != nil {
			cancel()
			return nil, fmt.Errorf("faden: open: turn %s: %w", t.RequestID, err)
		}
	}
	turns, err = store.Unfinished(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("faden: open: %w", err)
	}
	for _, t := range turns {
		r.kick(t.Session)
	}
	return r, nil
}

// Submit stores a pending turn and returns it. A known RequestID returns the
// existing turn and runs nothing.
func (r *Runner) Submit(ctx context.Context, s Submission) (Turn, error) {
	t, err := r.store.SubmitTurn(ctx, Turn{
		RequestID: s.RequestID,
		Session:   s.Session,
		Input:     s.Input,
		Backend:   s.Backend,
		Model:     s.Model,
	})
	if err != nil {
		return Turn{}, fmt.Errorf("faden: submit %s: %w", s.RequestID, err)
	}
	r.kick(t.Session)
	return t, nil
}

// Wait returns the turn once it is done or failed.
func (r *Runner) Wait(ctx context.Context, requestID string) (Turn, error) {
	for {
		r.mu.Lock()
		changed, fatal := r.changed, r.fatal
		r.mu.Unlock()
		t, err := r.store.Turn(ctx, requestID)
		if err != nil {
			return Turn{}, fmt.Errorf("faden: wait %s: %w", requestID, err)
		}
		if t.Status.Final() {
			return t, nil
		}
		if fatal != nil {
			return Turn{}, fatal
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return Turn{}, ctx.Err()
		case <-r.ctx.Done():
			return Turn{}, errors.New("faden: wait: runner closed")
		}
	}
}

// Close stops the running turns and waits for them. A stopped turn stays
// running, so the next Open continues it.
func (r *Runner) Close() error {
	r.cancel()
	r.wg.Wait()
	return nil
}

func (r *Runner) kick(session string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[session] || r.ctx.Err() != nil {
		return
	}
	r.active[session] = true
	r.wg.Add(1)
	go r.work(session)
}

// work runs the turns of one session, oldest first, until none is left.
func (r *Runner) work(session string) {
	defer r.wg.Done()
	for {
		r.mu.Lock()
		t, ok, err := r.next(session)
		if !ok || err != nil || r.ctx.Err() != nil {
			r.active[session] = false
			r.mu.Unlock()
			if err != nil {
				r.fail(err)
			}
			return
		}
		r.mu.Unlock()
		if err := r.run(t); err != nil {
			r.mu.Lock()
			r.active[session] = false
			r.mu.Unlock()
			r.fail(err)
			return
		}
	}
}

func (r *Runner) next(session string) (Turn, bool, error) {
	turns, err := r.store.Unfinished(r.ctx)
	if err != nil {
		return Turn{}, false, fmt.Errorf("faden: list turns: %w", err)
	}
	for _, t := range turns {
		if t.Session == session {
			return t, true, nil
		}
	}
	return Turn{}, false, nil
}

func (r *Runner) fail(err error) {
	if r.ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	if r.fatal == nil {
		r.fatal = err
	}
	r.broadcast()
	r.mu.Unlock()
}

// broadcast wakes every Wait. The caller holds r.mu.
func (r *Runner) broadcast() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *Runner) finish(t Turn, status Status, msg string) error {
	t.Status, t.Error = status, msg
	if err := r.store.SaveTurn(r.ctx, t); err != nil {
		return fmt.Errorf("faden: finish turn %s: %w", t.RequestID, err)
	}
	r.mu.Lock()
	r.broadcast()
	r.mu.Unlock()
	return nil
}

// recordInput stores the turn's user message unless an earlier attempt did.
// The runner owns it, not the backend: the input is in the store from
// Submit on, so a crash before the first event cannot lose it, and a
// continuation prompt never enters the thread as something the user said.
// It comes after any handover event, so a backend rebuilding its history
// finds the handover before the request. It also marks which backend ran
// the turn, which catchUp needs even when the backend emits nothing.
func (r *Runner) recordInput(t Turn, emit func(Event) error) error {
	events, err := r.store.Events(r.ctx, t.Session, 0)
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.RequestID == t.RequestID && e.Kind == UserMessage {
			return nil
		}
	}
	return emit(Event{Kind: UserMessage, Text: t.Input})
}

// run runs one turn. A running turn already has its attempt raised by Open.
func (r *Runner) run(t Turn) error {
	if t.Status == Pending {
		t.Status, t.Attempts = Running, 1
		if err := r.store.SaveTurn(r.ctx, t); err != nil {
			return fmt.Errorf("faden: start turn %s: %w", t.RequestID, err)
		}
	}
	backend, ok := r.backends[t.Backend]
	if !ok {
		return r.finish(t, Failed, fmt.Sprintf("faden: no backend %q", t.Backend))
	}
	session, err := r.store.Session(r.ctx, t.Session)
	if err != nil {
		return fmt.Errorf("faden: turn %s: %w", t.RequestID, err)
	}
	send := t
	if t.Attempts > 1 {
		if err := backend.Stop(r.ctx, t); err != nil {
			return r.finish(t, Failed, fmt.Sprintf("faden: stop turn %s: %v", t.RequestID, err))
		}
		send.Input = continuePrompt
		if backend.ContinuesWithoutPrompt() {
			send.Input = ""
		}
	}
	emit := func(e Event) error {
		e.Session, e.RequestID = t.Session, t.RequestID
		if e.Time.IsZero() {
			e.Time = time.Now().UTC()
		}
		_, err := r.store.AppendEvent(r.ctx, e)
		return err
	}
	if err := r.catchUp(&send, emit); err != nil {
		return fmt.Errorf("faden: handover turn %s: %w", t.RequestID, err)
	}
	if err := r.recordInput(t, emit); err != nil {
		return fmt.Errorf("faden: record input of turn %s: %w", t.RequestID, err)
	}
	cursor, runErr := backend.Run(r.ctx, send, session.Cursors[t.Backend], emit)
	if r.ctx.Err() != nil {
		return nil // closed: the turn stays running for the next Open
	}
	if len(cursor) > 0 {
		if err := r.store.SetCursor(r.ctx, t.Session, t.Backend, json.RawMessage(cursor)); err != nil {
			return fmt.Errorf("faden: turn %s: %w", t.RequestID, err)
		}
	}
	if runErr != nil {
		return r.finish(t, Failed, runErr.Error())
	}
	return r.finish(t, Done, "")
}
