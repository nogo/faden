package faden_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/nogo/faden"
)

func TestNewAttemptFailsForUnknownSession(t *testing.T) {
	store := openStore(t, t.TempDir())
	_, err := faden.NewAttempt(t.Context(), store, faden.Turn{Session: "s1", RequestID: "r1", Backend: "a"})
	if !errors.Is(err, faden.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAttemptIsBoundToItsSessionAndBackend(t *testing.T) {
	store := openStore(t, t.TempDir())
	ctx := t.Context()
	turn, err := store.SubmitTurn(ctx, faden.Turn{Session: "s1", RequestID: "r1", Backend: "a"})
	if err != nil {
		t.Fatal(err)
	}
	store.SetCursor(ctx, "s1", "a", json.RawMessage(`{"n":1}`))
	store.SetCursor(ctx, "s1", "b", json.RawMessage(`{"n":2}`))
	a, err := faden.NewAttempt(ctx, store, turn)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Cursor()) != `{"n":1}` {
		t.Fatalf("cursor = %s, want backend a's", a.Cursor())
	}
	if err := a.SaveCursor(ctx, json.RawMessage(`{"n":3}`)); err != nil {
		t.Fatal(err)
	}
	sess, _ := store.Session(ctx, "s1")
	if string(a.Cursor()) != `{"n":3}` || string(sess.Cursors["a"]) != `{"n":3}` || string(sess.Cursors["b"]) != `{"n":2}` {
		t.Fatalf("after save: attempt %s, store %v", a.Cursor(), sess.Cursors)
	}
	if err := a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	events, err := faden.CollectEvents(a.Events(ctx, 0))
	if err != nil || len(events) != 1 || events[0].Session != "s1" || events[0].RequestID != "r1" || events[0].Time.IsZero() {
		t.Fatalf("events = %+v, %v", events, err)
	}
}
