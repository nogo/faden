package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
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

// checkpoint records what the fake codex had received when the cursor was saved.
type checkpoint struct {
	dir     string
	data    json.RawMessage
	gotThen string
	path    string
}

func (s *checkpoint) SetCursor(_ context.Context, _, _ string, data json.RawMessage) error {
	s.data = append(s.data[:0], data...)
	got, _ := os.ReadFile(filepath.Join(s.dir, "got"))
	s.gotThen = string(got)
	if s.path != "" {
		return os.WriteFile(s.path, data, 0600)
	}
	return nil
}

func gotMethods(t *testing.T, dir string) []string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "got"))
	var methods []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
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
	s := &checkpoint{dir: dir, path: filepath.Join(dir, "cursor")}
	b, err := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir}, Store: s, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return b, s
}

func TestTurnThenResumeWithoutPrompt(t *testing.T) {
	dir := setup(t, turnLines)
	b, store := newBackend(t, dir, "-c", `sandbox_mode="read-only"`)
	var events []faden.Event
	emit := func(e faden.Event) error { events = append(events, e); return nil }
	raw, err := b.Run(t.Context(), faden.Turn{Session: "s", Input: "Read greeting.txt", Model: "invented-model"}, nil, emit)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"thread_id":"thr_invented"}` {
		t.Fatalf("cursor: %s", raw)
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
		if !json.Valid(e.Native) {
			t.Fatalf("native: %q", e.Native)
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
	events = nil
	if _, err := b.Run(t.Context(), faden.Turn{Session: "s"}, raw, emit); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMethods(t, dir), []string{"initialize", "initialized", "thread/resume", "turn/start", ""}) {
		t.Fatalf("methods: %v", gotMethods(t, dir))
	}
	got, _ = os.ReadFile(filepath.Join(dir, "got"))
	if !strings.Contains(string(got), `"threadId":"thr_invented"`) || !strings.Contains(string(got), `"input":[]`) {
		t.Fatalf("resume lines:\n%s", got)
	}
	if !b.ContinuesWithoutPrompt() {
		t.Fatal("codex continues without a prompt")
	}
}

func TestHandoverPutsCatchUpBeforeRequest(t *testing.T) {
	dir := setup(t, turnLines)
	b, _ := newBackend(t, dir)
	catchUp := []faden.Event{{Seq: 1, Kind: faden.UserMessage, Text: "earlier question"}, {Seq: 2, Kind: faden.AssistantMessage, Text: "earlier answer"}}
	_, err := b.Run(t.Context(), faden.Turn{Session: "s", Input: "new request", CatchUp: catchUp}, nil, func(faden.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "got"))
	var turn struct {
		Params struct {
			Input []struct{ Text string }
		}
	}
	for _, line := range strings.Split(string(got), "\n") {
		if strings.Contains(line, `"turn/start"`) {
			json.Unmarshal([]byte(line), &turn)
		}
	}
	text := turn.Params.Input[0].Text
	want := handover.Block(handover.Select(catchUp, "new request", 0)) + "\nnew request"
	if text != want || !strings.Contains(text, "earlier answer") || !strings.HasSuffix(text, "new request") {
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
			b, _ := newBackend(t, dir)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if _, err := b.Run(ctx, faden.Turn{Session: "s", Input: "x"}, nil, func(faden.Event) error { return nil }); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestHostHelper(t *testing.T) {
	dir := os.Getenv("FADEN_CODEX_HOST")
	if dir == "" {
		return
	}
	s := &checkpoint{dir: dir, path: filepath.Join(dir, "cursor")}
	b, _ := New(Config{Store: s, Runtime: faden.Local{PIDDir: dir, Dir: dir}, Args: []string{"-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`}})
	input := "Write first.txt containing FIRST. Then run bash sleep 120. Then write second.txt and third.txt containing DONE, one after another. If interrupted and resumed, before using tools say FIRST_FILE_KNOWN if this conversation shows that first.txt was written, or FIRST_FILE_UNKNOWN otherwise."
	if _, err := b.Run(t.Context(), faden.Turn{Session: "s", Input: input}, nil, func(faden.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func waitFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waiting for %s", path)
	return nil
}

// TestLiveResumeAfterKill kills the host mid-turn, continues without a prompt,
// and logs whether Codex knew the work done before the kill.
func TestLiveResumeAfterKill(t *testing.T) {
	if os.Getenv("FADEN_LIVE") != "1" {
		t.Skip("set FADEN_LIVE=1 to run Codex")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHostHelper$")
	cmd.Env = append(os.Environ(), "FADEN_CODEX_HOST="+dir)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	waitFile(t, filepath.Join(dir, "first.txt"))
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	raw := waitFile(t, filepath.Join(dir, "cursor"))
	b, _ := newBackend(t, dir, "-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`)
	var answer strings.Builder
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	_, err := b.Run(ctx, faden.Turn{Session: "s"}, raw, func(e faden.Event) error {
		if e.Kind == faden.AssistantMessage {
			answer.WriteString(e.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	knowledge := "indeterminate"
	if strings.Contains(answer.String(), "FIRST_FILE_UNKNOWN") {
		knowledge = "unknown"
	} else if strings.Contains(answer.String(), "FIRST_FILE_KNOWN") {
		knowledge = "known"
	}
	t.Logf("Codex knowledge of first file: %s", knowledge)
	t.Logf("Codex resume after a kill; resumed assistant: %s", answer.String())
}
