package llama_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
	"github.com/nogo/faden/llama"
)

func TestReasoningStaysNative(t *testing.T) {
	rec, srv := newReplay(t, "reasoning_call.json", "final.json")
	dir := t.TempDir()
	store := openStore(t, dir)
	var runs int
	b, err := llama.New(llama.Config{URL: srv.URL + "/v1", Tools: []llama.Tool{addTool(&runs)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, store, b, "r1", "what is 2+3?"); got.Status != faden.Done {
		t.Fatalf("turn = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "s1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("invented reasoning run: %d stored bytes", info.Size())
	events, err := store.Events(t.Context(), "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var native struct {
		Reasoning string `json:"reasoning_content"`
	}
	if err := json.Unmarshal(events[1].Native, &native); err != nil {
		t.Fatal(err)
	}
	if native.Reasoning != "I will add two and three." {
		t.Fatalf("native reasoning = %q", native.Reasoning)
	}
	if events[1].Text != "Adding." {
		t.Fatalf("answer = %q", events[1].Text)
	}
	reqs := rec.sent()
	if len(reqs) != 2 || reqs[1].Messages[1].Reasoning != native.Reasoning {
		t.Fatalf("requests = %+v", reqs)
	}
	for _, item := range handover.Select(events, "continue", 16000) {
		if strings.Contains(item.Text, native.Reasoning) {
			t.Fatalf("handover exposes reasoning: %q", item.Text)
		}
	}
	output, err := handover.ReadThread(t.Context(), store, "s1", json.RawMessage(`{"from":1,"to":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, native.Reasoning) || strings.Contains(output, "reasoning_content") {
		t.Fatalf("read_thread exposes reasoning: %s", output)
	}
	if !strings.Contains(output, "Adding.") {
		t.Fatalf("read_thread lost answer: %s", output)
	}
}
