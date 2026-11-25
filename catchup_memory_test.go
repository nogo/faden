package faden_test

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
)

func heapInUse() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// A backend that never took part in a long thread gets a catch-up of all of
// it. The attempt must not hold that thread while the backend runs: the
// handover reads it from the store and keeps only what it selects.
func TestHandoverDoesNotHoldTheThreadWhileTheBackendRuns(t *testing.T) {
	ctx := t.Context()
	store := openStore(t, t.TempDir())
	if _, err := store.SubmitTurn(ctx, faden.Turn{RequestID: "old", Session: "s", Backend: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTurn(ctx, faden.Turn{RequestID: "old", Progress: faden.Progress{Status: faden.Done}}); err != nil {
		t.Fatal(err)
	}
	output := strings.Repeat("x", 100<<10)
	const events = 200 // 20 MB of tool output
	for range events {
		if _, err := store.AppendEvent(ctx, faden.Event{Session: "s", RequestID: "old", Kind: faden.ToolResult, Output: output}); err != nil {
			t.Fatal(err)
		}
	}
	output = ""
	var held int64
	var prompt string
	b := &fake{run: func(ctx context.Context, turn faden.Turn, a *faden.Attempt) error {
		var err error
		if prompt, err = handover.Prompt(ctx, turn, a); err != nil {
			return err
		}
		held = heapInUse()
		return nil
	}}
	before := heapInUse()
	r, err := faden.Open(store, map[string]faden.Backend{"first": &fake{}, "second": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	next := sub("s", "next", "go on")
	next.Backend = "second"
	if _, err := r.Submit(ctx, next); err != nil {
		t.Fatal(err)
	}
	wait(t, r, "next")
	if !strings.Contains(prompt, "Omitted events") {
		t.Fatalf("prompt has no catch-up: %.200q", prompt)
	}
	if grew := held - before; grew > 4<<20 {
		t.Fatalf("heap grew %d KB while the backend ran", grew>>10)
	}
	t.Logf("heap during Run: %+d KB for %d events of 100 KB", (held-before)>>10, events)
}
