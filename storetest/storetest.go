// Package storetest is the conformance suite every faden.Store passes. Call
// Run from the store's own test.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nogo/faden"
)

// Storage is one empty place a store keeps its data.
type Storage struct {
	// Open opens a store on the storage. The test closes it before opening again.
	Open func() faden.Store
	// Tear leaves a half-written record at the end of the storage, as a
	// crash in the middle of a write does. Nil if the store cannot tear.
	Tear func()
	// InterruptCreate leaves an empty or torn first record for session.
	// Nil if session creation is atomic.
	InterruptCreate func(session string, torn bool)
}

// Run runs the suite. newStorage returns a fresh Storage for each test.
func Run(t *testing.T, newStorage func(t *testing.T) Storage) {
	ctx := context.Background()
	turn := func(id, session string) faden.Turn {
		return faden.Turn{RequestID: id, Session: session, Input: "in " + id, Backend: "a", Model: "m"}
	}

	t.Run("SubmitStoresPendingTurn", func(t *testing.T) {
		s := newStorage(t).Open()
		defer s.Close()
		got, err := s.SubmitTurn(ctx, turn("r1", "s1"))
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != faden.Pending || got.Attempts != 0 || got.Created.IsZero() {
			t.Fatalf("submitted turn = %+v", got)
		}
		sess, err := s.Session(ctx, "s1")
		if err != nil || sess.ID != "s1" {
			t.Fatalf("session = %+v, %v", sess, err)
		}
	})

	t.Run("RequestIDReuseReturnsExistingTurn", func(t *testing.T) {
		s := newStorage(t).Open()
		defer s.Close()
		first, _ := s.SubmitTurn(ctx, turn("r1", "s1"))
		again := turn("r1", "s2")
		again.Input = "other"
		got, err := s.SubmitTurn(ctx, again)
		if err != nil {
			t.Fatal(err)
		}
		if got.Session != "s1" || got.Input != first.Input {
			t.Fatalf("reused request id returned %+v, want %+v", got, first)
		}
		if _, err := s.Session(ctx, "s2"); !errors.Is(err, faden.ErrNotFound) {
			t.Fatalf("session s2 err = %v, want ErrNotFound", err)
		}
		unfinished, _ := s.Unfinished(ctx)
		if len(unfinished) != 1 {
			t.Fatalf("unfinished = %d turns, want 1", len(unfinished))
		}
	})

	t.Run("RequestIDIsUniquePerStoreNotPerSession", func(t *testing.T) {
		s := newStorage(t).Open()
		defer s.Close()
		s.SubmitTurn(ctx, turn("r1", "s1"))
		got, _ := s.SubmitTurn(ctx, turn("r1", "s2"))
		if got.Session != "s1" {
			t.Fatalf("session = %q, want s1", got.Session)
		}
	})

	t.Run("EventsKeepOrderAndSequencePerSession", func(t *testing.T) {
		s := newStorage(t).Open()
		defer s.Close()
		s.SubmitTurn(ctx, turn("r1", "s1"))
		s.SubmitTurn(ctx, turn("r2", "s2"))
		for _, text := range []string{"one", "two", "three"} {
			if _, err := s.AppendEvent(ctx, faden.Event{Session: "s1", RequestID: "r1", Kind: faden.AssistantMessage, Text: text}); err != nil {
				t.Fatal(err)
			}
		}
		other, _ := s.AppendEvent(ctx, faden.Event{Session: "s2", RequestID: "r2", Kind: faden.UserMessage, Text: "x"})
		if other.Seq != 1 {
			t.Fatalf("first event of s2 has seq %d, want 1", other.Seq)
		}
		got, err := s.Events(ctx, "s1", 0)
		if err != nil {
			t.Fatal(err)
		}
		for i, text := range []string{"one", "two", "three"} {
			if got[i].Text != text || got[i].Seq != int64(i+1) {
				t.Fatalf("event %d = %+v", i, got[i])
			}
		}
		rest, _ := s.Events(ctx, "s1", 2)
		if len(rest) != 1 || rest[0].Text != "three" {
			t.Fatalf("events after 2 = %+v", rest)
		}
	})

	t.Run("EventKeepsNativePayload", func(t *testing.T) {
		s := newStorage(t).Open()
		defer s.Close()
		s.SubmitTurn(ctx, turn("r1", "s1"))
		native := json.RawMessage(`{"type":"tool_use","id":"t1"}`)
		input := json.RawMessage(`{"cmd":"ls"}`)
		s.AppendEvent(ctx, faden.Event{Session: "s1", RequestID: "r1", Kind: faden.ToolCall, Tool: "bash", Action: "run", Input: input, Output: "o", Failed: true, Native: native})
		got, _ := s.Events(ctx, "s1", 0)
		if len(got) != 1 || string(got[0].Native) != string(native) || string(got[0].Input) != string(input) ||
			got[0].Tool != "bash" || got[0].Action != "run" || got[0].Output != "o" || !got[0].Failed {
			t.Fatalf("event = %+v", got)
		}
	})

	t.Run("LargeEventSurvivesReopen", func(t *testing.T) {
		st := newStorage(t)
		s := st.Open()
		s.SubmitTurn(ctx, turn("r1", "s1"))
		large := strings.Repeat("o", 1<<20)
		s.AppendEvent(ctx, faden.Event{Session: "s1", RequestID: "r1", Kind: faden.ToolResult, Output: large})
		s.SetCursor(ctx, "s1", "a", json.RawMessage(`{"n":1}`))
		s.AppendEvent(ctx, faden.Event{Session: "s1", RequestID: "r1", Kind: faden.AssistantMessage, Text: "after"})
		s.Close()

		s = st.Open()
		defer s.Close()
		events, err := s.Events(ctx, "s1", 0)
		if err != nil || len(events) != 2 || events[0].Output != large || events[1].Text != "after" || events[1].Seq != 2 {
			t.Fatalf("events after reopen: %d, %v", len(events), err)
		}
	})

	t.Run("OneCursorPerBackend", func(t *testing.T) {
		s := newStorage(t).Open()
		defer s.Close()
		s.SubmitTurn(ctx, turn("r1", "s1"))
		s.SetCursor(ctx, "s1", "a", json.RawMessage(`{"n":1}`))
		s.SetCursor(ctx, "s1", "b", json.RawMessage(`{"n":2}`))
		s.SetCursor(ctx, "s1", "a", json.RawMessage(`{"n":3}`))
		sess, _ := s.Session(ctx, "s1")
		if len(sess.Cursors) != 2 || string(sess.Cursors["a"]) != `{"n":3}` || string(sess.Cursors["b"]) != `{"n":2}` {
			t.Fatalf("cursors = %v", sess.Cursors)
		}
	})

	t.Run("LaterSubmitKeepsSession", func(t *testing.T) {
		st := newStorage(t)
		s := st.Open()
		s.SubmitTurn(ctx, turn("r1", "s1"))
		s.SetCursor(ctx, "s1", "a", json.RawMessage(`{"n":1}`))
		before, _ := s.Session(ctx, "s1")
		other := turn("r2", "s1")
		other.Backend = "b"
		if _, err := s.SubmitTurn(ctx, other); err != nil {
			t.Fatal(err)
		}
		s.Close()

		s = st.Open()
		defer s.Close()
		sess, err := s.Session(ctx, "s1")
		if err != nil || string(sess.Cursors["a"]) != `{"n":1}` || !sess.Created.Equal(before.Created) {
			t.Fatalf("session after a later submit = %+v, %v; created before %v", sess, err, before.Created)
		}
	})

	t.Run("UnfinishedListsPendingAndRunningOldestFirst", func(t *testing.T) {
		s := newStorage(t).Open()
		defer s.Close()
		for _, id := range []string{"r1", "r2", "r3", "r4"} {
			s.SubmitTurn(ctx, turn(id, "s1"))
		}
		r1, _ := s.Turn(ctx, "r1")
		r1.Status = faden.Done
		s.SaveTurn(ctx, r1)
		r2, _ := s.Turn(ctx, "r2")
		r2.Status, r2.Attempts = faden.Running, 1
		s.SaveTurn(ctx, r2)
		r4, _ := s.Turn(ctx, "r4")
		r4.Status, r4.Error = faden.Failed, "boom"
		s.SaveTurn(ctx, r4)
		got, _ := s.Unfinished(ctx)
		if len(got) != 2 || got[0].RequestID != "r2" || got[0].Status != faden.Running || got[0].Attempts != 1 || got[1].RequestID != "r3" {
			t.Fatalf("unfinished = %+v", got)
		}
	})

	t.Run("MissingThingsAreNotFound", func(t *testing.T) {
		s := newStorage(t).Open()
		defer s.Close()
		if _, err := s.Turn(ctx, "nope"); !errors.Is(err, faden.ErrNotFound) {
			t.Fatalf("Turn err = %v", err)
		}
		if _, err := s.Session(ctx, "nope"); !errors.Is(err, faden.ErrNotFound) {
			t.Fatalf("Session err = %v", err)
		}
		if err := s.SaveTurn(ctx, turn("nope", "s1")); !errors.Is(err, faden.ErrNotFound) {
			t.Fatalf("SaveTurn err = %v", err)
		}
		if _, err := s.AppendEvent(ctx, faden.Event{Session: "nope"}); !errors.Is(err, faden.ErrNotFound) {
			t.Fatalf("AppendEvent err = %v", err)
		}
	})

	t.Run("EverythingSurvivesReopen", func(t *testing.T) {
		st := newStorage(t)
		s := st.Open()
		s.SubmitTurn(ctx, turn("r1", "s1"))
		r1, _ := s.Turn(ctx, "r1")
		r1.Status, r1.Attempts = faden.Running, 2
		r1.Usage = []faden.ModelUsage{{Model: "invented", InputTokens: 90, OutputTokens: 12, CacheReadTokens: 30, CacheWriteTokens: 20, CostUSD: 0.125}}
		s.SaveTurn(ctx, r1)
		s.AppendEvent(ctx, faden.Event{Session: "s1", RequestID: "r1", Kind: faden.UserMessage, Text: "hi"})
		s.SetCursor(ctx, "s1", "a", json.RawMessage(`{"n":1}`))
		s.Close()

		s = st.Open()
		defer s.Close()
		got, _ := s.Turn(ctx, "r1")
		if got.Status != faden.Running || got.Attempts != 2 || !reflect.DeepEqual(got.Usage, r1.Usage) {
			t.Fatalf("turn = %+v", got)
		}
		events, _ := s.Events(ctx, "s1", 0)
		sess, _ := s.Session(ctx, "s1")
		if len(events) != 1 || events[0].Text != "hi" || string(sess.Cursors["a"]) != `{"n":1}` {
			t.Fatalf("events = %+v, session = %+v", events, sess)
		}
		next, _ := s.AppendEvent(ctx, faden.Event{Session: "s1", RequestID: "r1", Kind: faden.AssistantMessage})
		if next.Seq != 2 {
			t.Fatalf("seq after reopen = %d, want 2", next.Seq)
		}
	})

	for _, torn := range []bool{false, true} {
		name := "EmptySessionIsAbsentAtOpen"
		if torn {
			name = "TornFirstRecordIsAbsentAtOpen"
		}
		t.Run(name, func(t *testing.T) {
			st := newStorage(t)
			if st.InterruptCreate == nil {
				t.Skip("session creation is atomic")
			}
			s := st.Open()
			want, err := s.SubmitTurn(ctx, turn("r1", "s1"))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			st.InterruptCreate("s", torn)
			s = st.Open()
			defer s.Close()
			if _, err := s.Session(ctx, "s"); !errors.Is(err, faden.ErrNotFound) {
				t.Fatalf("interrupted session err = %v, want ErrNotFound", err)
			}
			if sess, err := s.Session(ctx, "s1"); err != nil || sess.ID != "s1" {
				t.Fatalf("valid session = %+v, %v", sess, err)
			}
			if got, err := s.Turn(ctx, "r1"); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("valid turn = %+v, %v; want %+v", got, err, want)
			}
			if _, err := s.SubmitTurn(ctx, turn("r2", "s")); err != nil {
				t.Fatalf("create interrupted session again: %v", err)
			}
		})
	}

	t.Run("TornLastWriteIsCutAtOpen", func(t *testing.T) {
		st := newStorage(t)
		if st.Tear == nil {
			t.Skip("store cannot tear")
		}
		s := st.Open()
		s.SubmitTurn(ctx, turn("r1", "s1"))
		s.AppendEvent(ctx, faden.Event{Session: "s1", RequestID: "r1", Kind: faden.UserMessage, Text: "kept"})
		s.Close()
		st.Tear()

		s = st.Open()
		events, _ := s.Events(ctx, "s1", 0)
		if len(events) != 1 || events[0].Text != "kept" {
			t.Fatalf("events after tear = %+v", events)
		}
		if _, err := s.AppendEvent(ctx, faden.Event{Session: "s1", RequestID: "r1", Text: "after"}); err != nil {
			t.Fatal(err)
		}
		s.Close()

		s = st.Open()
		defer s.Close()
		events, _ = s.Events(ctx, "s1", 0)
		if len(events) != 2 || events[1].Text != "after" || events[1].Seq != 2 {
			t.Fatalf("events after tear and write = %+v", events)
		}
	})
}
