package faden_test

import (
	"context"
	"testing"

	"github.com/nogo/faden"
)

type steeringFake struct {
	*fake
	steer func(ctx context.Context, t faden.Turn, text string, store func() error) error
}

func (b *steeringFake) Steer(ctx context.Context, t faden.Turn, text string, store func() error) error {
	return b.steer(ctx, t, text, store)
}

// deliver is a steer that stores, then delivers.
func deliver(_ context.Context, _ faden.Turn, _ string, store func() error) error { return store() }

func steered(session, id, input string) faden.Submission {
	s := sub(session, id, input)
	s.Steer = true
	return s
}

// blocking runs every turn until it is cancelled, and closes started once.
func blocking(started chan struct{}) *fake {
	return &fake{run: func(ctx context.Context, _ faden.Turn, _ *faden.Attempt) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-ctx.Done()
		return ctx.Err()
	}}
}

func userMessages(t *testing.T, store faden.Store, session string) []faden.Event {
	t.Helper()
	var out []faden.Event
	for e, err := range store.Events(t.Context(), session, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == faden.UserMessage {
			out = append(out, e)
		}
	}
	return out
}

func TestSteerJoinsRunningTurnOnceAndSurvivesReopen(t *testing.T) {
	store := openStore(t, t.TempDir())
	started := make(chan struct{})
	b := &steeringFake{fake: blocking(started)}
	b.steer = func(ctx context.Context, turn faden.Turn, text string, store2 func() error) error {
		if err := store2(); err != nil {
			return err
		}
		msgs := userMessages(t, store, turn.Session)
		if last := msgs[len(msgs)-1]; last.RequestID != turn.RequestID || last.Text != text {
			t.Errorf("not stored before delivery: %+v", msgs)
		}
		return nil
	}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), sub("s", "running", "work")); err != nil {
		t.Fatal(err)
	}
	<-started
	got, err := r.Submit(t.Context(), steered("s", "blue", "use blue"))
	if err != nil || got.Status != faden.Joined || got.Joined != "running" || got.Input != "use blue" {
		t.Fatalf("Submit = %+v, %v", got, err)
	}
	again, err := r.Submit(t.Context(), steered("s", "blue", "use blue"))
	if err != nil || again.Status != faden.Joined {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if n := len(userMessages(t, store, "s")); n != 2 {
		t.Fatalf("%d user messages after a retry, want 2", n)
	}
	if done, err := r.Wait(t.Context(), "blue"); err != nil || done.Status != faden.Joined {
		t.Fatalf("Wait = %+v, %v", done, err)
	}
	r.Close()

	continued := &fake{run: func(context.Context, faden.Turn, *faden.Attempt) error { return nil }}
	reopened, err := faden.Open(store, map[string]faden.Backend{"fake": continued})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	wait(t, reopened, "running")
	if got, err := store.Turn(t.Context(), "blue"); err != nil || got.Status != faden.Joined {
		t.Fatalf("joined after reopen = %+v, %v", got, err)
	}
	msgs := userMessages(t, store, "s")
	if len(msgs) != 2 || msgs[1].Text != "use blue" || msgs[1].RequestID != "running" {
		t.Fatalf("user messages = %+v", msgs)
	}
}

// Every case where the input cannot join queues it as a turn, and stores no
// event before that turn runs.
func TestSteerQueuesWhenTheRunningTurnCannotTakeIt(t *testing.T) {
	refuse := func(context.Context, faden.Turn, string, func() error) error { return faden.ErrNotRunning }
	cases := []struct {
		name  string
		steer func(context.Context, faden.Turn, string, func() error) error // nil: the backend cannot steer
		edit  func(*faden.Submission)
	}{
		{name: "input closed", steer: refuse},
		{name: "backend cannot steer"},
		{name: "other model", steer: deliver, edit: func(s *faden.Submission) { s.Model = "other" }},
		{name: "files", steer: deliver, edit: func(s *faden.Submission) { s.Files = []string{"a.txt"} }},
		{name: "context", steer: deliver, edit: func(s *faden.Submission) { s.Context = "rules" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := openStore(t, t.TempDir())
			started := make(chan struct{})
			var b faden.Backend = blocking(started)
			if c.steer != nil {
				b = &steeringFake{fake: blocking(started), steer: c.steer}
			}
			r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if _, err := r.Submit(t.Context(), sub("s", "running", "work")); err != nil {
				t.Fatal(err)
			}
			<-started
			s := steered("s", "next", "use blue")
			if c.edit != nil {
				c.edit(&s)
			}
			got, err := r.Submit(t.Context(), s)
			if err != nil || got.Status != faden.Pending || got.Joined != "" {
				t.Fatalf("Submit = %+v, %v", got, err)
			}
			if n := len(userMessages(t, store, "s")); n != 1 {
				t.Fatalf("%d user messages, want 1", n)
			}
		})
	}
}

func TestSteerWithoutRunningTurnQueues(t *testing.T) {
	store := openStore(t, t.TempDir())
	b := &steeringFake{fake: &fake{run: func(context.Context, faden.Turn, *faden.Attempt) error { return nil }}, steer: deliver}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := r.Submit(t.Context(), steered("s", "first", "hello"))
	if err != nil || got.Status != faden.Pending {
		t.Fatalf("Submit = %+v, %v", got, err)
	}
	if done := wait(t, r, "first"); done.Status != faden.Done {
		t.Fatalf("turn = %+v", done)
	}
}
