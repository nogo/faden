// Package memstore is a faden.Store that keeps sessions, turns, events and
// cursors in process memory. Reach for it when a job needs faden's backends
// but not durability: a session title, a one-off question, a test.
//
// Everything is lost when the process ends, so a restart finds no turn to
// continue. There is no lock and a claim is always granted; one process owns its memory.
//
// The store holds every event of every session until it is dropped. It suits
// a runner that lives for one job. A host that runs for months uses jsonl or
// sqlite.
package memstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nogo/faden"
)

type session struct {
	meta   faden.Session
	events []faden.Event // event n is at events[n-1]
}

// Store implements faden.Store.
type Store struct {
	mu       sync.Mutex // guards everything below
	sessions map[string]*session
	turns    map[string]faden.Turn // by request id
}

// New returns an empty store.
func New() *Store {
	return &Store{sessions: map[string]*session{}, turns: map[string]faden.Turn{}}
}

// copyTurn returns t with its own slices, so a caller cannot change the stored turn.
func copyTurn(t faden.Turn) faden.Turn {
	t.Usage = slices.Clone(t.Usage)
	t.Files = slices.Clone(t.Files)
	return t
}

// SubmitTurn implements faden.Store.
func (s *Store) SubmitTurn(_ context.Context, t faden.Turn) (faden.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if known, ok := s.turns[t.RequestID]; ok {
		return copyTurn(known), nil
	}
	if t.RequestID == "" || t.Session == "" {
		return faden.Turn{}, errors.New("memstore: submit: request id and session are required")
	}
	t.Progress = faden.Progress{Status: faden.Pending}
	t.Created = time.Now().UTC()
	t = copyTurn(t)
	sess, ok := s.sessions[t.Session]
	if !ok {
		sess = &session{meta: faden.Session{ID: t.Session, Created: t.Created}}
		s.sessions[t.Session] = sess
	}
	s.turns[t.RequestID] = t
	return copyTurn(t), nil
}

// SaveTurn implements faden.Store.
func (s *Store) SaveTurn(_ context.Context, t faden.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	known, ok := s.turns[t.RequestID]
	if !ok {
		return fmt.Errorf("memstore: save turn %s: %w", t.RequestID, faden.ErrNotFound)
	}
	known.Progress = t.Progress
	known.Usage = slices.Clone(t.Usage) // the caller keeps its own slice
	s.turns[t.RequestID] = known
	return nil
}

// Turn implements faden.Store.
func (s *Store) Turn(_ context.Context, requestID string) (faden.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.turns[requestID]
	if !ok {
		return faden.Turn{}, fmt.Errorf("memstore: turn %s: %w", requestID, faden.ErrNotFound)
	}
	return copyTurn(t), nil
}

// Unfinished implements faden.Store.
func (s *Store) Unfinished(_ context.Context) ([]faden.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []faden.Turn
	for _, t := range s.turns {
		if !t.Status.Final() {
			out = append(out, copyTurn(t))
		}
	}
	slices.SortFunc(out, func(a, b faden.Turn) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.RequestID, b.RequestID)
	})
	return out, nil
}

// Session implements faden.Store.
func (s *Store) Session(_ context.Context, id string) (faden.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return faden.Session{}, fmt.Errorf("memstore: session %s: %w", id, faden.ErrNotFound)
	}
	out := sess.meta
	out.Cursors = maps.Clone(sess.meta.Cursors)
	return out, nil
}

// copyEvent returns e with its own buffers, so callers cannot change stored events.
func copyEvent(e faden.Event) faden.Event {
	e.Input = slices.Clone(e.Input)
	e.Native = slices.Clone(e.Native)
	return e
}

// AppendEvent implements faden.Store.
func (s *Store) AppendEvent(_ context.Context, e faden.Event) (faden.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[e.Session]
	if !ok {
		return faden.Event{}, fmt.Errorf("memstore: append event: session %s: %w", e.Session, faden.ErrNotFound)
	}
	e.Seq = int64(len(sess.events)) + 1
	e = copyEvent(e)
	sess.events = append(sess.events, e)
	return copyEvent(e), nil
}

// Events implements faden.Store. Stored events are never changed, so it
// takes the lock only to find them.
func (s *Store) Events(_ context.Context, id string, after int64) iter.Seq2[faden.Event, error] {
	return func(yield func(faden.Event, error) bool) {
		s.mu.Lock()
		sess, ok := s.sessions[id]
		var events []faden.Event
		if ok && max(after, 0) < int64(len(sess.events)) {
			events = sess.events[max(after, 0):len(sess.events):len(sess.events)]
		}
		s.mu.Unlock()
		if !ok {
			yield(faden.Event{}, fmt.Errorf("memstore: events: session %s: %w", id, faden.ErrNotFound))
			return
		}
		for _, e := range events {
			if !yield(copyEvent(e), nil) {
				return
			}
		}
	}
}

// NextEvent implements faden.Store.
func (s *Store) NextEvent(ctx context.Context, id string, after int64) (*faden.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("memstore: next event: session %s: %w", id, faden.ErrNotFound)
	}
	after = max(after, 0)
	if after >= int64(len(sess.events)) {
		return nil, nil
	}
	e := copyEvent(sess.events[after])
	return &e, nil
}

// SetCursor implements faden.Store.
func (s *Store) SetCursor(_ context.Context, id, backend string, cursor json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("memstore: set cursor: session %s: %w", id, faden.ErrNotFound)
	}
	if sess.meta.Cursors == nil {
		sess.meta.Cursors = map[string]json.RawMessage{}
	}
	sess.meta.Cursors[backend] = cursor
	return nil
}

// Claim does nothing but check that the session exists: one process owns the
// memory, so no other owner can hold it.
func (s *Store) Claim(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return fmt.Errorf("memstore: claim: session %s: %w", id, faden.ErrNotFound)
	}
	return nil
}

// Release does nothing.
func (s *Store) Release(context.Context, string) error { return nil }

// Close does nothing: the memory lives as long as the Store value, so the
// store can be used again after Close.
func (s *Store) Close() error { return nil }

var _ faden.Store = (*Store)(nil)
