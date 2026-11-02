package faden_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/nogo/faden"
)

func TestFilesAreStoredAsPathsSentAgainOnContinuationAndShownInUserMessage(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	files := []string{"shot.png", "notes.txt"}
	if _, err := s.SubmitTurn(t.Context(), faden.Turn{RequestID: "r1", Session: "s1", Input: "look", Files: files, Backend: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTurn(t.Context(), faden.Turn{RequestID: "r1", Status: faden.Running, Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	store := openStore(t, dir)
	var sent faden.Turn
	b := &fake{run: func(_ context.Context, turn faden.Turn, _ *faden.Attempt) error { sent = turn; return nil }}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	wait(t, r, "r1")
	if sent.Attempts != 2 || sent.Input != "Continue where you left off." || !reflect.DeepEqual(sent.Files, files) {
		t.Fatalf("continuation sent %+v, want the files again", sent)
	}
	events, err := store.Events(t.Context(), "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Kind != faden.UserMessage || events[0].Text != "look\nAttached file: shot.png\nAttached file: notes.txt\n" {
		t.Fatalf("user message = %+v", events[0])
	}
}
