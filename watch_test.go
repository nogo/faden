package faden_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nogo/faden"
)

func TestWatchReplayLiveAndSlowReader(t *testing.T) {
	store := openStore(t, t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	reached := make(chan *faden.Attempt)
	proceed := make(chan struct{})
	b := &fake{run: func(ctx context.Context, turn faden.Turn, a *faden.Attempt) error {
		if err := a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: "before"}); err != nil {
			return err
		}
		reached <- a
		select {
		case <-proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
		for i := range 100 {
			a.EmitFragment(faden.Fragment{Kind: faden.FragmentText, Text: "unstored"})
			if err := a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: fmt.Sprint(i)}); err != nil {
				return err
			}
		}
		return nil
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(ctx, sub("s", "r", "hello")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	updates, err := r.Watch(ctx, "s", 1)
	if err != nil {
		t.Fatal(err)
	}
	close(proceed)
	turn, err := r.Wait(ctx, "r")
	if err != nil || turn.Status != faden.Done {
		t.Fatalf("turn: %+v, %v", turn, err)
	}
	// The turn has completed without a single read from the watcher.
	events, err := store.Events(ctx, "s", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range events {
		for {
			select {
			case u := <-updates:
				if u.Event == nil {
					continue
				}
				if u.Event.Seq != want.Seq || u.Event.Text != want.Text {
					t.Fatalf("got %+v, want %+v", u.Event, want)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			break
		}
	}
	if len(events) != 101 {
		t.Fatalf("stored %d events; fragments entered the store", len(events))
	}
	cancel()
	for range updates {
	}
}

func TestWatchClosesWithRunnerAndRejectsMissingSession(t *testing.T) {
	store := openStore(t, t.TempDir())
	store.SubmitTurn(t.Context(), faden.Turn{Session: "s", RequestID: "r"})
	r, err := faden.Open(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Watch(t.Context(), "missing", 0); err == nil {
		t.Fatal("missing session accepted")
	}
	updates, err := r.Watch(t.Context(), "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	for range updates {
	}
}
