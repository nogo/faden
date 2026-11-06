package faden_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nogo/faden"
)

type steeringFake struct {
	*fake
	steer func(context.Context, faden.Turn, string) error
}

func (b *steeringFake) Steer(ctx context.Context, turn faden.Turn, text string) error {
	return b.steer(ctx, turn, text)
}

func TestSteerStoresBeforeDeliveryAndSurvivesReopen(t *testing.T) {
	store := openStore(t, t.TempDir())
	started := make(chan struct{})
	b := &steeringFake{fake: &fake{run: func(ctx context.Context, _ faden.Turn, _ *faden.Attempt) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}}
	b.steer = func(ctx context.Context, turn faden.Turn, text string) error {
		events, err := faden.CollectEvents(store.Events(ctx, turn.Session, 0))
		if err != nil {
			return err
		}
		last := events[len(events)-1]
		if last.Kind != faden.UserMessage || last.RequestID != turn.RequestID || last.Text != text {
			t.Errorf("stored before delivery: %+v", events)
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
	if got, err := r.Steer(t.Context(), "running", "use blue"); err != nil || got.Status != faden.Running {
		t.Fatalf("Steer = %+v, %v", got, err)
	}
	if _, err := r.Submit(t.Context(), sub("s", "pending", "later")); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Steer(t.Context(), "pending", "wrong"); !errors.Is(err, faden.ErrNotRunning) || got.Status != faden.Pending {
		t.Fatalf("pending = %+v, %v", got, err)
	}
	r.Close()
	continued := &fake{run: func(ctx context.Context, turn faden.Turn, a *faden.Attempt) error {
		if turn.RequestID == "running" {
			events, err := faden.CollectEvents(store.Events(ctx, turn.Session, 0))
			if err != nil {
				return err
			}
			count := 0
			for _, e := range events {
				if e.Kind == faden.UserMessage && e.Text == "use blue" {
					count++
				}
			}
			if count != 1 {
				t.Errorf("steered messages after reopen = %d", count)
			}
		}
		return nil
	}}
	reopened, err := faden.Open(store, map[string]faden.Backend{"fake": continued})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	final := wait(t, reopened, "running")
	if got, err := reopened.Steer(t.Context(), "running", "wrong"); !errors.Is(err, faden.ErrNotRunning) || !reflect.DeepEqual(got, final) {
		t.Fatalf("final steer error = %v", err)
	}
	wait(t, reopened, "pending")
	events, err := faden.CollectEvents(store.Events(t.Context(), "s", 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Text == "wrong" {
			t.Fatalf("rejected message stored: %+v", e)
		}
	}
}

func TestSteerWithoutCapabilityStoresNothing(t *testing.T) {
	store := openStore(t, t.TempDir())
	started := make(chan struct{})
	b := &fake{run: func(ctx context.Context, _ faden.Turn, _ *faden.Attempt) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), sub("s", "r", "work")); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := r.Steer(t.Context(), "r", "wrong"); !errors.Is(err, faden.ErrCannotSteer) {
		t.Fatalf("Steer = %v", err)
	}
	events, err := faden.CollectEvents(store.Events(t.Context(), "s", 0))
	if err != nil || len(events) != 1 || events[0].Text != "work" {
		t.Fatalf("events = %+v, %v", events, err)
	}
}
