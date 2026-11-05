package faden_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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

func (r *handoverRuntime) Stop(context.Context, string) error { return nil }
func (r *handoverRuntime) Run(_ context.Context, command faden.Command) (faden.Result, error) {
	args, in, out := command.Argv, command.Stdin, command.Stdout
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
	cb, err := claude.New(claude.Config{Runtime: runtime})
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
	lb, err := llama.New(llama.Config{URL: server.URL + "/v1"})
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
	events, err := faden.CollectEvents(store.Events(context.Background(), "s", 0))
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
	cb, err := claude.New(claude.Config{Runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	other := &fake{run: func(ctx context.Context, _ faden.Turn, a *faden.Attempt) error {
		return a.Emit(ctx, faden.Event{Kind: faden.UserMessage, Text: "original"})
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
	silent := &fake{run: func(context.Context, faden.Turn, *faden.Attempt) error {
		return nil
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
	events, err := faden.CollectEvents(store.Events(context.Background(), "s", 0))
	if err != nil {
		t.Fatal(err)
	}
	// A silent backend's turn is still marked by its stored user message.
	if len(events) != 3 || events[1].Kind != faden.Handover || events[1].Action != "two" {
		t.Fatalf("%+v", events)
	}
}

// llamaRecorder is a llama-server stand-in that keeps the last request's
// messages and answers with a fixed text.
type llamaRecorder struct {
	mu       sync.Mutex
	messages []string
}

func (l *llamaRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct{ Role, Content string }
	}
	json.NewDecoder(r.Body).Decode(&body)
	l.mu.Lock()
	l.messages = nil
	for _, m := range body.Messages {
		l.messages = append(l.messages, m.Role+": "+m.Content)
	}
	l.mu.Unlock()
	io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"llama answer"}}]}`)
}

// count returns how many messages of the last request contain text.
func (l *llamaRecorder) count(text string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.messages {
		n += strings.Count(m, text)
	}
	return n
}

func runTurns(t *testing.T, store faden.Store, backends map[string]faden.Backend, subs ...faden.Submission) {
	t.Helper()
	runner, err := faden.Open(store, backends)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	for _, s := range subs {
		if _, err := runner.Submit(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		if turn := wait(t, runner, s.RequestID); turn.Status != faden.Done {
			t.Fatalf("%+v", turn)
		}
	}
}

// otherAgent answers with a payload in a foreign native format.
var otherAgent = &fake{run: func(ctx context.Context, _ faden.Turn, a *faden.Attempt) error {
	return a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: "other answer", Native: json.RawMessage(`{"content":[{"type":"text","text":"other answer"}]}`)})
}}

// A session that starts elsewhere reaches llama only as handover context:
// every earlier message once, never as llama's own native message.
func TestLlamaJoiningLaterSeesEarlierMessagesOnce(t *testing.T) {
	store := openStore(t, t.TempDir())
	rec := &llamaRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	lb, err := llama.New(llama.Config{URL: srv.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	runTurns(t, store, map[string]faden.Backend{"other": otherAgent, "llama": lb},
		faden.Submission{Session: "s", RequestID: "1", Backend: "other", Input: "first question"},
		faden.Submission{Session: "s", RequestID: "2", Backend: "llama", Input: "second question"},
	)
	for _, text := range []string{"first question", "other answer", "second question"} {
		if n := rec.count(text); n != 1 {
			t.Fatalf("%q appears %d times: %v", text, n, rec.messages)
		}
	}
}

// A session that starts on llama keeps llama's own first turn when it
// comes back after a handover.
func TestLlamaReturningKeepsItsOwnFirstTurn(t *testing.T) {
	store := openStore(t, t.TempDir())
	rec := &llamaRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	lb, err := llama.New(llama.Config{URL: srv.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	runTurns(t, store, map[string]faden.Backend{"other": otherAgent, "llama": lb},
		faden.Submission{Session: "s", RequestID: "1", Backend: "llama", Input: "first question"},
		faden.Submission{Session: "s", RequestID: "2", Backend: "other", Input: "second question"},
		faden.Submission{Session: "s", RequestID: "3", Backend: "llama", Input: "third question"},
	)
	for _, text := range []string{"first question", "second question", "other answer", "third question"} {
		if n := rec.count(text); n != 1 {
			t.Fatalf("%q appears %d times: %v", text, n, rec.messages)
		}
	}
	if !slices.Contains(rec.messages, "user: first question") {
		t.Fatalf("llama's own first turn is not native: %v", rec.messages)
	}
}
