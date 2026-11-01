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

// Submission is what a host asks the runner to run.
type Submission struct {
	Session   string
	RequestID string
	Input     string
	Backend   string
	Model     string
}

// Open starts a runner on store with the backends by name. Before it returns,
// every running turn has its attempt count raised in the store; each then
// continues in the background. The host owns the store and closes it after Close.
func Open(store Store, backends map[string]Backend) (*Runner, error) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		store:    store,
		backends: backends,
		ctx:      ctx,
		cancel:   cancel,
		active:   map[string]bool{},
		changed:  make(chan struct{}),
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
