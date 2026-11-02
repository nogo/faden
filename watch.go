package faden

import (
	"context"
	"fmt"
	"strings"
)

// FragmentKind identifies a transient answer fragment.
type FragmentKind string

const (
	FragmentText     FragmentKind = "text"
	FragmentThinking FragmentKind = "thinking"
)

// Fragment is a transient part of an answer. It is never stored.
type Fragment struct {
	RequestID string
	Kind      FragmentKind
	Text      string
}

// Update carries exactly one completed event or transient fragment.
type Update struct {
	Event    *Event
	Fragment *Fragment
}

const fragmentBytes = 4096
const fragmentSlots = 16

type watcher struct {
	session   string
	wake      chan struct{}
	fragments chan Fragment
}

// Watch replays events after after, then follows the session until ctx or the
// runner closes. Fragments are best effort; completed events are never skipped.
// A store read failure closes the channel. Watch reports an unknown session
// before starting. Direct writes to the store are outside the live contract.
func (r *Runner) Watch(ctx context.Context, session string, after int64) (<-chan Update, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return nil, fmt.Errorf("faden: watch: runner closed")
	}
	if _, err := r.store.Session(ctx, session); err != nil {
		return nil, fmt.Errorf("faden: watch %s: %w", session, err)
	}
	w := &watcher{session: session, wake: make(chan struct{}, 1), fragments: make(chan Fragment, fragmentSlots)}
	if r.watchers == nil {
		r.watchers = make(map[*watcher]struct{})
	}
	r.watchers[w] = struct{}{}
	out := make(chan Update)
	r.watchWG.Go(func() {
		defer close(out)
		defer func() { r.mu.Lock(); delete(r.watchers, w); r.mu.Unlock() }()
		readCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(r.ctx, cancel)
		defer stop()
		defer cancel()
		after = max(after, 0)
		for {
			e, err := r.store.NextEvent(readCtx, session, after)
			if err != nil {
				return
			}
			if e != nil {
				select {
				case out <- Update{Event: e}:
					after = e.Seq
				case <-readCtx.Done():
					return
				}
				continue
			}
			select {
			case <-readCtx.Done():
				return
			case <-w.wake:
			case f := <-w.fragments:
				// A fragment must never delay replay of a completed item.
				select {
				case out <- Update{Fragment: &f}:
				case <-readCtx.Done():
					return
				case <-w.wake:
				}
			}
		}
	})
	return out, nil
}

// notify wakes watchers after a durable append. The caller holds r.mu.
func (r *Runner) notify(session string) {
	for w := range r.watchers {
		if w.session == session {
			select {
			case w.wake <- struct{}{}:
			default:
			}
		}
	}
}

func (r *Runner) fragment(session string, f Fragment) {
	if len(f.Text) == 0 || len(f.Text) > fragmentBytes {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var copied bool
	for w := range r.watchers {
		if w.session != session || len(w.fragments) == cap(w.fragments) {
			continue
		}
		if !copied {
			f.Text = strings.Clone(f.Text)
			copied = true
		}
		select {
		case w.fragments <- f:
		default:
		}
	}
}
