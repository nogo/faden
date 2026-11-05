package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nogo/faden"
)

// claims runs the ownership contract: one owner per session, fenced writes,
// takeover of a lost claim, and writes that other stores on the storage see.
func claims(t *testing.T, newStorage func(t *testing.T) Storage) {
	ctx := context.Background()
	// pair opens two stores on one storage and submits a turn of session "s".
	pair := func(t *testing.T) (a, b faden.Store) {
		t.Helper()
		st := newStorage(t)
		if !st.Shared {
			t.Skip("one store per storage")
		}
		a, b = st.Open(), st.Open()
		t.Cleanup(func() { a.Close(); b.Close() })
		if _, err := a.SubmitTurn(ctx, faden.Turn{RequestID: "r1", Session: "s", Input: "in", Backend: "a"}); err != nil {
			t.Fatal(err)
		}
		return a, b
	}
	event := func(text string) faden.Event {
		return faden.Event{Session: "s", RequestID: "r1", Kind: faden.AssistantMessage, Text: text}
	}
	// takeOver polls until b can claim s, because a lost claim of a store
	// that hangs runs out on the clock.
	takeOver := func(t *testing.T, b faden.Store) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := b.Claim(ctx, "s")
			if err == nil {
				return
			}
			if !errors.Is(err, faden.ErrLocked) || time.Now().After(deadline) {
				t.Fatalf("takeover claim: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	t.Run("ClaimOfLiveSessionIsLocked", func(t *testing.T) {
		a, b := pair(t)
		if err := a.Claim(ctx, "s"); err != nil {
			t.Fatal(err)
		}
		if err := a.Claim(ctx, "s"); err != nil {
			t.Fatalf("claim again by the owner: %v", err)
		}
		if err := b.Claim(ctx, "s"); !errors.Is(err, faden.ErrLocked) {
			t.Fatalf("second claim err = %v, want ErrLocked", err)
		}
		if err := b.Claim(ctx, "unknown"); !errors.Is(err, faden.ErrNotFound) {
			t.Fatalf("claim of unknown session err = %v, want ErrNotFound", err)
		}
	})

	t.Run("ReleaseLetsAnotherStoreClaim", func(t *testing.T) {
		a, b := pair(t)
		a.Claim(ctx, "s")
		if err := a.Release(ctx, "s"); err != nil {
			t.Fatal(err)
		}
		if err := b.Claim(ctx, "s"); err != nil {
			t.Fatalf("claim after release: %v", err)
		}
		if _, err := a.AppendEvent(ctx, event("late")); !errors.Is(err, faden.ErrLocked) {
			t.Fatalf("write by the former owner err = %v, want ErrLocked", err)
		}
		if err := a.Release(ctx, "s"); err != nil {
			t.Fatalf("release without a claim: %v", err)
		}
		if err := b.Claim(ctx, "s"); err != nil {
			t.Fatalf("release without a claim took the claim of another: %v", err)
		}
	})

	t.Run("WritesOfANonOwnerAreFenced", func(t *testing.T) {
		a, b := pair(t)
		a.Claim(ctx, "s")
		turn, _ := b.Turn(ctx, "r1")
		turn.Status = faden.Running
		if err := b.SaveTurn(ctx, turn); !errors.Is(err, faden.ErrLocked) {
			t.Fatalf("SaveTurn err = %v, want ErrLocked", err)
		}
		if _, err := b.AppendEvent(ctx, event("x")); !errors.Is(err, faden.ErrLocked) {
			t.Fatalf("AppendEvent err = %v, want ErrLocked", err)
		}
		if err := b.SetCursor(ctx, "s", "a", json.RawMessage(`{"n":1}`)); !errors.Is(err, faden.ErrLocked) {
			t.Fatalf("SetCursor err = %v, want ErrLocked", err)
		}
		got, _ := a.Turn(ctx, "r1")
		events, _ := faden.CollectEvents(a.Events(ctx, "s", 0))
		sess, _ := a.Session(ctx, "s")
		if got.Status != faden.Pending || len(events) != 0 || len(sess.Cursors) != 0 {
			t.Fatalf("a fenced write changed the session: %+v, %+v, %+v", got, events, sess)
		}
		if _, err := a.AppendEvent(ctx, event("mine")); err != nil {
			t.Fatalf("owner write: %v", err)
		}
	})

	t.Run("UnownedSessionAcceptsWritesFromAnyStore", func(t *testing.T) {
		a, b := pair(t)
		if _, err := a.AppendEvent(ctx, event("one")); err != nil {
			t.Fatal(err)
		}
		got, err := b.AppendEvent(ctx, event("two"))
		if err != nil || got.Seq != 2 {
			t.Fatalf("second store append = %+v, %v; want seq 2", got, err)
		}
	})

	t.Run("OtherStoresSeeWhatTheOwnerWrote", func(t *testing.T) {
		a, b := pair(t)
		a.Claim(ctx, "s")
		// b reads before and after: it must not keep a stale copy.
		if events, _ := faden.CollectEvents(b.Events(ctx, "s", 0)); len(events) != 0 {
			t.Fatalf("events = %+v", events)
		}
		turn, _ := a.Turn(ctx, "r1")
		turn.Status, turn.Attempts = faden.Running, 1
		a.SaveTurn(ctx, turn)
		a.AppendEvent(ctx, event("seen"))
		a.SetCursor(ctx, "s", "a", json.RawMessage(`{"n":2}`))
		events, err := faden.CollectEvents(b.Events(ctx, "s", 0))
		if err != nil || len(events) != 1 || events[0].Text != "seen" {
			t.Fatalf("events = %+v, %v", events, err)
		}
		if next, err := b.NextEvent(ctx, "s", 0); err != nil || next == nil || next.Text != "seen" {
			t.Fatalf("next event = %+v, %v", next, err)
		}
		if got, err := b.Turn(ctx, "r1"); err != nil || got.Status != faden.Running || got.Attempts != 1 {
			t.Fatalf("turn = %+v, %v", got, err)
		}
		if sess, err := b.Session(ctx, "s"); err != nil || string(sess.Cursors["a"]) != `{"n":2}` {
			t.Fatalf("session = %+v, %v", sess, err)
		}
		if left, err := b.Unfinished(ctx); err != nil || len(left) != 1 || left[0].Status != faden.Running {
			t.Fatalf("unfinished = %+v, %v", left, err)
		}
	})

	t.Run("TurnForAnOwnedSessionIsStoredForItsOwner", func(t *testing.T) {
		a, b := pair(t)
		a.Claim(ctx, "s")
		if _, err := b.SubmitTurn(ctx, faden.Turn{RequestID: "r2", Session: "s", Input: "more", Backend: "a"}); err != nil {
			t.Fatalf("submit to an owned session: %v", err)
		}
		left, err := a.Unfinished(ctx)
		if err != nil || len(left) != 2 {
			t.Fatalf("owner unfinished = %+v, %v", left, err)
		}
		if _, err := a.AppendEvent(ctx, event("after")); err != nil {
			t.Fatalf("owner write after a foreign submit: %v", err)
		}
		events, err := faden.CollectEvents(b.Events(ctx, "s", 0))
		if err != nil || len(events) != 1 || events[0].Seq != 1 {
			t.Fatalf("events = %+v, %v", events, err)
		}
	})

	t.Run("RequestIDIsUniqueAcrossStores", func(t *testing.T) {
		a, b := pair(t)
		got, err := b.SubmitTurn(ctx, faden.Turn{RequestID: "r1", Session: "other", Input: "dup", Backend: "a"})
		if err != nil || got.Session != "s" || got.Input != "in" {
			t.Fatalf("submit of a known request id = %+v, %v", got, err)
		}
		if _, err := a.Session(ctx, "other"); !errors.Is(err, faden.ErrNotFound) {
			t.Fatalf("session other err = %v, want ErrNotFound", err)
		}
		if left, _ := b.Unfinished(ctx); len(left) != 1 {
			t.Fatalf("unfinished = %+v, want 1 turn", left)
		}
	})

	t.Run("ClaimOfADeadOwnerCanBeTakenOver", func(t *testing.T) {
		st := newStorage(t)
		if !st.Shared {
			t.Skip("one store per storage")
		}
		a := st.Open()
		if st.Lapse != nil {
			a = st.Lapse()
		}
		b := st.Open()
		t.Cleanup(func() { b.Close() })
		a.SubmitTurn(ctx, faden.Turn{RequestID: "r1", Session: "s", Backend: "a"})
		if err := a.Claim(ctx, "s"); err != nil {
			t.Fatal(err)
		}
		if st.Lapse == nil {
			a.Close() // the owner dies; its locks go with it
		} else {
			t.Cleanup(func() { a.Close() })
		}
		takeOver(t, b)
		if _, err := b.AppendEvent(ctx, event("new owner")); err != nil {
			t.Fatalf("write by the new owner: %v", err)
		}
	})

	t.Run("WriteWithALostClaimFails", func(t *testing.T) {
		st := newStorage(t)
		if !st.Shared || st.Lapse == nil {
			t.Skip("a claim ends only when its owner dies")
		}
		a, b := st.Lapse(), st.Open()
		t.Cleanup(func() { a.Close(); b.Close() })
		a.SubmitTurn(ctx, faden.Turn{RequestID: "r1", Session: "s", Backend: "a"})
		if err := a.Claim(ctx, "s"); err != nil {
			t.Fatal(err)
		}
		takeOver(t, b)
		turn, _ := a.Turn(ctx, "r1")
		turn.Status = faden.Running
		if err := a.SaveTurn(ctx, turn); !errors.Is(err, faden.ErrLocked) {
			t.Fatalf("SaveTurn with a lost claim err = %v, want ErrLocked", err)
		}
		if _, err := a.AppendEvent(ctx, event("stale")); !errors.Is(err, faden.ErrLocked) {
			t.Fatalf("AppendEvent with a lost claim err = %v, want ErrLocked", err)
		}
		if err := a.SetCursor(ctx, "s", "a", json.RawMessage(`{}`)); !errors.Is(err, faden.ErrLocked) {
			t.Fatalf("SetCursor with a lost claim err = %v, want ErrLocked", err)
		}
		if got, _ := b.Turn(ctx, "r1"); got.Status != faden.Pending {
			t.Fatalf("a stale write changed the turn: %+v", got)
		}
	})
}
