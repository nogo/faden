package faden_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
)

func TestContextStaysOutsideThread(t *testing.T) {
	store := openStore(t, t.TempDir())
	b := &fake{run: func(_ context.Context, turn faden.Turn, _ *faden.Attempt) error {
		if turn.Context != "Vault rules." || turn.Input != "My question" {
			t.Errorf("sent = %+v", turn)
		}
		return nil
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), faden.Submission{Session: "s1", RequestID: "r1", Backend: "fake", Input: "My question", Context: "Vault rules."}); err != nil {
		t.Fatal(err)
	}
	wait(t, r, "r1")
	events, err := faden.CollectEvents(store.Events(t.Context(), "s1", 0))
	if err != nil {
		t.Fatal(err)
	}
	var users int
	for _, e := range events {
		if e.Kind == faden.UserMessage {
			users++
			if e.Text != "My question" {
				t.Fatalf("user = %q", e.Text)
			}
		}
	}
	if users != 1 {
		t.Fatalf("user messages = %d", users)
	}
	text, err := handover.ReadThread(t.Context(), store, "s1", json.RawMessage(`{"from":1,"to":100}`))
	if err != nil || strings.Contains(text, "Vault rules.") {
		t.Fatalf("read_thread = %q, %v", text, err)
	}
	for _, item := range handover.Select(events, "next", 100) {
		if strings.Contains(item.Text, "Vault rules.") {
			t.Fatalf("handover = %+v", item)
		}
	}
}

func TestContextDoesNotConsumeHandoverBudget(t *testing.T) {
	store := openStore(t, t.TempDir())
	b := &fake{run: func(ctx context.Context, turn faden.Turn, a *faden.Attempt) error {
		if turn.Backend == "first" {
			return a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: "Previous answer"})
		}
		without := turn
		without.Context = ""
		plain := handover.Prompt(without, a)
		prompt := handover.Prompt(turn, a)
		if !strings.Contains(plain, "Previous answer") {
			t.Fatalf("missing answer: %q", plain)
		}
		want := strings.TrimSuffix(plain, turn.Input) + turn.Context + "\n\n" + turn.Input
		if prompt != want {
			t.Fatalf("context changed selection: %q", prompt)
		}
		return nil
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"first": b, "next": b}, faden.Options{Budget: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, submission := range []faden.Submission{
		{Session: "s", RequestID: "r1", Backend: "first", Input: "First question"},
		{Session: "s", RequestID: "r2", Backend: "next", Input: "Next question", Context: strings.Repeat("context", 100)},
	} {
		if _, err := r.Submit(t.Context(), submission); err != nil {
			t.Fatal(err)
		}
		if turn := wait(t, r, submission.RequestID); turn.Status != faden.Done {
			t.Fatalf("turn = %+v", turn)
		}
	}
}
