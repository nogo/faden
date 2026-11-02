package codex

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/internal/continuationtest"
	"github.com/nogo/faden/jsonl"
)

// These invented lines exercise the wire format. They are not a real-run recording.
const turnLines = `{"method":"turn/started","params":{"turn":{"id":"turn_1","status":"inProgress"}}}
{"method":"item/started","params":{"item":{"type":"commandExecution","id":"item_1","command":"cat greeting.txt"}}}
{"id":"approval-7","method":"item/commandExecution/requestApproval","params":{"itemId":"item_1","command":"cat greeting.txt"}}
{"method":"item/completed","params":{"item":{"type":"userMessage","id":"item_0","content":[{"type":"text","text":"Read greeting.txt"}]}}}
{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"item_2","text":"I will inspect the file."}}}
{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"item_1","command":"cat greeting.txt","cwd":"/work","status":"declined","aggregatedOutput":"hello","exitCode":0}}}
{"method":"item/completed","params":{"item":{"type":"fileChange","id":"item_3","status":"completed","changes":[{"path":"a.txt","kind":"add"},{"path":"b.txt","kind":"add"}]}}}
{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"item_4","text":"The file says hello."}}}
{"method":"turn/completed","params":{"turn":{"id":"turn_1","status":"completed"}}}
`

// A fake codex that answers the handshake, logs every line it receives
// to "got", and replays "fixture" after turn/start.
const fakeCodex = `#!/bin/sh
printf '%s\n' "$@" > args
printf '%s' "$HOST_VAR" > var
while IFS= read -r line; do
  printf '%s\n' "$line" >> got
  case $line in
    *'"method":"initialize"'*) echo '{"id":1,"result":{}}' ;;
    *'"method":"thread/start"'*|*'"method":"thread/resume"'*) echo '{"id":2,"result":{"thread":{"id":"thr_invented"}}}' ;;
    *'"method":"turn/start"'*) echo '{"id":3,"result":{"turn":{"id":"turn_1"}}}'; cat fixture ;;
  esac
done
`

func setup(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"fixture": fixture} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(fakeCodex), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

// checkpoint is a jsonl store that records what the fake codex had received
// when the cursor was saved.
type checkpoint struct {
	*jsonl.Store
	dir     string
	gotThen string
}

func (s *checkpoint) SetCursor(ctx context.Context, session, backend string, data json.RawMessage) error {
	got, _ := os.ReadFile(filepath.Join(s.dir, "got"))
	s.gotThen = string(got)
	return s.Store.SetCursor(ctx, session, backend, data)
}

// newAttempt submits turn to store and returns a run of it with the cursor
// the store holds, as the runner would without catch-up.
func newAttempt(t *testing.T, store faden.Store, turn faden.Turn) *faden.Attempt {
	t.Helper()
	turn.Backend = "codex"
	if _, err := store.SubmitTurn(t.Context(), turn); err != nil {
		t.Fatal(err)
	}
	a, err := faden.NewAttempt(t.Context(), store, turn)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func gotMethods(t *testing.T, dir string) []string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "got"))
	var methods []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var m struct {
			Method string `json:"method"`
		}
		json.Unmarshal([]byte(line), &m)
		methods = append(methods, m.Method)
	}
	return methods
}

func newBackend(t *testing.T, dir string, args ...string) (*Backend, *checkpoint) {
	t.Helper()
	store, err := jsonl.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	b, err := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir}, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return b, &checkpoint{Store: store, dir: dir}
}

func TestTurnThenResumeWithoutPrompt(t *testing.T) {
	dir := setup(t, turnLines)
	b, store := newBackend(t, dir, "-c", `sandbox_mode="read-only"`)
	first := faden.Turn{Session: "s", RequestID: "r1", Input: "Read greeting.txt", Model: "invented-model"}
	if err := b.Run(t.Context(), first, newAttempt(t, store, first)); err != nil {
		t.Fatal(err)
	}
	sess, err := store.Session(t.Context(), "s")
	if err != nil || string(sess.Cursors["codex"]) != `{"thread_id":"thr_invented"}` {
		t.Fatalf("cursor: %s, %v", sess.Cursors["codex"], err)
	}
	events, err := store.Events(t.Context(), "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(store.gotThen, "turn/start") || !strings.Contains(store.gotThen, "thread/start") {
		t.Fatalf("cursor saved after turn/start was sent; codex had received:\n%s", store.gotThen)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if string(args) != "app-server\n-c\nsandbox_mode=\"read-only\"\n" {
		t.Fatalf("args: %q", args)
	}
	var kinds []faden.Kind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
		// The CLI keeps its own session; faden stores normalized fields only.
		if e.Native != nil {
			t.Fatalf("native payload stored: %q", e.Native)
		}
	}
	want := []faden.Kind{faden.AssistantMessage, faden.ToolCall, faden.ToolResult, faden.ToolCall, faden.ToolResult, faden.AssistantMessage, faden.TurnEnded}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds: %v", kinds)
	}
	if events[1].Action != "cat greeting.txt" || events[2].Output != "hello" || events[3].Action != "a.txt, b.txt" {
		t.Fatalf("events: %+v", events)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "got"))
	for _, need := range []string{`"decision":"decline"`, `"id":"approval-7"`, `"model":"invented-model"`, `"text":"Read greeting.txt"`} {
		if !strings.Contains(string(got), need) {
			t.Fatalf("missing %s in:\n%s", need, got)
		}
	}

	os.Remove(filepath.Join(dir, "got"))
	second := faden.Turn{Session: "s", RequestID: "r2"}
	if err := b.Run(t.Context(), second, newAttempt(t, store, second)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMethods(t, dir), []string{"initialize", "initialized", "thread/resume", "turn/start", ""}) {
		t.Fatalf("methods: %v", gotMethods(t, dir))
	}
	got, _ = os.ReadFile(filepath.Join(dir, "got"))
	if !strings.Contains(string(got), `"threadId":"thr_invented"`) || !strings.Contains(string(got), `"input":[]`) {
		t.Fatalf("resume lines:\n%s", got)
	}
	if b.ContinuesWithoutPrompt() {
		t.Fatal("codex needs stored events in its continuation prompt")
	}
}

// answering is a backend that answers every turn with one message.
type answering string

func (answering) ContinuesWithoutPrompt() bool           { return true }
func (answering) Stop(context.Context, faden.Turn) error { return nil }
func (s answering) Run(ctx context.Context, _ faden.Turn, a *faden.Attempt) error {
	return a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: string(s)})
}

func TestHandoverPutsCatchUpBeforeRequest(t *testing.T) {
	dir := setup(t, turnLines)
	b, store := newBackend(t, dir)
	r, err := faden.Open(store, map[string]faden.Backend{"other": answering("earlier answer"), "codex": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, s := range []faden.Submission{
		{Session: "s", RequestID: "r1", Backend: "other", Input: "earlier question"},
		{Session: "s", RequestID: "r2", Backend: "codex", Input: "new request"},
	} {
		if _, err := r.Submit(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		if turn, err := r.Wait(t.Context(), s.RequestID); err != nil || turn.Status != faden.Done {
			t.Fatalf("turn %+v: %v", turn, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "got"))
	var turn struct {
		Params struct {
			Input []struct{ Text string }
		}
	}
	for line := range strings.SplitSeq(string(got), "\n") {
		if strings.Contains(line, `"turn/start"`) {
			json.Unmarshal([]byte(line), &turn)
		}
	}
	text := turn.Params.Input[0].Text
	if !strings.Contains(text, "[user]\nearlier question") || !strings.Contains(text, "[assistant]\nearlier answer") || !strings.HasSuffix(text, "\n\nnew request") {
		t.Fatalf("prompt: %q", text)
	}
}

func TestFailures(t *testing.T) {
	cases := map[string]string{
		"turn failed":   `{"method":"turn/completed","params":{"turn":{"status":"failed","error":{"message":"invented failure"}}}}` + "\n",
		"no completion": `{"method":"item/completed","params":{"item":{"type":"agentMessage","text":"x"}}}` + "\n",
		"malformed":     "{\n",
		"thread error":  "",
	}
	for name, fixture := range cases {
		t.Run(name, func(t *testing.T) {
			dir := setup(t, fixture)
			if name == "thread error" {
				script := "#!/bin/sh\nread line\necho '{\"id\":1,\"error\":{\"message\":\"invented\"}}'\ncat >/dev/null\n"
				os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0700)
			}
			b, store := newBackend(t, dir)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			turn := faden.Turn{Session: "s", RequestID: "r", Input: "x"}
			if err := b.Run(ctx, turn, newAttempt(t, store, turn)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func runnerBackend(t *testing.T, dir string) continuationtest.Factory {
	t.Helper()
	return func() faden.Backend {
		b, err := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir}, Args: []string{"-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
}

func TestHostHelper(t *testing.T) {
	dir := os.Getenv("FADEN_CODEX_HOST")
	if dir == "" {
		return
	}
	input := "Write first.txt containing FIRST. Then run bash sleep 120. Then write second.txt and third.txt containing DONE, one after another. If interrupted and resumed, before using tools say FIRST_FILE_KNOWN if this conversation shows that first.txt was written, or FIRST_FILE_UNKNOWN otherwise."
	continuationtest.Host(t, dir, "codex", input, runnerBackend(t, dir))
}

func waitFile(t *testing.T, path string) []byte {
	t.Helper()
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil {
			return data
		}
		select {
		case <-timer.C:
			t.Fatalf("waiting for %s", path)
		case <-tick.C:
		}
	}
}

// TestLiveResumeAfterKill kills the host mid-turn, continues through the runner,
// and logs whether Codex knew the work done before the kill.
func TestLiveResumeAfterKill(t *testing.T) {
	t.Setenv("FADEN_KILL_AFTER_FILE", "first.txt")
	if os.Getenv("FADEN_LIVE") != "1" {
		t.Skip("set FADEN_LIVE=1 to run Codex")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	continuationtest.KillHost(t, dir, "FADEN_CODEX_HOST")
	events := continuationtest.Continue(t, dir, "codex", runnerBackend(t, dir))
	var answer strings.Builder
	for _, e := range events {
		if e.Kind == faden.AssistantMessage {
			answer.WriteString(e.Text)
		}
	}
	waitFile(t, filepath.Join(dir, "second.txt"))
	waitFile(t, filepath.Join(dir, "third.txt"))
	knowledge := "indeterminate"
	if strings.Contains(answer.String(), "FIRST_FILE_UNKNOWN") {
		knowledge = "unknown"
	} else if strings.Contains(answer.String(), "FIRST_FILE_KNOWN") {
		knowledge = "known"
	}
	t.Logf("Codex knowledge of first file: %s", knowledge)
	t.Logf("Codex resume after a kill; resumed assistant: %s", answer.String())
}

func TestKilledTurnCarriesStoredToolEvents(t *testing.T) {
	partial, _, _ := strings.Cut(turnLines, `{"method":"item/completed","params":{"item":{"type":"fileChange"`)
	dir := setup(t, partial)
	continuationtest.KillHost(t, dir, "FADEN_CODEX_HOST")
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte(turnLines), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "got")); err != nil {
		t.Fatal(err)
	}
	continuationtest.Continue(t, dir, "codex", runnerBackend(t, dir))
	data, err := os.ReadFile(filepath.Join(dir, "got"))
	if err != nil {
		t.Fatal(err)
	}
	var prompt string
	for line := range strings.SplitSeq(string(data), "\n") {
		var m struct {
			Method string
			Params struct{ Input []struct{ Text string } }
		}
		if json.Unmarshal([]byte(line), &m) == nil && m.Method == "turn/start" && len(m.Params.Input) > 0 {
			prompt = m.Params.Input[0].Text
		}
	}
	end := strings.Index(prompt, "Continue where you left off.")
	for _, text := range []string{"cat greeting.txt", "hello"} {
		at := strings.Index(prompt, text)
		if at < 0 || end < 0 || at >= end {
			t.Fatalf("missing %q before continuation: %s", text, prompt)
		}
	}
}

func TestFailedToolResultIsMarked(t *testing.T) {
	cases := map[string]bool{
		`{"type":"commandExecution","command":"false","status":"completed","aggregatedOutput":"","exitCode":1}`: true,
		`{"type":"commandExecution","command":"true","status":"completed","aggregatedOutput":"","exitCode":0}`:  false,
		`{"type":"commandExecution","command":"rm x","status":"declined","aggregatedOutput":""}`:                true,
		`{"type":"fileChange","status":"failed","changes":[{"path":"a.txt"}]}`:                                  true,
		`{"type":"mcpToolCall","server":"s","tool":"t","result":null,"error":{"message":"invented failure"}}`:   true,
		`{"type":"mcpToolCall","server":"s","tool":"t","result":{"content":[]},"error":null}`:                   false,
	}
	for raw, want := range cases {
		var it item
		if err := json.Unmarshal([]byte(raw), &it); err != nil {
			t.Fatal(err)
		}
		var got []faden.Event
		if err := emitItem(it, func(e faden.Event) error { got = append(got, e); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Failed || got[1].Failed != want {
			t.Errorf("%s: events %+v, want result failed %v", raw, got, want)
		}
	}
}

func TestPrepareSetupReachesAppServerAndBothThreadRequests(t *testing.T) {
	dir := setup(t, turnLines)
	store, err := jsonl.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b, _ := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir}, Args: []string{"--own"}})
	runner, err := faden.Open(store, map[string]faden.Backend{"codex": b}, faden.Options{Prepare: func(context.Context, faden.Turn) (faden.Setup, error) {
		return faden.Setup{Dir: "/invented/ws", Args: []string{"--extra"}, Env: []string{"HOST_VAR=invented"}, System: "invented rules"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	for _, id := range []string{"r1", "r2"} {
		if _, err := runner.Submit(t.Context(), faden.Submission{Session: "s", RequestID: id, Input: "hi", Backend: "codex"}); err != nil {
			t.Fatal(err)
		}
		if got, err := runner.Wait(t.Context(), id); err != nil || got.Status != faden.Done {
			t.Fatalf("turn %s = %+v, %v", id, got, err)
		}
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	env, _ := os.ReadFile(filepath.Join(dir, "var"))
	if string(args) != "app-server\n--own\n--extra\n" || string(env) != "invented" {
		t.Fatalf("args %q env %q", args, env)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "got"))
	for _, method := range []string{"thread/start", "thread/resume"} {
		var line string
		for l := range strings.SplitSeq(string(got), "\n") {
			if strings.Contains(l, `"method":"`+method+`"`) {
				line = l
			}
		}
		for _, need := range []string{`"cwd":"/invented/ws"`, `"developerInstructions":"invented rules"`} {
			if !strings.Contains(line, need) {
				t.Errorf("%s request %q lacks %s", method, line, need)
			}
		}
	}
}

// interruptRuntime answers the handshake and records whether interruption precedes exit.
type interruptRuntime struct {
	ready       chan struct{}
	interrupted chan struct{}
}

func (r *interruptRuntime) Stop(context.Context, string) error { return nil }
func (r *interruptRuntime) Run(ctx context.Context, cmd faden.Command) (faden.Result, error) {
	scan := bufio.NewScanner(cmd.Stdin)
	for scan.Scan() {
		var m message
		if err := json.Unmarshal(scan.Bytes(), &m); err != nil {
			return faden.Result{}, err
		}
		switch m.Method {
		case "initialize":
			fmt.Fprintln(cmd.Stdout, `{"id":1,"result":{}}`)
		case "thread/start":
			fmt.Fprintln(cmd.Stdout, `{"id":2,"result":{"thread":{"id":"thread-cancel"}}}`)
		case "turn/start":
			fmt.Fprintln(cmd.Stdout, `{"id":3,"result":{"turn":{"id":"turn-cancel"}}}`)
			fmt.Fprintln(cmd.Stdout, `{"id":"ready","method":"item/commandExecution/requestApproval","params":{}}`)
		case "turn/interrupt":
			var p struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
			}
			if err := json.Unmarshal(m.Params, &p); err != nil || p.ThreadID != "thread-cancel" || p.TurnID != "turn-cancel" {
				return faden.Result{}, fmt.Errorf("bad interrupt: %s", m.Params)
			}
			close(r.interrupted)
			<-ctx.Done()
			return faden.Result{}, ctx.Err()
		default:
			if string(m.ID) == `"ready"` {
				close(r.ready)
			}
		}
	}
	return faden.Result{}, scan.Err()
}

func TestCancelInterruptsBeforeRuntimeEnds(t *testing.T) {
	runtime := &interruptRuntime{ready: make(chan struct{}), interrupted: make(chan struct{})}
	b, err := New(Config{Runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	turn := faden.Turn{Session: "s", RequestID: "cancel", Input: "wait"}
	a := newAttempt(t, store, turn)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, turn, a) }()
	select {
	case <-runtime.ready:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop runtime")
	}
	select {
	case <-runtime.interrupted:
	default:
		t.Fatal("runtime ended without turn/interrupt")
	}
}

func TestFilesSendImageAsLocalImageAndTextFileAsPathLine(t *testing.T) {
	dir := setup(t, turnLines)
	work := t.TempDir()
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==")
	os.WriteFile(filepath.Join(work, "shot.png"), png, 0600)
	os.WriteFile(filepath.Join(work, "notes.txt"), []byte("invented notes"), 0600)
	store, err := jsonl.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b, _ := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir}})
	runner, err := faden.Open(store, map[string]faden.Backend{"codex": b}, faden.Options{Prepare: func(context.Context, faden.Turn) (faden.Setup, error) {
		return faden.Setup{Dir: work}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	runner.Submit(t.Context(), faden.Submission{Session: "s", RequestID: "r1", Input: "look", Files: []string{"shot.png", "notes.txt"}, Backend: "codex"})
	if got, err := runner.Wait(t.Context(), "r1"); err != nil || got.Status != faden.Done {
		t.Fatalf("turn = %+v, %v", got, err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "got"))
	var line string
	for l := range strings.SplitSeq(string(got), "\n") {
		if strings.Contains(l, `"method":"turn/start"`) {
			line = l
		}
	}
	var req struct {
		Params struct {
			Input []map[string]string `json:"input"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		t.Fatalf("%v: %q", err, line)
	}
	in := req.Params.Input
	if len(in) != 2 || in[0]["type"] != "text" || in[0]["text"] != "look\nAttached file: notes.txt\n" || in[1]["type"] != "localImage" || in[1]["path"] != filepath.Join(work, "shot.png") {
		t.Fatalf("input = %+v", in)
	}
	if strings.Contains(line, "iVBOR") {
		t.Fatal("image bytes were sent")
	}
}
