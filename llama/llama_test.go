package llama_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/jsonl"
	"github.com/nogo/faden/llama"
)

// replay serves recorded answers in order and keeps the requests it got.
type replay struct {
	mu       sync.Mutex
	answers  []string
	requests []request
}

type request struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func newReplay(t *testing.T, files ...string) (*replay, *httptest.Server) {
	t.Helper()
	r := &replay{}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		r.answers = append(r.answers, string(b))
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return r, srv
}

func (r *replay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	var parsed request
	json.Unmarshal(body, &parsed)
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Path != "/v1/chat/completions" || len(r.answers) == 0 {
		http.Error(w, "unexpected request", http.StatusInternalServerError)
		return
	}
	r.requests = append(r.requests, parsed)
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, r.answers[0])
	r.answers = r.answers[1:]
}

func (r *replay) sent() []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]request(nil), r.requests...)
}

func openStore(t *testing.T, dir string) *jsonl.Store {
	t.Helper()
	s, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addTool(runs *int) llama.Tool {
	return llama.Tool{
		Name:       "add",
		Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}}}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			*runs++
			var a struct{ A, B int }
			json.Unmarshal(args, &a)
			return string(rune('0' + a.A + a.B)), nil
		},
	}
}

func run(t *testing.T, store *jsonl.Store, b *llama.Backend, id, input string) faden.Turn {
	t.Helper()
	r, err := faden.Open(store, map[string]faden.Backend{"llama": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), faden.Submission{Session: "s1", RequestID: id, Input: input, Backend: "llama", Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	got, err := r.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestTurnStoresAnswersCallsAndResultsInOrder(t *testing.T) {
	rec, srv := newReplay(t, "add_call.json", "final.json")
	store := openStore(t, t.TempDir())
	var runs int
	b, err := llama.New(llama.Config{URL: srv.URL, Store: store, Tools: []llama.Tool{addTool(&runs)}})
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, store, b, "r1", "what is 2+3?")
	if got.Status != faden.Done {
		t.Fatalf("turn = %+v", got)
	}
	events, _ := store.Events(t.Context(), "s1", 0)
	var kinds []faden.Kind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []faden.Kind{faden.UserMessage, faden.AssistantMessage, faden.ToolCall, faden.ToolResult, faden.AssistantMessage}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if events[2].Tool != "add" || string(events[2].Input) != `{"a":2,"b":3}` || events[3].Output != "5" || events[4].Text != "All done." {
		t.Fatalf("events = %+v", events)
	}
	reqs := rec.sent()
	if len(reqs) != 2 || reqs[0].Model != "test-model" {
		t.Fatalf("requests = %+v", reqs)
	}
	last := reqs[1].Messages
	if len(last) != 3 || last[2].Role != "tool" || last[2].Content != "5" || last[2].ToolCallID != "call_b1" {
		t.Fatalf("second request messages = %+v", last)
	}
	names := map[string]bool{}
	for _, tl := range reqs[0].Tools {
		names[tl.Function.Name] = true
	}
	if !names["add"] || !names["read_thread"] || names["bash"] {
		t.Fatalf("tools offered = %v", names)
	}
}

// seed stores a turn cut off after a call: the user message, the assistant
// message with its call, and the call, but no result.
func seed(t *testing.T, dir, tool, args string) {
	t.Helper()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	store.SubmitTurn(ctx, faden.Turn{RequestID: "r1", Session: "s1", Input: "go", Backend: "llama"})
	store.SaveTurn(ctx, faden.Turn{RequestID: "r1", Session: "s1", Input: "go", Backend: "llama", Status: faden.Running, Attempts: 1})
	native, _ := json.Marshal(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
		"id": "c1", "type": "function", "function": map[string]string{"name": tool, "arguments": args},
	}}})
	for _, e := range []faden.Event{
		{Kind: faden.UserMessage, Text: "go"},
		{Kind: faden.AssistantMessage, Native: native},
		{Kind: faden.ToolCall, Tool: tool, Input: json.RawMessage(args), Native: json.RawMessage(`{"id":"c1"}`)},
	} {
		e.Session, e.RequestID = "s1", "r1"
		if _, err := store.AppendEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoredCallWithoutResultRunsAgainOnlyIfSafe(t *testing.T) {
	for _, tc := range []struct {
		name       string
		safe       bool
		wantRuns   int
		wantResult string
	}{
		{"safe tool runs again", true, 1, "5"},
		{"unsafe tool is reported interrupted", false, 0, "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seed(t, dir, "add", `{"a":2,"b":3}`)
			rec, srv := newReplay(t, "final.json")
			store := openStore(t, dir)
			var runs int
			tool := addTool(&runs)
			tool.Safe = tc.safe
			b, _ := llama.New(llama.Config{URL: srv.URL, Store: store, Tools: []llama.Tool{tool}})
			got := run2(t, store, b)
			if got.Status != faden.Done || runs != tc.wantRuns {
				t.Fatalf("turn = %+v, runs = %d", got, runs)
			}
			msgs := rec.sent()[0].Messages
			if len(msgs) != 3 || msgs[2].Role != "tool" || !strings.HasPrefix(msgs[2].Content, tc.wantResult) {
				t.Fatalf("messages = %+v", msgs)
			}
		})
	}
}

// run2 opens a runner on a store that already holds a running turn.
func run2(t *testing.T, store *jsonl.Store, b *llama.Backend) faden.Turn {
	t.Helper()
	r, err := faden.Open(store, map[string]faden.Backend{"llama": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	got, err := r.Wait(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A crash after the turn started but before anything was stored must not
// lose the turn: its input is in the store.
func TestTurnKilledBeforeFirstEventContinuesFromStoredInput(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	ctx := t.Context()
	store.SubmitTurn(ctx, faden.Turn{RequestID: "r1", Session: "s1", Input: "go", Backend: "llama"})
	store.SaveTurn(ctx, faden.Turn{RequestID: "r1", Session: "s1", Input: "go", Backend: "llama", Status: faden.Running, Attempts: 1})
	rec, srv := newReplay(t, "final.json")
	b, _ := llama.New(llama.Config{URL: srv.URL, Store: store})
	if got := run2(t, store, b); got.Status != faden.Done {
		t.Fatalf("turn = %+v", got)
	}
	msgs := rec.sent()[0].Messages
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "go" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestCallNeverStoredRunsEvenIfUnsafe(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "add", `{"a":1,"b":1}`)
	// Drop the stored call: the crash came right after the answer.
	path := filepath.Join(dir, "s1.jsonl")
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	os.WriteFile(path, []byte(strings.Join(lines[:len(lines)-1], "\n")+"\n"), 0o600)
	_, srv := newReplay(t, "final.json")
	store := openStore(t, dir)
	var runs int
	b, _ := llama.New(llama.Config{URL: srv.URL, Store: store, Tools: []llama.Tool{addTool(&runs)}})
	if got := run2(t, store, b); got.Status != faden.Done || runs != 1 {
		t.Fatalf("turn = %+v, runs = %d", got, runs)
	}
}

func TestReadThreadReturnsRangeAndRefusesOtherSession(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "read_thread", `{"from":1,"to":2}`)
	_, srv := newReplay(t, "final.json")
	store := openStore(t, dir)
	rec := &recorder{Store: store}
	b, _ := llama.New(llama.Config{URL: srv.URL, Store: rec})
	run2(t, store, b)
	events, _ := store.Events(t.Context(), "s1", 0)
	var result string
	for _, e := range events {
		if e.Kind == faden.ToolResult {
			result = e.Output
		}
	}
	lines := strings.Split(result, "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"seq":1`) || !strings.Contains(lines[1], `"seq":2`) {
		t.Fatalf("read_thread result = %q", result)
	}
	if rec.sessions["s1"] == 0 || len(rec.sessions) != 1 {
		t.Fatalf("sessions read = %v", rec.sessions)
	}

	dir2 := t.TempDir()
	seed(t, dir2, "read_thread", `{"session":"s2","from":1,"to":2}`)
	_, srv2 := newReplay(t, "final.json")
	store2 := openStore(t, dir2)
	rec2 := &recorder{Store: store2}
	b2, _ := llama.New(llama.Config{URL: srv2.URL, Store: rec2})
	run2(t, store2, b2)
	events, _ = store2.Events(t.Context(), "s1", 0)
	for _, e := range events {
		if e.Kind == faden.ToolResult && !strings.HasPrefix(e.Output, "error: only this conversation") {
			t.Fatalf("result = %q, want a refusal", e.Output)
		}
	}
	if rec2.sessions["s2"] != 0 {
		t.Fatalf("session s2 was read: %v", rec2.sessions)
	}
}

// recorder counts which sessions the store was asked for.
type recorder struct {
	llama.Store
	mu       sync.Mutex
	sessions map[string]int
}

func (r *recorder) Events(ctx context.Context, session string, after int64) ([]faden.Event, error) {
	r.mu.Lock()
	if r.sessions == nil {
		r.sessions = map[string]int{}
	}
	r.sessions[session]++
	r.mu.Unlock()
	return r.Store.Events(ctx, session, after)
}

func TestNewRejectsHostToolWithBuiltInName(t *testing.T) {
	_, err := llama.New(llama.Config{Tools: []llama.Tool{{Name: "bash", Run: func(context.Context, json.RawMessage) (string, error) { return "", nil }}}})
	if err == nil {
		t.Fatal("New accepted a tool named bash")
	}
}

func TestServerErrorFailsTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	store := openStore(t, t.TempDir())
	b, _ := llama.New(llama.Config{URL: srv.URL, Store: store})
	got := run(t, store, b, "r1", "hi")
	if got.Status != faden.Failed || !strings.Contains(got.Error, "503") {
		t.Fatalf("turn = %+v", got)
	}
}

// The crash test re-runs this test binary as a child. The child asks its
// model to run bash; the command signals "ready" on the child's stdout after
// the call is stored, then blocks reading the child's stdin. The parent kills
// the child with SIGKILL and restarts on the same directory.
const (
	crashDirEnv = "FADEN_LLAMA_CRASH_DIR"
	runsEnv     = "FADEN_RUNS"
)

func TestCrashChildProcess(t *testing.T) {
	dir := os.Getenv(crashDirEnv)
	if dir == "" {
		t.Skip("helper for TestKilledBetweenBashCallAndResultDoesNotRunBashAgain")
	}
	_, srv := newReplay(t, "bash_call.json")
	store := openStore(t, dir)
	b, _ := llama.New(llama.Config{URL: srv.URL, Store: store, Runtime: faden.Local{PIDDir: dir, Dir: dir}})
	r, err := faden.Open(store, map[string]faden.Backend{"llama": b})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(t.Context(), faden.Submission{Session: "s1", RequestID: "r1", Input: "run it", Backend: "llama"}); err != nil {
		t.Fatal(err)
	}
	select {} // killed by the parent
}

func TestKilledBetweenBashCallAndResultDoesNotRunBashAgain(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(t.TempDir(), "runs")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChildProcess$")
	cmd.Env = append(os.Environ(), crashDirEnv+"="+dir, runsEnv+"="+runs)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "ready\n" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("child output %q, err %v", line, err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	stdin.Close() // lets the orphaned bash exit

	store := openStore(t, dir)
	events, _ := store.Events(t.Context(), "s1", 0)
	if last := events[len(events)-1]; last.Kind != faden.ToolCall || last.Tool != "bash" {
		t.Fatalf("last stored event before restart = %+v, want the bash call", last)
	}

	rec, srv := newReplay(t, "final.json")
	b, _ := llama.New(llama.Config{URL: srv.URL, Store: store, Runtime: faden.Local{PIDDir: dir, Dir: dir}})
	got := run2(t, store, b)
	if got.Status != faden.Done || got.Attempts != 2 {
		t.Fatalf("turn = %+v", got)
	}
	ran, _ := os.ReadFile(runs)
	if string(ran) != "ran\n" {
		t.Fatalf("bash ran %q, want one run", ran)
	}
	msgs := rec.sent()[0].Messages
	if last := msgs[len(msgs)-1]; last.Role != "tool" || !strings.Contains(last.Content, "interrupted") {
		t.Fatalf("model saw %+v", last)
	}
}

func TestBashRunsThroughRuntimeInItsDirectory(t *testing.T) {
	dir := t.TempDir()
	_, srv := newReplay(t, "final.json")
	seed2 := t.TempDir()
	seed(t, seed2, "bash", `{"command":"pwd; echo oops >&2; exit 3"}`)
	// Drop the stored call so the loop runs bash itself.
	path := filepath.Join(seed2, "s1.jsonl")
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	os.WriteFile(path, []byte(strings.Join(lines[:len(lines)-1], "\n")+"\n"), 0o600)
	store2 := openStore(t, seed2)
	b, _ := llama.New(llama.Config{URL: srv.URL, Store: store2, Runtime: faden.Local{PIDDir: dir, Dir: dir}})
	run2(t, store2, b)
	events, _ := store2.Events(t.Context(), "s1", 0)
	for _, e := range events {
		if e.Kind == faden.ToolResult {
			resolved, _ := filepath.EvalSymlinks(dir)
			if !strings.Contains(e.Output, resolved) || !strings.Contains(e.Output, "oops") || !strings.Contains(e.Output, "[exit code 3]") {
				t.Fatalf("bash result = %q", e.Output)
			}
			return
		}
	}
	t.Fatal("no tool result stored")
}

// Live runs one turn with one tool call against a real llama-server.
func TestLive(t *testing.T) {
	if os.Getenv("FADEN_LIVE") != "1" {
		t.Skip("set FADEN_LIVE=1, FADEN_LLAMA_URL and FADEN_LLAMA_MODEL")
	}
	url, model := os.Getenv("FADEN_LLAMA_URL"), os.Getenv("FADEN_LLAMA_MODEL")
	if url == "" || model == "" {
		t.Fatal("FADEN_LLAMA_URL and FADEN_LLAMA_MODEL are required")
	}
	store := openStore(t, t.TempDir())
	var runs int
	b, err := llama.New(llama.Config{URL: url, Model: model, Store: store, Tools: []llama.Tool{addTool(&runs)}})
	if err != nil {
		t.Fatal(err)
	}
	// Not run(): its turn names a fake model, which overrides the configured
	// one, and its timeout is too short for a real model.
	r, err := faden.Open(store, map[string]faden.Backend{"llama": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), faden.Submission{Session: "s1", RequestID: "r1", Input: "Use the add tool to add 2 and 3, then tell me the sum.", Backend: "llama"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	got, err := r.Wait(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != faden.Done || runs == 0 {
		t.Fatalf("turn = %+v, tool runs = %d", got, runs)
	}
}

func TestCallIDReusedInLaterTurnRunsAgain(t *testing.T) {
	_, srv := newReplay(t, "add_call.json", "final.json", "add_call.json", "final.json")
	store := openStore(t, t.TempDir())
	var runs int
	b, err := llama.New(llama.Config{URL: srv.URL, Store: store, Tools: []llama.Tool{addTool(&runs)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r1", "r2"} {
		if got := run(t, store, b, id, "what is 2+3?"); got.Status != faden.Done {
			t.Fatalf("turn %s = %+v", id, got)
		}
	}
	if runs != 2 {
		t.Fatalf("tool ran %d times, want 2", runs)
	}
	events, _ := store.Events(t.Context(), "s1", 0)
	var calls, results int
	for _, e := range events {
		switch e.Kind {
		case faden.ToolCall:
			calls++
		case faden.ToolResult:
			results++
		}
	}
	if calls != 2 || results != 2 {
		t.Fatalf("stored %d calls and %d results, want 2 and 2", calls, results)
	}
}

// stopOnlyRuntime forwards recovery cleanup and rejects any unexpected command.
type stopOnlyRuntime struct{ local faden.Local }

func (r stopOnlyRuntime) Stop(ctx context.Context, key string) error { return r.local.Stop(ctx, key) }
func (r stopOnlyRuntime) Run(context.Context, faden.Command) (faden.Result, error) {
	return faden.Result{}, fmt.Errorf("unexpected command")
}

func TestContinuedTurnWithoutCommandStopsLeftover(t *testing.T) {
	dir := t.TempDir()
	local := faden.Local{PIDDir: dir, Dir: dir}
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := local.Run(t.Context(), faden.Command{Key: "r1", Argv: []string{"sh", "-c", "echo ready; sleep 600 & wait"}, Stdout: readyWriter{ready}})
		done <- err
	}()
	select {
	case <-ready:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.SubmitTurn(t.Context(), faden.Turn{RequestID: "r1", Session: "s1", Backend: "llama", Input: "go"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTurn(t.Context(), faden.Turn{RequestID: "r1", Session: "s1", Backend: "llama", Input: "go", Status: faden.Running, Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	}))
	defer srv.Close()
	backend, err := llama.New(llama.Config{URL: srv.URL, Store: store, Runtime: stopOnlyRuntime{local}})
	if err != nil {
		t.Fatal(err)
	}
	// The runner continues the turn as attempt 2 and stops its leftover first.
	runner, err := faden.Open(store, map[string]faden.Backend{"llama": backend})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	if got, err := runner.Wait(t.Context(), "r1"); err != nil || got.Status != faden.Done {
		t.Fatalf("turn = %+v, %v", got, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("leftover still running")
	}
}

type readyWriter struct{ ready chan struct{} }

func (w readyWriter) Write(p []byte) (int, error) {
	select {
	case w.ready <- struct{}{}:
	default:
	}
	return len(p), nil
}
