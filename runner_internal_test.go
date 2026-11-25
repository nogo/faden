package faden

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// adoptStore lists one unfinished turn per session once, then nothing. Its
// claims fail for the sessions in locked. Every other Store method is unused.
type adoptStore struct {
	Store
	mu     sync.Mutex
	turns  []Turn
	locked map[string]bool
}

func (s *adoptStore) Unfinished(context.Context) ([]Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turns := s.turns
	s.turns = nil
	return turns, nil
}

func (s *adoptStore) Claim(_ context.Context, session string) error {
	if s.locked[session] {
		return ErrLocked
	}
	return nil
}

func (s *adoptStore) Release(context.Context, string) error { return nil }

func TestRunnerForgetsSessionsItNoLongerServes(t *testing.T) {
	store := &adoptStore{locked: map[string]bool{}}
	for i := range 100 {
		session := fmt.Sprint("s", i)
		store.turns = append(store.turns, Turn{RequestID: fmt.Sprint("r", i), Session: session, Progress: Progress{Status: Pending}})
		if i%2 == 0 {
			store.locked[session] = true
		}
	}
	r, err := Open(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if n := len(r.active); n != 0 {
		t.Fatalf("runner still tracks %d sessions after their workers ended", n)
	}
}
