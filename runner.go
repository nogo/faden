package faden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/nogo/faden/internal/attach"
)

const (
	maxAttempts    = 3
	continuePrompt = "Continue where you left off."
)

// Runner runs turns, one at a time per session, and continues the turns a
// crash cut off. It runs only the sessions it has claimed in the store, so
// several runners on one store serve different sessions at once.
type Runner struct {
	budget   int
	prepare  func(context.Context, Turn) (Setup, error)
	poll     time.Duration
	store    Store
	backends map[string]Backend
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	watchWG  sync.WaitGroup
	pollWG   sync.WaitGroup        // the loop that adopts sessions
	watchers map[*watcher]struct{} // guarded by mu

	steerMu sync.Mutex // orders steering appends, cancellation and completion

	// mu guards active, turns, changed, draining, fatal and watchers. It is
	// held only for work in memory, with one exception: the status change of
	// a turn (run starting it, finish, Cancel) reads and writes the store
	// under mu, so a cancel and a finish cannot both win. An event append
	// never holds it.
	mu       sync.Mutex
	turns    map[string]context.CancelFunc
	active   map[string]bool // sessions with a worker
	changed  chan struct{}   // closed and replaced when a turn reaches a final status
	fatal    error           // first store error a worker met
	draining bool            // Drain was called: no turn starts
}

// history is what a turn needs from its session's earlier events. One pass
// gathers it while holding one event at a time: the other turns are kept as
// their last sequence numbers, and only this turn's own events are kept whole.
type history struct {
	last      map[string]int64 // request id of another turn -> seq of its last event
	lastOther string           // request id of the newest event of another turn
	marker    *Event           // this turn's handover event, if a retry finds one
	own       []Event          // this turn's other events, when the caller keeps them
	hasInput  bool             // this turn's user message is stored
}

// readHistory reads the session once before t runs. keepOwn keeps t's own
// events for a continuation prompt.
func (r *Runner) readHistory(t Turn, keepOwn bool) (history, error) {
	h := history{last: map[string]int64{}}
	for e, err := range r.store.Events(r.ctx, t.Session, 0) {
		if err != nil {
			return h, err
		}
		switch {
		case e.RequestID != t.RequestID:
			h.last[e.RequestID] = e.Seq
			h.lastOther = e.RequestID
		case e.Kind == Handover:
			h.marker = &e
		default:
			h.hasInput = h.hasInput || e.Kind == UserMessage
			if keepOwn {
				h.own = append(h.own, e)
			}
		}
	}
	return h, nil
}

// catchUp derives the boundary from executed turns, not the queued session
// backend. Only a handover reads the session again, from its boundary on.
func (r *Runner) catchUp(a *Attempt, h history) error {
	t := a.turn
	var boundary HandoverBoundary
	if h.marker != nil {
		var err error
		if boundary, err = HandoverOf(*h.marker); err != nil {
			return err
		}
	} else {
		if h.lastOther == "" {
			return nil
		}
		previous, err := r.backendOf(h.lastOther)
		if err != nil || previous == t.Backend {
			return err
		}
		boundary = HandoverBoundary{From: previous, To: t.Backend, Budget: r.budget}
		for id, seq := range h.last {
			if seq <= boundary.After {
				continue
			}
			name, err := r.backendOf(id)
			if err != nil {
				return err
			}
			if name == t.Backend {
				boundary.After = seq
			}
		}
		if err := a.Emit(r.ctx, boundary.event()); err != nil {
			return err
		}
	}
	a.budget = boundary.Budget
	for e, err := range r.store.Events(r.ctx, t.Session, boundary.After) {
		if err != nil {
			return err
		}
		if e.RequestID != t.RequestID {
			a.catchUp = append(a.catchUp, e)
		}
	}
	return nil
}

// backendOf names the backend that ran the turn with this request id.
func (r *Runner) backendOf(requestID string) (string, error) {
	turn, err := r.store.Turn(r.ctx, requestID)
	return turn.Backend, err
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
	// Context is stored with the turn and sent before Input on every attempt.
	Context string
	// Files are paths relative to the attempt's Setup.Dir, sent with every attempt.
	Files   []string
	Backend string
	Model   string
}

// Setup is the host's preparation of one attempt. A backend applies what it
// can; a field it cannot apply is ignored. Setup is never stored, because Env
// may carry a credential.
type Setup struct {
	// Dir is the working directory of the attempt.
	Dir string
	// Args follow the backend's own arguments.
	Args []string
	// Env holds NAME=value entries added to the command's environment.
	Env []string
	// System is the system prompt, added to the agent's own.
	System string
}

// Options configures the runner.
type Options struct {
	// Budget is the byte budget of a handover. Zero uses 16,000 bytes.
	Budget int
	// Prepare, if set, runs before every attempt, a continuation included.
	// t.Attempts is 1 for a first attempt and above 1 for a continuation, so
	// a host can keep the workspace a killed attempt left. Its result reaches
	// the backend through Attempt.Setup. An error fails the turn.
	Prepare func(ctx context.Context, t Turn) (Setup, error)
	// Poll is how often the runner looks in the store for what other
	// processes did: a session whose owner is gone, and a turn that another
	// process runs (for Wait). Zero uses one second.
	Poll time.Duration
}

// Open starts a runner on store with the backends by name. Before it returns,
// the runner has claimed every session with an unfinished turn that has no
// live owner, and every running turn in those sessions has its attempt count
// raised in the store; each then continues in the background. A session owned
// by another process is left to it. The runner looks again every Options.Poll,
// so it also takes over a session whose owner died. The host owns the store
// and closes it after Close.
func Open(store Store, backends map[string]Backend, options ...Options) (*Runner, error) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		store:    store,
		backends: backends,
		ctx:      ctx,
		cancel:   cancel,
		active:   map[string]bool{},
		turns:    map[string]context.CancelFunc{},
		changed:  make(chan struct{}),
	}
	if len(options) > 0 {
		r.budget = options[0].Budget
		r.prepare = options[0].Prepare
		r.poll = options[0].Poll
	}
	if r.poll <= 0 {
		r.poll = time.Second
	}
	if err := r.adoptAll(); err != nil {
		r.Close()
		return nil, fmt.Errorf("faden: open: %w", err)
	}
	r.pollWG.Go(func() {
		tick := time.NewTicker(r.poll)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				// A failed look is retried at the next tick; a store that stays
				// broken fails the calls that need it.
				r.adoptAll()
			}
		}
	})
	return r, nil
}

// adoptAll tries to claim every session that has an unfinished turn.
func (r *Runner) adoptAll() error {
	turns, err := r.store.Unfinished(r.ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, t := range turns {
		if seen[t.Session] {
			continue
		}
		seen[t.Session] = true
		if err := r.kick(t.Session); err != nil {
			return err
		}
	}
	return nil
}

// adopt claims session and prepares the running turns it finds there for a
// continuation: their owner is gone, so the attempt count rises, or the turn
// is given up after maxAttempts. It reports false when another process owns
// the session.
func (r *Runner) adopt(session string) (bool, error) {
	if err := r.store.Claim(r.ctx, session); err != nil {
		if errors.Is(err, ErrLocked) {
			return false, nil
		}
		return false, fmt.Errorf("claim session %s: %w", session, err)
	}
	turns, err := r.store.Unfinished(r.ctx)
	if err != nil {
		return true, err
	}
	for _, t := range turns {
		if t.Session != session || t.Status != Running {
			continue
		}
		if t.Attempts >= maxAttempts {
			t.Status = Failed
			t.Error = fmt.Sprintf("faden: turn stopped after %d attempts; send a new turn", t.Attempts)
			// A given-up turn never continues, so nothing else would stop
			// what its last attempt left running.
			if b, ok := r.backends[t.Backend]; ok {
				if err := b.Stop(r.ctx, t); err != nil {
					t.Error += fmt.Sprintf("; its leftover could not be stopped: %v", err)
				}
			}
		} else {
			t.Attempts++
		}
		if err := r.store.SaveTurn(r.ctx, t); err != nil {
			return true, fmt.Errorf("turn %s: %w", t.RequestID, err)
		}
	}
	return true, nil
}

// Submit stores a pending turn and returns it. A known RequestID returns the
// existing turn and runs nothing. If another process owns the session, that
// process runs the turn.
func (r *Runner) Submit(ctx context.Context, s Submission) (Turn, error) {
	t, err := r.store.SubmitTurn(ctx, Turn{
		RequestID: s.RequestID,
		Session:   s.Session,
		Input:     s.Input,
		Context:   s.Context,
		Files:     slices.Clone(s.Files),
		Backend:   s.Backend,
		Model:     s.Model,
	})
	if err != nil {
		return Turn{}, fmt.Errorf("faden: submit %s: %w", s.RequestID, err)
	}
	if err := r.kick(t.Session); err != nil {
		return t, fmt.Errorf("faden: submit %s: turn stored: %w", s.RequestID, err)
	}
	return t, nil
}

// Cancel permanently ends one turn. Final turns are returned unchanged.
// The cancellation is stored before the backend is stopped.
func (r *Runner) Cancel(ctx context.Context, requestID string) (Turn, error) {
	r.steerMu.Lock()
	defer r.steerMu.Unlock()
	r.mu.Lock()
	t, err := r.store.Turn(ctx, requestID)
	if err != nil {
		r.mu.Unlock()
		return Turn{}, fmt.Errorf("faden: cancel %s: %w", requestID, err)
	}
	if t.Status.Final() {
		r.mu.Unlock()
		return t, nil
	}
	running := t.Status == Running
	t.Status = Cancelled
	if err := r.store.SaveTurn(ctx, t); err != nil {
		r.mu.Unlock()
		return Turn{}, fmt.Errorf("faden: cancel %s: %w", requestID, err)
	}
	r.broadcast()
	if cancel := r.turns[requestID]; cancel != nil {
		cancel()
	}
	r.mu.Unlock()
	if running {
		// Only a turn that started has work in the thread to cut off. A
		// pending turn leaves no event, so no reader of the thread meets a
		// request that ended without its user message.
		if err := r.append(ctx, Event{Session: t.Session, RequestID: requestID, Kind: TurnEnded, Text: "cancelled", Time: time.Now().UTC()}); err != nil {
			return t, fmt.Errorf("faden: cancel event %s: %w", requestID, err)
		}
		if b := r.backends[t.Backend]; b != nil {
			if err := b.Stop(ctx, t); err != nil {
				return t, fmt.Errorf("faden: stop cancelled turn %s: %w", requestID, err)
			}
		}
	}
	return t, nil
}

// Wait returns the turn once it reaches a final status.
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
		// Another process may finish the turn, and only the store shows it.
		poll := time.NewTimer(r.poll)
		select {
		case <-changed:
		case <-poll.C:
		case <-ctx.Done():
			poll.Stop()
			return Turn{}, ctx.Err()
		case <-r.ctx.Done():
			poll.Stop()
			return Turn{}, errors.New("faden: wait: runner closed")
		}
		poll.Stop()
	}
}

// Close stops the running turns and waits for them. A stopped turn stays
// running, so the next Open continues it.
func (r *Runner) Close() error {
	r.mu.Lock()
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
	r.pollWG.Wait()
	r.watchWG.Wait()
	return nil
}

// Drain starts no new turn and waits for the running turns to end. Turns
// submitted meanwhile stay pending. When ctx is done first, Drain closes the
// runner as Close does and returns the context's error; the turns still
// running continue on the next Open. The host sets the grace period with
// the deadline of ctx.
func (r *Runner) Drain(ctx context.Context) error {
	r.mu.Lock()
	r.draining = true
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		r.Close()
		return fmt.Errorf("faden: drain: %w", ctx.Err())
	}
}

// kick starts the worker of a session unless it has one, the runner drains,
// or another process owns the session.
func (r *Runner) kick(session string) error {
	r.mu.Lock()
	if r.active[session] || r.draining || r.ctx.Err() != nil {
		r.mu.Unlock()
		return nil
	}
	r.active[session] = true
	r.wg.Add(1) // under mu, so Close cannot wait between the check and the worker
	r.mu.Unlock()
	owned, err := r.adopt(session)
	if err != nil || !owned {
		if owned {
			r.store.Release(context.WithoutCancel(r.ctx), session)
		}
		r.mu.Lock()
		r.active[session] = false
		r.mu.Unlock()
		r.wg.Done()
		return err
	}
	go func() {
		defer r.wg.Done()
		r.work(session)
	}()
	return nil
}

// work runs the turns of one session while this runner owns it, then
// releases it so another process may take its next turn.
func (r *Runner) work(session string) {
	idle, err := r.serve(session)
	// The release comes before active is cleared, so a Submit that finds the
	// worker still active leaves its turn to the check below.
	r.store.Release(context.WithoutCancel(r.ctx), session)
	r.mu.Lock()
	r.active[session] = false
	r.mu.Unlock()
	switch {
	case errors.Is(err, ErrLocked):
		// The claim was lost, so another process owns the session now.
	case err != nil:
		r.fail(err)
	case idle && r.ctx.Err() == nil:
		// A turn stored after the last look, by a process that found the
		// session owned, would wait for the next poll. Look once more.
		_, ok, err := r.next(session)
		if err == nil && ok {
			err = r.kick(session)
		}
		if err != nil {
			r.fail(err)
		}
	}
}

// serve runs the unfinished turns of a session, oldest first. It reports
// idle when none is left.
func (r *Runner) serve(session string) (idle bool, err error) {
	for {
		r.mu.Lock()
		t, ok, err := r.next(session)
		stop := !ok || err != nil || r.ctx.Err() != nil || (r.draining && t.Status != Running)
		r.mu.Unlock()
		if stop {
			return !ok && err == nil, err
		}
		if err := r.run(t); err != nil {
			return false, err
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

// append stores e, then wakes the watchers of its session. It holds r.mu
// only to wake them, never around the store write.
func (r *Runner) append(ctx context.Context, e Event) error {
	if _, err := r.store.AppendEvent(ctx, e); err != nil {
		return err
	}
	r.mu.Lock()
	r.notify(e.Session)
	r.mu.Unlock()
	return nil
}

// broadcast wakes every Wait. The caller holds r.mu.
func (r *Runner) broadcast() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *Runner) finish(t Turn, status Status, msg string) error {
	r.steerMu.Lock()
	defer r.steerMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, err := r.store.Turn(context.WithoutCancel(r.ctx), t.RequestID)
	if err != nil {
		return err
	}
	if stored.Status == Cancelled {
		stored.Usage = t.Usage
		return r.store.SaveTurn(context.WithoutCancel(r.ctx), stored)
	}
	t.Status, t.Error = status, msg
	if err := r.store.SaveTurn(context.WithoutCancel(r.ctx), t); err != nil {
		return fmt.Errorf("faden: finish turn %s: %w", t.RequestID, err)
	}
	r.broadcast()
	return nil
}

// recordInput stores the turn's user message unless an earlier attempt did.
// The runner owns it, not the backend: the input is in the store from
// Submit on, so a crash before the first event cannot lose it, and a
// continuation prompt never enters the thread as something the user said.
// It comes after any handover event, so a backend rebuilding its history
// finds the handover before the request. It also marks which backend ran
// the turn, which catchUp needs even when the backend emits nothing.
func (r *Runner) recordInput(a *Attempt, h history) error {
	if h.hasInput {
		return nil
	}
	return a.Emit(r.ctx, Event{Kind: UserMessage, Text: attach.WithLines(a.turn.Input, a.turn.Files)})
}

// run runs one turn. A running turn already has its attempt raised by Open.
func (r *Runner) run(t Turn) error {
	r.mu.Lock()
	t, err := r.store.Turn(r.ctx, t.RequestID)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	if t.Status.Final() {
		r.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(r.ctx)
	r.turns[t.RequestID] = cancel
	defer func() { cancel(); r.mu.Lock(); delete(r.turns, t.RequestID); r.mu.Unlock() }()
	if t.Status == Pending {
		t.Status, t.Attempts = Running, 1
		if err := r.store.SaveTurn(r.ctx, t); err != nil {
			r.mu.Unlock()
			return fmt.Errorf("faden: start turn %s: %w", t.RequestID, err)
		}
	}
	r.mu.Unlock()
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
	a := &Attempt{runner: r, store: r.store, turn: t, cursor: session.Cursors[t.Backend], usage: append([]ModelUsage(nil), t.Usage...)}
	if r.prepare != nil {
		setup, err := r.prepare(ctx, t)
		if err != nil {
			if r.ctx.Err() != nil {
				return nil // closed: the turn stays running for the next Open
			}
			return r.finish(t, Failed, fmt.Sprintf("faden: prepare turn %s: %v", t.RequestID, err))
		}
		a.setup = setup
	}
	// One pass serves catch-up, continuation and the input check. catchUp can
	// emit only this turn's handover marker, which neither later step reads.
	continues := t.Attempts > 1 && !backend.ContinuesWithoutPrompt()
	h, err := r.readHistory(t, continues)
	if err != nil {
		return fmt.Errorf("faden: turn %s: %w", t.RequestID, err)
	}
	if err := r.catchUp(a, h); err != nil {
		return fmt.Errorf("faden: handover turn %s: %w", t.RequestID, err)
	}
	if continues {
		r.addOwnEvents(a, h.own)
	}
	if err := r.recordInput(a, h); err != nil {
		return fmt.Errorf("faden: record input of turn %s: %w", t.RequestID, err)
	}
	var runErr error
	if ctx.Err() == nil {
		runErr = backend.Run(ctx, send, a)
	}
	t.Usage = a.usage
	if r.ctx.Err() != nil {
		// Keep reported usage for the next attempt, while leaving the turn running.
		return r.finish(t, Running, "")
	}
	if runErr != nil {
		var quota *QuotaError
		var unavailable *UnavailableError
		switch {
		case errors.As(runErr, &quota):
			t.Failure, t.RetryAt, t.LimitType = FailureQuota, quota.ResetsAt, quota.LimitType
		case errors.As(runErr, &unavailable):
			t.Failure = FailureUnavailable
		}
		return r.finish(t, Failed, runErr.Error())
	}
	return r.finish(t, Done, "")
}

// addOwnEvents appends what earlier attempts of the turn stored to its
// catch-up, so a backend that only sees a continuation prompt still learns
// what the turn already did. A CLI's own session keeps little of a killed
// turn. own holds no handover marker: it is bookkeeping.
func (r *Runner) addOwnEvents(a *Attempt, own []Event) {
	if a.budget == 0 {
		a.budget = r.budget
	}
	a.catchUp = append(a.catchUp, own...)
}
