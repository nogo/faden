package faden_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nogo/faden"
	"github.com/nogo/faden/claude"
	"github.com/nogo/faden/llama"
)

type handoverRuntime struct {
	mu      sync.Mutex
	args    [][]string
	prompts []string
}

func (r *handoverRuntime) RunStream(_ context.Context, args []string, in io.Reader, out io.Writer) (faden.Result, error) {
	var request struct{ Message struct{ Content string } }
	if err := json.NewDecoder(in).Decode(&request); err != nil {
		return faden.Result{}, err
	}
	r.mu.Lock()
	r.args = append(r.args, append([]string(nil), args...))
	r.prompts = append(r.prompts, request.Message.Content)
	r.mu.Unlock()
	_, err := io.WriteString(out, "{\"type\":\"assistant\",\"message\":{\"id\":\"a\",\"content\":[{\"type\":\"text\",\"text\":\"Claude answer\"}]}}\n{\"type\":\"result\",\"result\":\"done\"}\n")
	return faden.Result{}, err
}
func TestHandoverAcrossAdapters(t *testing.T) {
	store := openStore(t, t.TempDir())
	runtime := &handoverRuntime{}
	cb, err := claude.New(claude.Config{Runtime: runtime, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var messages []struct {
		Role    string
		Content string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string
				Content string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		messages = body.Messages
		mu.Unlock()
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"llama answer"}}]}`)
	}))
	defer server.Close()
	lb, err := llama.New(llama.Config{URL: server.URL, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := faden.Open(store, map[string]faden.Backend{"claude": cb, "llama": lb})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	for _, s := range []faden.Submission{
		{Session: "s", RequestID: "1", Backend: "claude", Input: "first question"},
		{Session: "s", RequestID: "2", Backend: "llama", Input: "second question"},
		{Session: "s", RequestID: "3", Backend: "claude", Input: "third question"},
	} {
		if _, err := runner.Submit(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		if turn := wait(t, runner, s.RequestID); turn.Status != faden.Done {
			t.Fatalf("%+v", turn)
		}
	}
	mu.Lock()
	raw, _ := json.Marshal(messages)
	mu.Unlock()
	for _, text := range []string{"first question", "Claude answer", "second question"} {
		if !strings.Contains(string(raw), text) {
			t.Fatalf("missing %q: %s", text, raw)
		}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !strings.Contains(strings.Join(runtime.args[1], " "), "--resume") {
		t.Fatal(runtime.args)
	}
	prompt := runtime.prompts[1]
	for _, text := range []string{"[user]", "[assistant]", "second question", "llama answer", "third question"} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("missing %q: %s", text, prompt)
		}
	}
	if strings.Contains(prompt, "first question") || strings.Contains(prompt, "Claude answer") {
		t.Fatalf("repeated seen messages: %s", prompt)
	}
	events, err := store.Events(context.Background(), "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Kind == faden.Handover {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("handover count %d", count)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := faden.Open(store, map[string]faden.Backend{"claude": cb, "llama": lb})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	large := strings.Repeat("large request ", 2000)
	if _, err := reopened.Submit(context.Background(), faden.Submission{Session: "s", RequestID: "4", Backend: "llama", Input: large}); err != nil {
		t.Fatal(err)
	}
	if turn := wait(t, reopened, "4"); turn.Status != faden.Done {
		t.Fatalf("%+v", turn)
	}
	mu.Lock()
	defer mu.Unlock()
	if messages[len(messages)-1].Content != large {
		t.Fatal("llama request shortened")
	}
	found := false
	for _, m := range messages {
		if m.Content == "third question" {
			found = true
		}
	}
	if !found {
		t.Fatal("returning llama missed catch-up after restart")
	}
}

func TestHandoverKeepsOversizedRequest(t *testing.T) {
	store := openStore(t, t.TempDir())
	runtime := &handoverRuntime{}
	cb, err := claude.New(claude.Config{Runtime: runtime, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	other := &fake{run: func(_ context.Context, _ faden.Turn, _ json.RawMessage, emit func(faden.Event) error) (json.RawMessage, error) {
		return nil, emit(faden.Event{Kind: faden.UserMessage, Text: "original"})
	}}
	runner, err := faden.Open(store, map[string]faden.Backend{"other": other, "claude": cb}, faden.Options{Budget: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	request := strings.Repeat("new request ", 2000)
	for _, s := range []faden.Submission{
		{Session: "s", RequestID: "1", Backend: "other", Input: "original"},
		{Session: "s", RequestID: "2", Backend: "claude", Input: request},
	} {
		if _, err := runner.Submit(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		if turn := wait(t, runner, s.RequestID); turn.Status != faden.Done {
			t.Fatalf("%+v", turn)
		}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !strings.HasSuffix(runtime.prompts[0], request) || !strings.Contains(runtime.prompts[0], "original") {
		t.Fatal("request or first message shortened")
	}
}

func TestHandoverAfterSilentBackend(t *testing.T) {
	store := openStore(t, t.TempDir())
	silent := &fake{run: func(context.Context, faden.Turn, json.RawMessage, func(faden.Event) error) (json.RawMessage, error) {
		return nil, nil
	}}
	runner, err := faden.Open(store, map[string]faden.Backend{"one": silent, "two": silent})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	for _, s := range []faden.Submission{
		{Session: "s", RequestID: "1", Backend: "one"},
		{Session: "s", RequestID: "2", Backend: "two"},
	} {
		if _, err := runner.Submit(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		if turn := wait(t, runner, s.RequestID); turn.Status != faden.Done {
			t.Fatalf("%+v", turn)
		}
	}
	events, err := store.Events(context.Background(), "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Kind != faden.Handover || events[1].Action != "two" {
		t.Fatalf("%+v", events)
	}
}
