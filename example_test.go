package faden_test

import (
	"context"
	"fmt"

	"github.com/nogo/faden"
	"github.com/nogo/faden/memstore"
)

// echo is a backend that answers with its input. A real host uses the
// claude, codex, acp or llama package instead.
type echo struct{}

func (echo) ContinuesWithoutPrompt() bool           { return false }
func (echo) Stop(context.Context, faden.Turn) error { return nil }
func (echo) Run(ctx context.Context, t faden.Turn, a *faden.Attempt) error {
	return a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: "you said: " + t.Input})
}

// A host submits a turn and waits for it. The turn and its events are in the
// store before Wait returns; jsonl or sqlite keep them across a restart.
func Example() {
	ctx := context.Background()
	store := memstore.New()
	runner, err := faden.Open(store, map[string]faden.Backend{"echo": echo{}})
	if err != nil {
		panic(err)
	}
	defer runner.Close()

	// A retried Submit with the same RequestID returns the stored turn and
	// runs nothing, so a client may retry freely.
	if _, err := runner.Submit(ctx, faden.Submission{
		Session: "s1", RequestID: "r1", Backend: "echo", Input: "hello",
	}); err != nil {
		panic(err)
	}
	turn, err := runner.Wait(ctx, "r1")
	if err != nil {
		panic(err)
	}
	fmt.Println(turn.Status)

	for e, err := range store.Events(ctx, "s1", 0) {
		if err != nil {
			panic(err)
		}
		if e.Kind == faden.UserMessage || e.Kind == faden.AssistantMessage {
			fmt.Printf("%s: %s\n", e.Kind, e.Text)
		}
	}
	// Output:
	// done
	// user_message: hello
	// assistant_message: you said: hello
}
